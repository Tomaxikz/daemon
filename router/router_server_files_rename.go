package router

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/router/middleware"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

type safeRenameFile struct {
	To   string `json:"to"`
	From string `json:"from"`
}

type normalizedRenameFile struct {
	from string
	to   string
}

var (
	errDuplicateRenameDestination = errors.New("duplicate rename destination")
	errRenameDestinationExists    = errors.New("rename destination exists")
)

func putServerRenameFilesSafe(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var data struct {
		Root  string           `json:"root"`
		Files []safeRenameFile `json:"files"`
	}
	// BindJSON writes the same 400 response used by the existing Pterodactyl
	// rename handler when the request body cannot be decoded.
	if err := c.BindJSON(&data); err != nil {
		return
	}
	if len(data.Files) == 0 || len(data.Files) > maxCopyManyItems {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "Between 1 and 1000 rename entries are required."})
		return
	}
	root, err := normalizeBetterFilesRoot(data.Root)
	if err != nil {
		abortBetterFilesPath(c, err)
		return
	}
	normalized, err := prepareRenameFiles(s.Filesystem(), root, data.Files)
	if err != nil {
		switch {
		case errors.Is(err, errDuplicateRenameDestination):
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "Duplicate rename destinations are not allowed."})
		case errors.Is(err, errRenameDestinationExists):
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Cannot move or rename file, destination already exists."})
		case errors.Is(err, errBetterFilesInvalidPath):
			abortBetterFilesPath(c, err)
		default:
			middleware.CaptureAndAbort(c, err)
		}
		return
	}

	for _, file := range normalized {
		if c.Request.Context().Err() != nil {
			return
		}
		if file.from == file.to {
			continue
		}
		if err := s.Filesystem().RenameNoReplace(file.from, file.to); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				s.Log().WithError(err).WithField("from_path", file.from).WithField("to_path", file.to).
					Warn("failed to rename: source or target does not exist")
				continue
			}
			if errors.Is(err, os.ErrExist) {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Cannot move or rename file, destination already exists."})
				return
			}
			middleware.CaptureAndAbort(c, err)
			return
		}
		s.RenameFileHistory(file.from, file.to)
	}
	c.Status(http.StatusNoContent)
}

func prepareRenameFiles(fs *serverfs.Filesystem, root string, input []safeRenameFile) ([]normalizedRenameFile, error) {
	normalized := make([]normalizedRenameFile, 0, len(input))
	destinations := make(map[string]struct{}, len(input))
	for _, requested := range input {
		from, err := joinBetterFilesPath(root, requested.From)
		if err != nil {
			return nil, err
		}
		to, err := joinBetterFilesPath(root, requested.To)
		if err != nil {
			return nil, err
		}
		if _, duplicate := destinations[to]; duplicate {
			return nil, errDuplicateRenameDestination
		}
		destinations[to] = struct{}{}
		if err := ensureBetterFilesAllowed(fs, from, to); err != nil {
			return nil, err
		}
		sourceInfo, sourceErr := fs.UnixFS().Lstat(from)
		if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
			return nil, sourceErr
		}
		if sourceErr == nil {
			if sourceInfo.Mode()&ufs.ModeSymlink != 0 || (!sourceInfo.IsDir() && !sourceInfo.Mode().IsRegular()) {
				return nil, errBetterFilesInvalidPath
			}
			if sourceInfo.IsDir() && strings.HasPrefix(to+"/", from+"/") {
				return nil, errBetterFilesInvalidPath
			}
		}
		if from != to {
			if sourceErr == nil {
				if _, destinationErr := fs.UnixFS().Lstat(to); destinationErr == nil {
					return nil, errRenameDestinationExists
				} else if !errors.Is(destinationErr, os.ErrNotExist) {
					return nil, destinationErr
				}
			}
		}
		normalized = append(normalized, normalizedRenameFile{from: from, to: to})
	}
	return normalized, nil
}
