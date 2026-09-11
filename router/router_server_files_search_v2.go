package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/gabriel-vasile/mimetype"
	"github.com/gin-gonic/gin"
	"golang.org/x/sys/unix"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/router/middleware"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

const (
	maxSearchV2Results       = 500
	maxSearchV2Patterns      = 128
	maxSearchV2PatternBytes  = 64 * 1024
	maxSearchV2Alternatives  = 64
	maxSearchV2Entries       = 250000
	maxSearchV2Directories   = 16384
	maxSearchV2Depth         = 256
	searchV2DirectoryBatch   = 128
	maxSearchV2RequestBytes  = 1024 * 1024
	defaultSearchV2ReadLimit = int64(2 * 1024 * 1024)
	maxSearchV2ReadLimit     = int64(1024 * 1024 * 1024)
	maxSearchV2TotalRead     = int64(1024 * 1024 * 1024)
	searchV2Timeout          = 30 * time.Second
	searchV2HeadBytes        = 128
)

type searchV2Pattern struct {
	glob          string
	directoryOnly bool
	negative      bool
}

type searchV2PathFilter struct {
	Include         []string `json:"include"`
	Exclude         []string `json:"exclude"`
	CaseInsensitive bool     `json:"case_insensitive"`
	includes        []searchV2Pattern
	excludes        []searchV2Pattern
}

type searchV2SizeFilter struct {
	Min *int64 `json:"min"`
	Max *int64 `json:"max"`
}

type searchV2ContentFilter struct {
	Query            string `json:"query"`
	MaxSearchSize    *int64 `json:"max_search_size"`
	IncludeUnmatched bool   `json:"include_unmatched"`
	CaseInsensitive  bool   `json:"case_insensitive"`
}

func (f searchV2ContentFilter) readLimit() int64 {
	if f.MaxSearchSize == nil {
		return defaultSearchV2ReadLimit
	}
	return *f.MaxSearchSize
}

type searchV2Payload struct {
	Root          string                 `json:"root"`
	PathFilter    *searchV2PathFilter    `json:"path_filter"`
	SizeFilter    *searchV2SizeFilter    `json:"size_filter"`
	ContentFilter *searchV2ContentFilter `json:"content_filter"`
	PerPage       int                    `json:"per_page"`
}

type searchV2Directory struct {
	file  ufs.File
	path  string
	depth int
	names []string
	index int
	eof   bool
}

type searchV2Budget struct {
	entriesVisited     int
	directoriesVisited int
	readRemaining      int64
}

type searchV2Matcher struct {
	pattern         []byte
	failure         []int
	caseInsensitive bool
}

var (
	errSearchV2TraversalLimit = errors.New("search traversal limit exceeded")
	errSearchV2ReadLimit      = errors.New("search total read limit exceeded")
	errSearchV2Root           = errors.New("invalid search root")
	searchV2BufferPool        = sync.Pool{New: func() interface{} {
		buffer := make([]byte, 64*1024)
		return &buffer
	}}
)

func postServerFilesSearch(c *gin.Context) {
	s := middleware.ExtractServer(c)
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxSearchV2RequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Search JSON exceeds 1 MiB."})
		} else {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Could not read search JSON."})
		}
		return
	}
	data := &searchV2Payload{PerPage: 100}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if !utf8.Valid(body) || decoder.Decode(&data) != nil || data == nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid search JSON, field type, or unsupported field. Only the documented V2 filters are accepted."})
		return
	}
	var extra interface{}
	if decoder.Decode(&extra) != io.EOF {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Search requires exactly one JSON object."})
		return
	}
	if err := validateSearchV2Payload(data); err != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	root, err := normalizeBetterFilesRoot(data.Root)
	if err != nil {
		abortBetterFilesPath(c, err)
		return
	}
	if err := ensureBetterFilesAllowed(s.Filesystem(), root); err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "The search root is forbidden."})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), searchV2Timeout)
	defer cancel()
	results, err := runSearchV2(ctx, s.Filesystem(), root, *data, nil)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			c.AbortWithStatusJSON(http.StatusRequestTimeout, gin.H{"error": "Search canceled or exceeded its 30-second time limit."})
		case errors.Is(err, errSearchV2TraversalLimit), errors.Is(err, errSearchV2ReadLimit):
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error() + "; narrow the search root or filters."})
		case errors.Is(err, errSearchV2Root):
			status := http.StatusUnprocessableEntity
			if errors.Is(err, ufs.ErrNotExist) {
				status = http.StatusNotFound
			}
			c.AbortWithStatusJSON(status, gin.H{"error": "Search root must be an existing directory without symbolic links."})
		default:
			middleware.CaptureAndAbort(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

func validateSearchV2Payload(data *searchV2Payload) error {
	if data.PerPage < 0 {
		return errors.New("per_page must be non-negative")
	}
	if data.PerPage > maxSearchV2Results {
		data.PerPage = maxSearchV2Results
	}
	if data.PathFilter != nil {
		filter := data.PathFilter
		if len(filter.Include)+len(filter.Exclude) > maxSearchV2Patterns {
			return errors.New("too many path patterns (maximum 128)")
		}
		total := 0
		for _, group := range [][]string{filter.Include, filter.Exclude} {
			for _, pattern := range group {
				total += len(pattern)
			}
		}
		if total > maxSearchV2PatternBytes {
			return errors.New("path patterns exceed 64 KiB in total")
		}
		var err error
		if filter.includes, err = compileSearchV2Patterns(filter.Include, filter.CaseInsensitive); err != nil {
			return err
		}
		if filter.excludes, err = compileSearchV2Patterns(filter.Exclude, filter.CaseInsensitive); err != nil {
			return err
		}
	}
	if data.SizeFilter != nil {
		f := data.SizeFilter
		if (f.Min != nil && *f.Min < 0) || (f.Max != nil && *f.Max < 0) {
			return errors.New("file sizes must be non-negative")
		}
		if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
			return errors.New("minimum file size exceeds maximum file size")
		}
	}
	if data.ContentFilter != nil {
		f := data.ContentFilter
		if !utf8.ValidString(f.Query) || len(f.Query) > betterFilesMaxPathLength {
			return errors.New("content query must be valid UTF-8 and at most 4096 bytes")
		}
		if limit := f.readLimit(); limit < 0 || limit > maxSearchV2ReadLimit {
			return errors.New("max_search_size must be between 0 and 1073741824 bytes")
		}
	}
	return nil
}

// Match the gitignore-style normalization used by Wings-rs's ignore crate.
// Globs are relative to the server filesystem root, independently of result names.
func compileSearchV2Patterns(patterns []string, insensitive bool) ([]searchV2Pattern, error) {
	result := make([]searchV2Pattern, 0, len(patterns))
	for _, pattern := range patterns {
		if !utf8.ValidString(pattern) || len(pattern) > betterFilesMaxPathLength || strings.ContainsAny(pattern, "\x00\r\n") {
			return nil, errors.New("invalid path pattern")
		}
		if strings.HasPrefix(pattern, "#") {
			continue
		}
		if !strings.HasSuffix(pattern, "\\ ") {
			pattern = strings.TrimRightFunc(pattern, unicode.IsSpace)
		}
		if pattern == "" {
			return nil, errors.New("path patterns must not be empty")
		}
		rule := searchV2Pattern{}
		if strings.HasPrefix(pattern, "\\!") || strings.HasPrefix(pattern, "\\#") {
			pattern = pattern[1:]
		} else if strings.HasPrefix(pattern, "!") {
			rule.negative = true
			pattern = pattern[1:]
		}
		anchored := strings.HasPrefix(pattern, "/")
		pattern = strings.TrimPrefix(pattern, "/")
		if strings.HasSuffix(pattern, "/") {
			rule.directoryOnly = true
			pattern = strings.TrimSuffix(pattern, "/")
			pattern = strings.TrimSuffix(pattern, "\\")
		}
		if pattern == "" || hasParentPathComponent(pattern) {
			return nil, errors.New("invalid path pattern")
		}
		// Bound doublestar's recursive brace expansion. Nested/empty alternatives
		// are deliberately rejected rather than changing the requested filter.
		product, alternatives, inClass := 1, 0, false
		for i := 0; i < len(pattern); i++ {
			if pattern[i] == '\\' {
				i++
				continue
			}
			if pattern[i] == '[' {
				inClass = true
			}
			if pattern[i] == ']' {
				inClass = false
				continue
			}
			if inClass {
				continue
			}
			switch pattern[i] {
			case '{':
				if alternatives != 0 {
					return nil, errors.New("nested glob alternatives are unsupported")
				}
				alternatives = 1
				if i+1 < len(pattern) && (pattern[i+1] == ',' || pattern[i+1] == '}') {
					return nil, errors.New("empty glob alternatives are unsupported")
				}
			case ',':
				if alternatives > 0 {
					alternatives++
					if i+1 < len(pattern) && (pattern[i+1] == ',' || pattern[i+1] == '}') {
						return nil, errors.New("empty glob alternatives are unsupported")
					}
				}
			case '/':
				if alternatives > 0 {
					return nil, errors.New("glob alternatives must stay within one path component")
				}
			case '}':
				if alternatives > 0 {
					if product > maxSearchV2Alternatives/alternatives {
						return nil, errors.New("glob has too many alternatives (maximum 64)")
					}
					product *= alternatives
					alternatives = 0
				}
			}
		}
		if !anchored && !strings.Contains(pattern, "/") {
			pattern = "**/" + pattern
		}
		if strings.HasSuffix(pattern, "/**") {
			pattern += "/*"
		}
		var err error
		rule.glob, err = compileSearchV2Glob(pattern, insensitive)
		if err != nil {
			return nil, err
		}
		result = append(result, rule)
	}
	return result, nil
}

func compileSearchV2Glob(pattern string, insensitive bool) (string, error) {
	pattern = searchV2GlobBytes(pattern, false)
	var out strings.Builder
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '\\' {
			out.WriteByte('\\')
			i++
			if i == len(pattern) {
				return "", errors.New("invalid path pattern escape")
			}
		} else if pattern[i] == '[' {
			start := i + 1
			negative := start < len(pattern) && (pattern[start] == '!' || pattern[start] == '^')
			if negative {
				start++
			}
			end := start
			if end < len(pattern) && pattern[end] == ']' {
				end++
			}
			for end < len(pattern) && pattern[end] != ']' {
				if pattern[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(pattern) {
				return "", errors.New("unclosed glob character class")
			}
			inner := pattern[start:end]
			if strings.HasPrefix(inner, "]") {
				inner = "\\" + inner
			}
			positive := "[" + inner + "]"
			if !doublestar.ValidatePattern(positive) {
				return "", errors.New("invalid glob character class")
			}
			out.WriteByte('[')
			if negative {
				out.WriteByte('!')
			}
			if insensitive {
				// Compile the original positive set before ASCII folding. Lowering
				// endpoints first changes ranges such as [A-z] and [Z-a].
				var members [256]bool
				for value := 0; value < len(members); value++ {
					if doublestar.MatchUnvalidated(positive, string(rune(value))) {
						members[searchV2LowerASCII(byte(value))] = true
					}
				}
				any := false
				for value, present := range members {
					if !present {
						continue
					}
					any = true
					if strings.ContainsRune("\\[]-!^", rune(value)) {
						out.WriteByte('\\')
					}
					out.WriteRune(rune(value))
				}
				if !any {
					return "", errors.New("empty glob character class")
				}
			} else {
				out.WriteString(inner)
			}
			out.WriteByte(']')
			i = end
			continue
		}
		value := pattern[i]
		if insensitive {
			value = searchV2LowerASCII(value)
		}
		out.WriteByte(value)
	}
	compiled := out.String()
	if !doublestar.ValidatePattern(compiled) {
		return "", errors.New("invalid path pattern")
	}
	return compiled, nil
}

// doublestar matches runes, while globset matches bytes. Mapping each byte to a
// rune preserves upstream '?' and character-class behavior for UTF-8 filenames.
func searchV2GlobBytes(value string, insensitive bool) string {
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		b := value[i]
		if insensitive {
			b = searchV2LowerASCII(b)
		}
		out.WriteRune(rune(b))
	}
	return out.String()
}

func searchV2PathDecision(serverPath string, directory bool, filter *searchV2PathFilter) (bool, bool) {
	if filter == nil {
		return true, false
	}
	candidate := searchV2GlobBytes(strings.TrimPrefix(serverPath, "/"), filter.CaseInsensitive)
	matchRules := func(rules []searchV2Pattern) bool {
		selected := false
		for _, rule := range rules {
			if (!rule.directoryOnly || directory) && doublestar.MatchUnvalidated(rule.glob, candidate) {
				selected = !rule.negative
			}
		}
		return selected
	}
	excluded := matchRules(filter.excludes)
	return !excluded && (len(filter.Include) == 0 || matchRules(filter.includes)), excluded
}

func openSearchV2Root(ctx context.Context, fs *serverfs.Filesystem, root string) (ufs.File, error) {
	current, err := fs.UnixFS().OpenFile(".", ufs.O_RDONLY|ufs.O_DIRECTORY|ufs.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		if name == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			_ = current.Close()
			return nil, err
		}
		child, err := fs.UnixFS().OpenFileat(int(current.Fd()), name, ufs.O_RDONLY|ufs.O_DIRECTORY|ufs.O_NOFOLLOW, 0)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = child
	}
	return current, nil
}

func runSearchV2(ctx context.Context, fs *serverfs.Filesystem, root string, data searchV2Payload, budget *searchV2Budget) ([]betterFilesEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rootFile, err := openSearchV2Root(ctx, fs, root)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSearchV2Root, err)
	}
	stack := []searchV2Directory{{file: rootFile, path: root}}
	defer func() {
		for _, frame := range stack {
			_ = frame.file.Close()
		}
	}()
	results := make([]betterFilesEntry, 0, data.PerPage)
	if budget == nil {
		budget = &searchV2Budget{readRemaining: maxSearchV2TotalRead}
	}
	budget.directoriesVisited++
	var matcher *searchV2Matcher
	if data.ContentFilter != nil {
		matcher = newSearchV2Matcher(data.ContentFilter.Query, data.ContentFilter.CaseInsensitive)
	}
	for len(stack) > 0 && len(results) < data.PerPage {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if budget.directoriesVisited > maxSearchV2Directories {
			return nil, errSearchV2TraversalLimit
		}
		frame := &stack[len(stack)-1]
		if frame.index == len(frame.names) {
			if frame.eof {
				_ = frame.file.Close()
				stack = stack[:len(stack)-1]
				continue
			}
			frame.names, err = frame.file.Readdirnames(searchV2DirectoryBatch)
			frame.index = 0
			frame.eof = errors.Is(err, io.EOF)
			if err != nil && !frame.eof {
				return nil, err
			}
			if len(frame.names) == 0 {
				continue
			}
		}
		name := frame.names[frame.index]
		frame.index++
		budget.entriesVisited++
		if budget.entriesVisited > maxSearchV2Entries {
			return nil, errSearchV2TraversalLimit
		}
		fullPath, err := joinBetterFilesPath(frame.path, name)
		if err != nil || ensureBetterFilesAllowed(fs, fullPath) != nil {
			continue
		}
		info, err := fs.UnixFS().Lstatat(int(frame.file.Fd()), name)
		if errors.Is(err, ufs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&ufs.ModeSymlink != 0 {
			continue
		}
		included, excluded := searchV2PathDecision(fullPath, info.IsDir(), data.PathFilter)
		if info.IsDir() {
			if excluded {
				continue
			}
			if frame.depth >= maxSearchV2Depth {
				return nil, errSearchV2TraversalLimit
			}
			child, err := fs.UnixFS().OpenFileat(int(frame.file.Fd()), name, ufs.O_RDONLY|ufs.O_DIRECTORY|ufs.O_NOFOLLOW, 0)
			if errors.Is(err, ufs.ErrNotExist) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				continue
			}
			if err != nil {
				return nil, err
			}
			budget.directoriesVisited++
			stack = append(stack, searchV2Directory{file: child, path: fullPath, depth: frame.depth + 1})
			continue
		}
		if !included || !info.Mode().IsRegular() {
			continue
		}
		entry, matched, err := searchV2File(ctx, fs, int(frame.file.Fd()), name, betterFilesRelativePath(root, fullPath), data, matcher, budget)
		if err != nil {
			return nil, err
		}
		if matched {
			results = append(results, entry)
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

func searchV2SizeMatches(size int64, filter *searchV2SizeFilter) bool {
	if filter == nil {
		return true
	}
	if filter.Min != nil && size < *filter.Min {
		return false
	}
	return filter.Max == nil || size < *filter.Max
}

type searchV2Reader struct {
	ctx    context.Context
	reader io.Reader
	budget *searchV2Budget
}

func (r *searchV2Reader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.budget.readRemaining <= 0 {
		return 0, errSearchV2ReadLimit
	}
	if int64(len(p)) > r.budget.readRemaining {
		p = p[:r.budget.readRemaining]
	}
	n, err := r.reader.Read(p)
	r.budget.readRemaining -= int64(n)
	return n, err
}

func searchV2File(ctx context.Context, fs *serverfs.Filesystem, parent int, name, relative string, data searchV2Payload, matcher *searchV2Matcher, budget *searchV2Budget) (betterFilesEntry, bool, error) {
	var empty betterFilesEntry
	// A concurrently replaced regular file must not turn this open into a FIFO wait.
	file, err := fs.UnixFS().OpenFileat(parent, name, ufs.O_RDONLY|ufs.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		// Like Wings-rs, skip files that disappeared or cannot be opened.
		return empty, false, ctx.Err()
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return empty, false, ctx.Err()
	}
	if !info.Mode().IsRegular() || !searchV2SizeMatches(info.Size(), data.SizeFilter) {
		return empty, false, nil
	}
	filter := data.ContentFilter
	scanned := filter != nil && info.Size() <= filter.readLimit()
	if filter != nil && !scanned && !filter.IncludeUnmatched {
		return empty, false, nil
	}
	limit := int64(searchV2HeadBytes)
	if scanned {
		limit = filter.readLimit()
	}
	reader := io.LimitReader(&searchV2Reader{ctx: ctx, reader: file, budget: budget}, limit)
	var head [searchV2HeadBytes]byte
	n, readErr := io.ReadFull(reader, head[:])
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return empty, false, readErr
	}
	if scanned {
		if !validSearchV2TextHead(head[:n]) {
			return empty, false, nil
		}
		matched, err := streamSearchV2Text(ctx, io.MultiReader(bytes.NewReader(head[:n]), reader), limit, matcher)
		if err != nil {
			return empty, false, err
		}
		if !matched {
			return empty, false, nil
		}
	}
	mime := mimetype.Detect(head[:n]).String()
	return newBetterFilesEntry(relative, info, mime), true, nil
}

// Wings-rs uses a 128-byte UTF-8 head heuristic, not MIME or NUL-byte filtering.
// An incomplete trailing rune in that head is allowed, as in is_valid_utf8_slice.
func validSearchV2TextHead(head []byte) bool {
	for len(head) > 0 {
		if !utf8.FullRune(head) {
			return true
		}
		value, size := utf8.DecodeRune(head)
		if value == utf8.RuneError && size == 1 {
			return false
		}
		head = head[size:]
	}
	return true
}

func searchV2LowerASCII(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + 'a' - 'A'
	}
	return value
}

func newSearchV2Matcher(query string, caseInsensitive bool) *searchV2Matcher {
	pattern := []byte(query)
	if caseInsensitive {
		for i := range pattern {
			pattern[i] = searchV2LowerASCII(pattern[i])
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
	return &searchV2Matcher{pattern: pattern, failure: failure, caseInsensitive: caseInsensitive}
}

// The KMP state spans read boundaries. Byte matching preserves UTF-8 literals
// and upstream ASCII-only case folding without allocating whole file contents.
func streamSearchV2Text(ctx context.Context, reader io.Reader, limit int64, matcher *searchV2Matcher) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if len(matcher.pattern) == 0 {
		return true, nil
	}
	buffer := searchV2BufferPool.Get().(*[]byte)
	defer searchV2BufferPool.Put(buffer)
	reader = io.LimitReader(reader, limit)
	state := 0
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, readErr := reader.Read(*buffer)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		for _, value := range (*buffer)[:n] {
			if matcher.caseInsensitive {
				value = searchV2LowerASCII(value)
			}
			for state > 0 && value != matcher.pattern[state] {
				state = matcher.failure[state-1]
			}
			if value == matcher.pattern[state] {
				state++
			}
			if state == len(matcher.pattern) {
				return true, nil
			}
		}
		if readErr == io.EOF {
			return false, nil
		}
		if readErr != nil {
			return false, readErr
		}
		if n == 0 {
			return false, io.ErrNoProgress
		}
	}
}
