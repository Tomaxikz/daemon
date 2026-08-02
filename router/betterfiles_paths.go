package router

import (
	"errors"
	"net/http"
	pathpkg "path"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/sys/unix"

	"github.com/pterodactyl/wings/internal/ufs"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

const (
	betterFilesMaxPathLength = 4096
	betterFilesMaxNameLength = 255
)

var errBetterFilesInvalidPath = errors.New("invalid file path")

// normalizeBetterFilesRoot converts a user supplied server path to one canonical,
// slash-prefixed path. Parent components are rejected before cleaning so that a
// traversal attempt can never be silently reinterpreted as a different path.
func normalizeBetterFilesRoot(value string) (string, error) {
	if value == "" {
		return "/", nil
	}
	if !validBetterFilesPathText(value) {
		return "", errBetterFilesInvalidPath
	}
	if strings.Contains(value, "\\") || hasParentPathComponent(value) {
		return "", errBetterFilesInvalidPath
	}

	cleaned := pathpkg.Clean("/" + strings.TrimLeft(value, "/"))
	if cleaned == "." {
		cleaned = "/"
	}
	if !strings.HasPrefix(cleaned, "/") || len(cleaned) > betterFilesMaxPathLength {
		return "", errBetterFilesInvalidPath
	}
	if !validBetterFilesComponents(cleaned) {
		return "", errBetterFilesInvalidPath
	}
	return cleaned, nil
}

// joinBetterFilesPath resolves a relative request path below root. Absolute
// values and values resolving to root itself are deliberately rejected.
func joinBetterFilesPath(root, value string) (string, error) {
	if value == "" || pathpkg.IsAbs(value) || !validBetterFilesPathText(value) {
		return "", errBetterFilesInvalidPath
	}
	if strings.Contains(value, "\\") || hasParentPathComponent(value) {
		return "", errBetterFilesInvalidPath
	}

	relative := pathpkg.Clean(value)
	if relative == "." || relative == "/" || strings.HasPrefix(relative, "../") {
		return "", errBetterFilesInvalidPath
	}
	if !validBetterFilesComponents(relative) {
		return "", errBetterFilesInvalidPath
	}

	joined := pathpkg.Join(root, relative)
	if joined == root || joined == "/" || len(joined) > betterFilesMaxPathLength {
		return "", errBetterFilesInvalidPath
	}
	if root != "/" && !strings.HasPrefix(joined, root+"/") {
		return "", errBetterFilesInvalidPath
	}
	return joined, nil
}

func normalizeBetterFilesUploadPath(directory, filename string) (string, string, error) {
	root, err := normalizeBetterFilesRoot(directory)
	if err != nil {
		return "", "", err
	}
	if filename == "" || filename == "." || filename == ".." || !validBetterFilesPathText(filename) {
		return "", "", errBetterFilesInvalidPath
	}
	if strings.ContainsAny(filename, `/\\`) || len(filename) > betterFilesMaxNameLength {
		return "", "", errBetterFilesInvalidPath
	}
	target, err := joinBetterFilesPath(root, filename)
	if err != nil {
		return "", "", err
	}
	return root, target, nil
}

func validBetterFilesPathText(value string) bool {
	return value != "" && len(value) <= betterFilesMaxPathLength && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validBetterFilesComponents(value string) bool {
	for _, component := range strings.Split(strings.Trim(value, "/"), "/") {
		if component == "" {
			continue
		}
		if component == "." || component == ".." || len(component) > betterFilesMaxNameLength {
			return false
		}
	}
	return true
}

func hasParentPathComponent(value string) bool {
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

// ensureBetterFilesAllowed checks both canonical and relative spellings of a
// path, including its ancestors. This keeps gitignore-style directory rules
// effective without changing the behavior of Filesystem.IsIgnored globally.
func ensureBetterFilesAllowed(fs *serverfs.Filesystem, paths ...string) error {
	for _, value := range paths {
		cleaned, err := normalizeBetterFilesRoot(value)
		if err != nil {
			return err
		}
		if cleaned == "/" {
			continue
		}

		parts := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
		for i := 1; i <= len(parts); i++ {
			relative := strings.Join(parts[:i], "/")
			if err := fs.IsIgnored(relative, "/"+relative); err != nil {
				return err
			}
		}
	}
	return nil
}

func betterFilesLstat(fs *serverfs.Filesystem, value string) (ufs.FileInfo, error) {
	info, err := fs.UnixFS().Lstat(value)
	if err != nil {
		return nil, err
	}
	if info.Mode()&ufs.ModeSymlink != 0 {
		return nil, errBetterFilesInvalidPath
	}
	return info, nil
}

func betterFilesPhysicalSize(info ufs.FileInfo) int64 {
	if stat, ok := info.Sys().(*unix.Stat_t); ok {
		return stat.Blocks * 512
	}
	return info.Size()
}

func abortBetterFilesPath(c *gin.Context, err error) {
	status := http.StatusUnprocessableEntity
	if errors.Is(err, ufs.ErrNotExist) {
		status = http.StatusNotFound
	}
	c.AbortWithStatusJSON(status, gin.H{"error": "The requested file path is invalid or unavailable."})
}
