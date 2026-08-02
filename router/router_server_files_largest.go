package router

import (
	"context"
	"errors"
	"net/http"
	pathpkg "path"
	"sort"

	"github.com/gin-gonic/gin"
	"golang.org/x/sys/unix"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/router/middleware"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

const (
	maxLargestDirectoryEntries = 500000
	maxLargestDirectories      = 100000
	maxLargestDirectoryDepth   = 256
)

type largestDirectoryAggregate struct {
	path          string
	info          ufs.FileInfo
	logical       int64
	physical      int64
	totalLogical  int64
	totalPhysical int64
}

type largestDirectoryWalk struct {
	path  string
	depth int
}

func getServerLargestDirectories(c *gin.Context) {
	s := middleware.ExtractServer(c)
	root, err := normalizeBetterFilesRoot(c.Query("directory"))
	if err != nil || ensureBetterFilesAllowed(s.Filesystem(), root) != nil {
		abortBetterFilesPath(c, errBetterFilesInvalidPath)
		return
	}
	rootInfo, err := betterFilesLstat(s.Filesystem(), root)
	if err != nil || !rootInfo.IsDir() {
		abortBetterFilesPath(c, err)
		return
	}

	entries, err := analyzeLargestDirectories(c.Request.Context(), s.Filesystem(), root, rootInfo)
	if err != nil {
		if errors.Is(err, errLargestDirectoryTraversalLimit) {
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "Directory analysis traversal limit exceeded."})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.JSON(http.StatusOK, entries)
}

var errLargestDirectoryTraversalLimit = errors.New("largest directory traversal limit exceeded")

func analyzeLargestDirectories(ctx context.Context, fs *serverfs.Filesystem, root string, rootInfo ufs.FileInfo) ([]betterFilesEntry, error) {
	aggregates := map[string]*largestDirectoryAggregate{
		root: {path: root, info: rootInfo},
	}
	stack := []largestDirectoryWalk{{path: root}}
	seenInodes := make(map[[2]uint64]struct{})
	visitedEntries := 0

	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if directory.depth > maxLargestDirectoryDepth || len(aggregates) > maxLargestDirectories {
			return nil, errLargestDirectoryTraversalLimit
		}

		entries, err := fs.ReadDirStat(directory.path)
		if err != nil {
			return nil, err
		}
		for _, info := range entries {
			visitedEntries++
			if visitedEntries > maxLargestDirectoryEntries {
				return nil, errLargestDirectoryTraversalLimit
			}
			fullPath := pathpkg.Join(directory.path, info.Name())
			if ensureBetterFilesAllowed(fs, fullPath) != nil || info.Mode()&ufs.ModeSymlink != 0 {
				continue
			}
			if info.IsDir() {
				aggregates[fullPath] = &largestDirectoryAggregate{path: fullPath, info: info}
				stack = append(stack, largestDirectoryWalk{path: fullPath, depth: directory.depth + 1})
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if stat, ok := info.Sys().(*unix.Stat_t); ok && stat.Nlink > 1 {
				key := [2]uint64{uint64(stat.Dev), stat.Ino}
				if _, exists := seenInodes[key]; exists {
					continue
				}
				seenInodes[key] = struct{}{}
			}
			aggregate := aggregates[directory.path]
			aggregate.logical = saturatingAddInt64(aggregate.logical, info.Size())
			aggregate.physical = saturatingAddInt64(aggregate.physical, betterFilesPhysicalSize(info))
		}
	}

	candidates := make([]*largestDirectoryAggregate, 0, len(aggregates))
	var total int64
	for _, aggregate := range aggregates {
		aggregate.totalLogical = aggregate.logical
		aggregate.totalPhysical = aggregate.physical
	}
	for path, aggregate := range aggregates {
		if path == root {
			continue
		}
		for parent := pathpkg.Dir(path); ; parent = pathpkg.Dir(parent) {
			if ancestor := aggregates[parent]; ancestor != nil {
				ancestor.totalLogical = saturatingAddInt64(ancestor.totalLogical, aggregate.logical)
				ancestor.totalPhysical = saturatingAddInt64(ancestor.totalPhysical, aggregate.physical)
			}
			if parent == root || parent == "/" || parent == "." {
				break
			}
		}
	}
	for path, aggregate := range aggregates {
		if path == root || aggregate.logical <= 0 {
			continue
		}
		candidates = append(candidates, aggregate)
		total = saturatingAddInt64(total, aggregate.logical)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].logical == candidates[j].logical {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].logical > candidates[j].logical
	})
	if total == 0 {
		return []betterFilesEntry{}, nil
	}

	threshold := total - total/10
	cutoff := len(candidates)
	var cumulative int64
	for i, candidate := range candidates {
		cumulative = saturatingAddInt64(cumulative, candidate.logical)
		if cumulative >= threshold {
			cutoff = i + 1
			break
		}
	}
	if cutoff < 10 {
		cutoff = 10
		if cutoff > len(candidates) {
			cutoff = len(candidates)
		}
	}
	if cutoff > 50 {
		cutoff = 50
	}
	if cutoff > len(candidates) {
		cutoff = len(candidates)
	}

	result := make([]betterFilesEntry, 0, cutoff)
	for _, candidate := range candidates[:cutoff] {
		entry := newBetterFilesEntry(betterFilesRelativePath(root, candidate.path), candidate.info, "inode/directory")
		entry.Size = candidate.totalLogical
		entry.SizePhysical = candidate.totalPhysical
		result = append(result, entry)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Size > result[j].Size })
	return result, nil
}

func saturatingAddInt64(current, value int64) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	if value > 0 && current > maxInt64-value {
		return maxInt64
	}
	return current + value
}
