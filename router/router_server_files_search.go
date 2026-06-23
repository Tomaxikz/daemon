package router

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/router/middleware"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

type FileSearchResult struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Directory  string `json:"directory"`
	IsFile     bool   `json:"is_file"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
}

type FileSearchMeta struct {
	SearchTimeMs int64  `json:"search_time_ms"`
	Truncated    bool   `json:"truncated"`
	Query        string `json:"query"`
	MaxResults   int    `json:"max_results"`
	MaxDepth     int    `json:"max_depth"`
	TotalScanned int64  `json:"total_scanned"`
}

type FileSearchResponse struct {
	Data []FileSearchResult `json:"data"`
	Meta FileSearchMeta     `json:"meta"`
}

var (
	sensitiveExtensions = map[string]struct{}{
		".pem": {},
		".key": {},
	}

	contentAllowedExtensions = map[string]struct{}{
		".txt":        {},
		".log":        {},
		".json":       {},
		".jsonc":      {},
		".yaml":       {},
		".yml":        {},
		".toml":       {},
		".ini":        {},
		".conf":       {},
		".cfg":        {},
		".properties": {},
		".env":        {},
		".md":         {},
		".xml":        {},
		".html":       {},
		".htm":        {},
		".css":        {},
		".scss":       {},
		".sass":       {},
		".less":       {},
		".js":         {},
		".jsx":        {},
		".ts":         {},
		".tsx":        {},
		".php":        {},
		".go":         {},
		".java":       {},
		".cs":         {},
		".py":         {},
		".rb":         {},
		".sh":         {},
	}

	sensitivePrefixes = []string{
		".env",
		".htaccess",
		".htpasswd",
		"id_rsa",
		"id_dsa",
		"id_ecdsa",
		"id_ed25519",
	}

	resultPool = sync.Pool{
		New: func() any {
			batch := make([]FileSearchResult, 0, batchSize)
			return &batch
		},
	}

	builderPool = sync.Pool{
		New: func() any {
			return &strings.Builder{}
		},
	}
)

const (
	maxQueryLength  = 256
	minQueryLength  = 1
	maxPathLength   = 4096
	maxFilenameLen  = 255
	jobChannelSize  = 2048
	resultChanSize  = 64
	batchSize       = 16
	maxContentBytes = 1024 * 1024
)

type walkJob struct {
	path  string
	depth int
}

type searchContext struct {
	fs            *serverfs.Filesystem
	queryLower    string
	maxResults    int
	maxDepth      int
	contentSearch bool
	results       []FileSearchResult
	resultCount   atomic.Int64
	truncated     atomic.Bool
	totalScanned  atomic.Int64
	done          chan struct{}
	jobChan       chan walkJob
	resultChan    chan []FileSearchResult
	pendingJobs   atomic.Int64
	stopOnce      sync.Once
	closeOnce     sync.Once
}

func getWorkerCount() int {
	n := runtime.NumCPU()
	if n > 8 {
		return 8
	}
	if n < 2 {
		return 2
	}
	return n
}

func sanitizeQuery(query string) (string, bool) {
	query = strings.TrimSpace(query)

	if len(query) < minQueryLength || len(query) > maxQueryLength {
		return "", false
	}

	if !utf8.ValidString(query) {
		return "", false
	}

	if strings.Contains(query, "..") || strings.ContainsAny(query, "\x00\n\r") {
		return "", false
	}

	return query, true
}

func isSensitiveFile(name string) bool {
	nameLower := strings.ToLower(name)

	for _, prefix := range sensitivePrefixes {
		if strings.HasPrefix(nameLower, prefix) {
			return true
		}
	}

	if idx := strings.LastIndexByte(name, '.'); idx != -1 {
		ext := strings.ToLower(name[idx:])
		if _, ok := sensitiveExtensions[ext]; ok {
			return true
		}
	}

	return false
}

func allowsContentSearch(name string) bool {
	nameLower := strings.ToLower(name)

	if strings.HasPrefix(nameLower, ".") {
		return false
	}

	if idx := strings.LastIndexByte(nameLower, '.'); idx != -1 {
		if _, ok := contentAllowedExtensions[nameLower[idx:]]; ok {
			return true
		}
	}

	return false
}

func contentMatches(fs *serverfs.Filesystem, displayPath string, queryLower string, size int64) bool {
	if size <= 0 || size > maxContentBytes {
		return false
	}

	file, stat, err := fs.File(displayPath)
	if err != nil {
		return false
	}
	defer file.Close()
	if stat.IsDir() || !stat.Mode().IsRegular() || stat.Size() <= 0 || stat.Size() > maxContentBytes {
		return false
	}

	limited := io.LimitReader(file, maxContentBytes)
	data, err := io.ReadAll(limited)
	if err != nil {
		return false
	}

	if bytes.IndexByte(data, 0) != -1 {
		return false
	}

	if !utf8.Valid(data) {
		return false
	}

	contentLower := strings.ToLower(string(data))
	return strings.Contains(contentLower, queryLower)
}

func (sc *searchContext) checkDone() bool {
	select {
	case <-sc.done:
		return true
	default:
		return false
	}
}

func (sc *searchContext) stop() {
	sc.stopOnce.Do(func() {
		close(sc.done)
	})
}

func (sc *searchContext) hasEnoughResults() bool {
	if sc.maxResults <= 0 {
		return false
	}
	return int(sc.resultCount.Load()) >= sc.maxResults
}

func getSearchResultBatch() []FileSearchResult {
	batch := resultPool.Get().(*[]FileSearchResult)
	return (*batch)[:0]
}

func putSearchResultBatch(batch []FileSearchResult) {
	if batch == nil {
		return
	}
	if cap(batch) > batchSize*4 {
		return
	}
	batch = batch[:0]
	resultPool.Put(&batch)
}

func buildSearchPath(dir string, name string) string {
	builder := builderPool.Get().(*strings.Builder)
	builder.Reset()
	defer builderPool.Put(builder)

	if dir == "" || dir == "/" {
		builder.Grow(len(name) + 1)
		builder.WriteByte('/')
		builder.WriteString(name)
		return builder.String()
	}

	builder.Grow(len(dir) + len(name) + 1)
	builder.WriteString(dir)
	builder.WriteByte('/')
	builder.WriteString(name)
	return builder.String()
}

func (sc *searchContext) submitResults(batch []FileSearchResult) bool {
	if len(batch) == 0 {
		return false
	}
	sc.resultCount.Add(int64(len(batch)))

	select {
	case sc.resultChan <- batch:
		return sc.hasEnoughResults()
	case <-sc.done:
		putSearchResultBatch(batch)
		return true
	}
}

func (sc *searchContext) collectResults() {
	for batch := range sc.resultChan {
		if sc.maxResults <= 0 {
			sc.results = append(sc.results, batch...)
			putSearchResultBatch(batch)
			continue
		}

		remaining := sc.maxResults - len(sc.results)
		if remaining <= 0 {
			sc.truncated.Store(true)
			putSearchResultBatch(batch)
			continue
		}

		if len(batch) > remaining {
			sc.results = append(sc.results, batch[:remaining]...)
			sc.truncated.Store(true)
		} else {
			sc.results = append(sc.results, batch...)
		}
		putSearchResultBatch(batch)
	}
}

func (sc *searchContext) enqueueJob(job walkJob) {
	if sc.checkDone() || sc.hasEnoughResults() {
		return
	}

	sc.pendingJobs.Add(1)

	defer func() {
		if r := recover(); r != nil {
			sc.pendingJobs.Add(-1)
		}
	}()

	select {
	case sc.jobChan <- job:
	case <-sc.done:
		sc.pendingJobs.Add(-1)
	}
}

func (sc *searchContext) finishJob() {
	if sc.pendingJobs.Add(-1) == 0 {
		sc.closeOnce.Do(func() {
			close(sc.jobChan)
		})
	}
}

func getDirectory(path string) string {
	lastSlash := strings.LastIndexByte(path, '/')
	if lastSlash <= 0 {
		return "/"
	}
	return path[:lastSlash]
}

func (sc *searchContext) worker(wg *sync.WaitGroup) {
	defer wg.Done()

	localBatch := getSearchResultBatch()
	defer func() {
		if localBatch != nil && len(localBatch) == 0 {
			putSearchResultBatch(localBatch)
		}
	}()

	for {
		select {
		case <-sc.done:
			if len(localBatch) > 0 {
				sc.submitResults(localBatch)
				localBatch = nil
			}
			return
		case job, ok := <-sc.jobChan:
			if !ok {
				if len(localBatch) > 0 {
					sc.submitResults(localBatch)
					localBatch = nil
				}
				return
			}

			if sc.checkDone() || sc.hasEnoughResults() {
				sc.finishJob()
				continue
			}

			entries, err := sc.fs.ReadDirStat(job.path)
			if err != nil {
				sc.finishJob()
				continue
			}

			var subDirs []walkJob

			for _, entry := range entries {
				if sc.checkDone() {
					sc.truncated.Store(true)
					break
				}

				name := entry.Name()
				nameLen := len(name)

				if nameLen == 0 || nameLen > maxFilenameLen {
					continue
				}

				isDir := entry.IsDir()

				sc.totalScanned.Add(1)

				nameLower := strings.ToLower(name)
				nameMatches := strings.Contains(nameLower, sc.queryLower)

				if nameMatches || (sc.contentSearch && !isDir && allowsContentSearch(name)) {
					if !isDir && isSensitiveFile(name) {
						continue
					}

					fullPath := buildSearchPath(job.path, name)

					if len(fullPath) > maxPathLength {
						continue
					}

					if !nameMatches {
						if !sc.contentSearch || isDir || !entry.Mode().IsRegular() || !contentMatches(sc.fs, fullPath, sc.queryLower, entry.Size()) {
							goto scanSubdirs
						}
					}

					result := FileSearchResult{
						Name:       name,
						Path:       fullPath,
						Directory:  getDirectory(fullPath),
						IsFile:     !isDir,
						Size:       entry.Size(),
						ModifiedAt: entry.ModTime().UTC().Format(time.RFC3339),
					}

					localBatch = append(localBatch, result)

					if len(localBatch) >= batchSize {
						if sc.submitResults(localBatch) {
							localBatch = getSearchResultBatch()
							sc.stop()
							break
						}
						localBatch = getSearchResultBatch()
					}
				}

			scanSubdirs:

				if isDir && (sc.maxDepth <= 0 || job.depth < sc.maxDepth) {
					subDirs = append(subDirs, walkJob{
						path:  path.Join(job.path, name),
						depth: job.depth + 1,
					})
				}
			}

			for _, subDir := range subDirs {
				if sc.checkDone() || sc.hasEnoughResults() {
					break
				}
				sc.enqueueJob(subDir)
			}

			if len(localBatch) > 0 {
				if sc.submitResults(localBatch) {
					sc.stop()
				}
				localBatch = getSearchResultBatch()
			}

			sc.finishJob()
		}
	}
}

func getServerFilesSearch(c *gin.Context) {
	s := middleware.ExtractServer(c)

	rawQuery := c.Query("query")
	query, valid := sanitizeQuery(rawQuery)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or missing query parameter"})
		return
	}

	maxResults := 50
	if v := c.Query("max_results"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			if parsed == 0 {
				maxResults = 0
			} else if parsed > 0 && parsed <= 200 {
				maxResults = parsed
			}
		}
	} else {
		maxResults = 0
	}

	maxDepth := 0
	if v := c.Query("max_depth"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 {
			maxDepth = parsed
		}
	}

	timeout := 8
	if v := c.Query("timeout"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 30 {
			timeout = parsed
		}
	}

	contentSearch := false
	if v := strings.TrimSpace(c.Query("content")); v != "" {
		if v == "1" || strings.EqualFold(v, "true") {
			contentSearch = true
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeout)*time.Second)
	defer cancel()

	resultsCap := maxResults
	if resultsCap <= 0 {
		resultsCap = 64
	}

	sc := &searchContext{
		fs:            s.Filesystem(),
		queryLower:    strings.ToLower(query),
		maxResults:    maxResults,
		maxDepth:      maxDepth,
		contentSearch: contentSearch,
		results:       make([]FileSearchResult, 0, resultsCap),
		done:          make(chan struct{}),
		jobChan:       make(chan walkJob, jobChannelSize),
		resultChan:    make(chan []FileSearchResult, resultChanSize),
	}

	go func() {
		<-ctx.Done()
		sc.stop()
	}()

	startTime := time.Now()

	workerCount := getWorkerCount()
	var wg sync.WaitGroup
	var collector sync.WaitGroup

	collector.Add(1)
	go func() {
		defer collector.Done()
		sc.collectResults()
	}()

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go sc.worker(&wg)
	}

	sc.pendingJobs.Store(1)
	select {
	case sc.jobChan <- walkJob{path: "/", depth: 0}:
	case <-sc.done:
		sc.finishJob()
	}

	wg.Wait()
	close(sc.resultChan)
	collector.Wait()

	searchTimeMs := time.Since(startTime).Milliseconds()

	response := FileSearchResponse{
		Data: sc.results,
		Meta: FileSearchMeta{
			SearchTimeMs: searchTimeMs,
			Truncated:    sc.truncated.Load(),
			Query:        query,
			MaxResults:   maxResults,
			MaxDepth:     maxDepth,
			TotalScanned: sc.totalScanned.Load(),
		},
	}

	c.JSON(http.StatusOK, response)
}
