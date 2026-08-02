package router

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	pathpkg "path"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/gabriel-vasile/mimetype"
	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/router/middleware"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

const (
	maxSearchV2Results       = 500
	maxSearchV2Patterns      = 128
	maxSearchV2Entries       = 250000
	maxSearchV2Directories   = 16384
	maxSearchV2Depth         = 256
	defaultSearchV2ReadLimit = int64(2 * 1024 * 1024)
	maxSearchV2ReadLimit     = int64(64 * 1024 * 1024)
)

type searchV2PathFilter struct {
	Include         []string `json:"include"`
	Exclude         []string `json:"exclude"`
	CaseInsensitive bool     `json:"case_insensitive"`
}

type searchV2SizeFilter struct {
	Min *int64 `json:"min"`
	Max *int64 `json:"max"`
}

type searchV2ContentFilter struct {
	Query            string `json:"query"`
	MaxSearchSize    int64  `json:"max_search_size"`
	IncludeUnmatched bool   `json:"include_unmatched"`
	CaseInsensitive  bool   `json:"case_insensitive"`
}

type searchV2Payload struct {
	Root          string                 `json:"root"`
	PathFilter    *searchV2PathFilter    `json:"path_filter"`
	SizeFilter    *searchV2SizeFilter    `json:"size_filter"`
	ContentFilter *searchV2ContentFilter `json:"content_filter"`
	PerPage       int                    `json:"per_page"`
}

type searchV2Directory struct {
	path  string
	depth int
}

type searchV2RuneMatcher struct {
	pattern         []rune
	failure         []int
	caseInsensitive bool
}

var searchV2BufferPool = sync.Pool{New: func() interface{} {
	buffer := make([]byte, 64*1024)
	return &buffer
}}

func postServerFilesSearch(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var data searchV2Payload
	if err := c.ShouldBindJSON(&data); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid search request."})
		return
	}
	if err := validateSearchV2Payload(&data); err != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	root, err := normalizeBetterFilesRoot(data.Root)
	if err != nil || ensureBetterFilesAllowed(s.Filesystem(), root) != nil {
		abortBetterFilesPath(c, errBetterFilesInvalidPath)
		return
	}
	rootInfo, err := betterFilesLstat(s.Filesystem(), root)
	if err != nil || !rootInfo.IsDir() {
		abortBetterFilesPath(c, err)
		return
	}

	results, err := runSearchV2(c.Request.Context(), s.Filesystem(), root, data)
	if err != nil {
		if errors.Is(err, errSearchV2TraversalLimit) {
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "Search traversal limit exceeded."})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

var errSearchV2TraversalLimit = errors.New("search traversal limit exceeded")

func validateSearchV2Payload(data *searchV2Payload) error {
	if data.PerPage <= 0 {
		data.PerPage = 100
	}
	if data.PerPage > maxSearchV2Results {
		data.PerPage = maxSearchV2Results
	}
	if data.PathFilter != nil {
		if len(data.PathFilter.Include)+len(data.PathFilter.Exclude) > maxSearchV2Patterns {
			return errors.New("too many path patterns")
		}
		normalizePatterns := func(patterns []string) error {
			for i, pattern := range patterns {
				if pattern == "" || len(pattern) > betterFilesMaxPathLength || strings.ContainsAny(pattern, "\x00\\") || hasParentPathComponent(pattern) {
					return errors.New("invalid path pattern")
				}
				pattern = strings.TrimLeft(pattern, "/")
				if !doublestar.ValidatePattern(pattern) {
					return errors.New("invalid path pattern")
				}
				if data.PathFilter.CaseInsensitive {
					pattern = strings.ToLower(pattern)
				}
				patterns[i] = pattern
			}
			return nil
		}
		if err := normalizePatterns(data.PathFilter.Include); err != nil {
			return err
		}
		if err := normalizePatterns(data.PathFilter.Exclude); err != nil {
			return err
		}
	}
	if data.SizeFilter != nil {
		if data.SizeFilter.Min != nil && *data.SizeFilter.Min < 0 || data.SizeFilter.Max != nil && *data.SizeFilter.Max < 0 {
			return errors.New("file sizes must be non-negative")
		}
		if data.SizeFilter.Min != nil && data.SizeFilter.Max != nil && *data.SizeFilter.Min > *data.SizeFilter.Max {
			return errors.New("minimum file size exceeds maximum file size")
		}
	}
	if data.ContentFilter != nil {
		if !utf8.ValidString(data.ContentFilter.Query) || len(data.ContentFilter.Query) > betterFilesMaxPathLength || strings.ContainsRune(data.ContentFilter.Query, '\x00') {
			return errors.New("invalid content query")
		}
		if data.ContentFilter.MaxSearchSize == 0 {
			data.ContentFilter.MaxSearchSize = defaultSearchV2ReadLimit
		}
		if data.ContentFilter.MaxSearchSize < 0 || data.ContentFilter.MaxSearchSize > maxSearchV2ReadLimit {
			return errors.New("max_search_size must be between 0 and 67108864")
		}
	}
	return nil
}

func runSearchV2(ctx context.Context, fs *serverfs.Filesystem, root string, data searchV2Payload) ([]betterFilesEntry, error) {
	results := make([]betterFilesEntry, 0, data.PerPage)
	stack := []searchV2Directory{{path: root}}
	entriesVisited := 0
	directoriesVisited := 0
	var contentMatcher *searchV2RuneMatcher
	if data.ContentFilter != nil {
		contentMatcher = newSearchV2RuneMatcher(data.ContentFilter.Query, data.ContentFilter.CaseInsensitive)
	}

	for len(stack) > 0 && len(results) < data.PerPage {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		directoriesVisited++
		if directoriesVisited > maxSearchV2Directories || directory.depth > maxSearchV2Depth {
			return nil, errSearchV2TraversalLimit
		}

		entries, err := fs.ReadDirStat(directory.path)
		if err != nil {
			return nil, err
		}
		for i := len(entries) - 1; i >= 0 && len(results) < data.PerPage; i-- {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			info := entries[i]
			entriesVisited++
			if entriesVisited > maxSearchV2Entries {
				return nil, errSearchV2TraversalLimit
			}
			fullPath := pathpkg.Join(directory.path, info.Name())
			if ensureBetterFilesAllowed(fs, fullPath) != nil || info.Mode()&ufs.ModeSymlink != 0 {
				continue
			}
			relative := betterFilesRelativePath(root, fullPath)
			pathMatches, pathExcluded := searchV2PathDecision(relative, data.PathFilter)

			if info.IsDir() {
				if pathExcluded {
					continue
				}
				if directory.depth < maxSearchV2Depth {
					stack = append(stack, searchV2Directory{path: fullPath, depth: directory.depth + 1})
				}
				if data.ContentFilter != nil || !pathMatches || !searchV2SizeMatches(info.Size(), data.SizeFilter) {
					continue
				}
				results = append(results, newBetterFilesEntry(relative, info, "inode/directory"))
				continue
			}
			if !info.Mode().IsRegular() || !pathMatches || !searchV2SizeMatches(info.Size(), data.SizeFilter) {
				continue
			}

			mime := ""
			if data.ContentFilter != nil {
				matched, detectedMime := searchV2ContentMatches(ctx, fs, fullPath, info, *data.ContentFilter, contentMatcher)
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if !matched {
					continue
				}
				mime = detectedMime
			}
			if mime == "" {
				mime = betterFilesMime(fs, fullPath, info)
			}
			results = append(results, newBetterFilesEntry(relative, info, mime))
		}
	}
	return results, nil
}

func betterFilesRelativePath(root, fullPath string) string {
	if root == "/" {
		return strings.TrimPrefix(fullPath, "/")
	}
	return strings.TrimPrefix(strings.TrimPrefix(fullPath, root), "/")
}

func searchV2PathDecision(relative string, filter *searchV2PathFilter) (bool, bool) {
	if filter == nil {
		return true, false
	}
	candidate := relative
	if filter.CaseInsensitive {
		candidate = strings.ToLower(candidate)
	}
	matchAny := func(patterns []string) bool {
		for _, pattern := range patterns {
			if doublestar.MatchUnvalidated(pattern, candidate) {
				return true
			}
		}
		return false
	}
	if matchAny(filter.Exclude) {
		return false, true
	}
	if len(filter.Include) > 0 && !matchAny(filter.Include) {
		return false, false
	}
	return true, false
}

func searchV2SizeMatches(size int64, filter *searchV2SizeFilter) bool {
	if filter == nil {
		return true
	}
	if filter.Min != nil && size < *filter.Min {
		return false
	}
	return filter.Max == nil || size <= *filter.Max
}

func searchV2ContentMatches(
	ctx context.Context,
	fs *serverfs.Filesystem,
	filePath string,
	info ufs.FileInfo,
	filter searchV2ContentFilter,
	matcher *searchV2RuneMatcher,
) (bool, string) {
	if info.Size() > filter.MaxSearchSize {
		return filter.IncludeUnmatched, betterFilesMime(fs, filePath, info)
	}
	file, err := fs.UnixFS().OpenFile(filePath, ufs.O_RDONLY|ufs.O_NOFOLLOW, 0)
	if err != nil {
		return false, ""
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() > filter.MaxSearchSize {
		return false, ""
	}
	detectedMime, err := mimetype.DetectReader(file)
	if err != nil || detectedMime == nil {
		return false, ""
	}
	detected := detectedMime.String()
	if !betterFilesTextMime(detected) {
		return false, ""
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, ""
	}
	if !streamSearchV2Text(ctx, io.LimitReader(file, filter.MaxSearchSize+1), filter.MaxSearchSize, matcher) {
		return false, ""
	}
	return true, detected
}

func newSearchV2RuneMatcher(query string, caseInsensitive bool) *searchV2RuneMatcher {
	pattern := []rune(query)
	if caseInsensitive {
		for i := range pattern {
			pattern[i] = unicode.ToLower(pattern[i])
		}
	}
	failure := make([]int, len(pattern))
	for i, matched := 1, 0; i < len(pattern); i++ {
		for matched > 0 && pattern[i] != pattern[matched] {
			matched = failure[matched-1]
		}
		if pattern[i] == pattern[matched] {
			matched++
		}
		failure[i] = matched
	}
	return &searchV2RuneMatcher{pattern: pattern, failure: failure, caseInsensitive: caseInsensitive}
}

func (m *searchV2RuneMatcher) consume(value rune, state *int) bool {
	if len(m.pattern) == 0 {
		return true
	}
	if m.caseInsensitive {
		value = unicode.ToLower(value)
	}
	for *state > 0 && value != m.pattern[*state] {
		*state = m.failure[*state-1]
	}
	if value == m.pattern[*state] {
		*state++
	}
	return *state == len(m.pattern)
}

// streamSearchV2Text validates UTF-8 and searches incrementally, keeping only
// one fixed-size buffer even when max_search_size is tens of megabytes.
func streamSearchV2Text(ctx context.Context, reader io.Reader, limit int64, matcher *searchV2RuneMatcher) bool {
	buffer := searchV2BufferPool.Get().(*[]byte)
	defer searchV2BufferPool.Put(buffer)

	matched := len(matcher.pattern) == 0
	matchState := 0
	var carry [utf8.UTFMax]byte
	carryLength := 0
	var total int64

	consume := func(value rune) bool {
		if value == 0 {
			return false
		}
		if !matched && matcher.consume(value, &matchState) {
			matched = true
		}
		return true
	}

	for {
		if err := ctx.Err(); err != nil {
			return false
		}
		n, readErr := reader.Read(*buffer)
		if n > 0 {
			total += int64(n)
			if total > limit || bytes.IndexByte((*buffer)[:n], 0) >= 0 {
				return false
			}
			data := (*buffer)[:n]

			if carryLength > 0 {
				for len(data) > 0 && !utf8.FullRune(carry[:carryLength]) {
					carry[carryLength] = data[0]
					carryLength++
					data = data[1:]
				}
				if utf8.FullRune(carry[:carryLength]) {
					value, size := utf8.DecodeRune(carry[:carryLength])
					if (size == 1 && value == utf8.RuneError) || size != carryLength || !consume(value) {
						return false
					}
					carryLength = 0
				}
			}

			for len(data) > 0 {
				if !utf8.FullRune(data) {
					carryLength = copy(carry[:], data)
					break
				}
				value, size := utf8.DecodeRune(data)
				if size == 1 && value == utf8.RuneError || !consume(value) {
					return false
				}
				data = data[size:]
			}
		}

		if readErr == io.EOF {
			return carryLength == 0 && matched
		}
		if readErr != nil || n == 0 {
			return false
		}
	}
}

func betterFilesTextMime(value string) bool {
	mediaType := strings.ToLower(strings.SplitN(value, ";", 2)[0])
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/ld+json", "application/xml", "application/javascript", "application/x-httpd-php", "application/toml", "application/x-yaml":
		return true
	default:
		return strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")
	}
}
