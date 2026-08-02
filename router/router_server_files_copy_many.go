package router

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	pathpkg "path"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/server"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

const (
	maxCopyManyItems   = 1000
	maxCopyWalkEntries = 500000
	maxCopyWalkDepth   = 256
)

type copyManyFile struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type copyManyPayload struct {
	Root       string         `json:"root"`
	Files      []copyManyFile `json:"files"`
	Overwrite  bool           `json:"overwrite"`
	Foreground *bool          `json:"foreground"`
}

type normalizedCopyManyFile struct {
	from        string
	to          string
	requestedTo string
}

func postServerCopyMany(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var data copyManyPayload
	if err := c.ShouldBindJSON(&data); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid copy request."})
		return
	}
	if len(data.Files) == 0 || len(data.Files) > maxCopyManyItems {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "Between 1 and 1000 copy entries are required."})
		return
	}

	root, err := normalizeBetterFilesRoot(data.Root)
	if err != nil {
		abortBetterFilesPath(c, err)
		return
	}
	files, skipped, err := prepareCopyMany(s.Filesystem(), root, data.Files, data.Overwrite)
	if err != nil {
		abortBetterFilesPath(c, err)
		return
	}

	foreground := true
	if data.Foreground != nil {
		foreground = *data.Foreground
	}
	parent := s.Context()
	if foreground {
		parent = c.Request.Context()
	}

	copied := 0
	operation, done, err := s.FileOperations().Start(parent, "copy_many", func(ctx context.Context, operation *server.FileOperation) error {
		total, err := copyManyTotal(ctx, s.Filesystem(), files)
		if err != nil {
			return err
		}
		operation.SetBytesTotal(total)
		for _, file := range files {
			if err := copyManyPath(ctx, s.Filesystem(), file.from, file.to, data.Overwrite, operation); err != nil {
				return err
			}
			copied++
		}
		return nil
	})
	if err != nil {
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "Too many file operations are active."})
		return
	}

	if !foreground {
		c.JSON(http.StatusAccepted, gin.H{"identifier": operation.Identifier(), "skipped": skipped})
		return
	}
	if err := <-done; err != nil {
		c.AbortWithStatusJSON(http.StatusExpectationFailed, gin.H{"error": "File copy operation failed or was cancelled."})
		return
	}
	c.JSON(http.StatusOK, gin.H{"copied": copied, "skipped": skipped})
}

func prepareCopyMany(fs *serverfs.Filesystem, root string, input []copyManyFile, overwrite bool) ([]normalizedCopyManyFile, []betterFilesEntry, error) {
	files := make([]normalizedCopyManyFile, 0, len(input))
	skipped := make([]betterFilesEntry, 0)
	destinations := make(map[string]struct{}, len(input))

	for _, requested := range input {
		from, err := joinBetterFilesPath(root, requested.From)
		if err != nil {
			return nil, nil, err
		}
		to, err := joinBetterFilesPath(root, requested.To)
		if err != nil || from == to {
			return nil, nil, errBetterFilesInvalidPath
		}
		if _, exists := destinations[to]; exists {
			return nil, nil, errBetterFilesInvalidPath
		}
		destinations[to] = struct{}{}
		if err := ensureBetterFilesAllowed(fs, from, to); err != nil {
			return nil, nil, err
		}

		sourceInfo, err := betterFilesLstat(fs, from)
		if err != nil || (!sourceInfo.IsDir() && !sourceInfo.Mode().IsRegular()) {
			return nil, nil, errBetterFilesInvalidPath
		}
		if sourceInfo.IsDir() && strings.HasPrefix(to+"/", from+"/") {
			return nil, nil, errBetterFilesInvalidPath
		}

		destinationInfo, destinationErr := fs.UnixFS().Lstat(to)
		if destinationErr == nil {
			if destinationInfo.Mode()&ufs.ModeSymlink != 0 {
				return nil, nil, errBetterFilesInvalidPath
			}
			if !overwrite {
				skipped = append(skipped, newBetterFilesEntry(requested.To, destinationInfo, betterFilesMime(fs, to, destinationInfo)))
				continue
			}
			if sourceInfo.IsDir() != destinationInfo.IsDir() {
				return nil, nil, errBetterFilesInvalidPath
			}
		} else if !errors.Is(destinationErr, ufs.ErrNotExist) {
			return nil, nil, destinationErr
		}

		files = append(files, normalizedCopyManyFile{from: from, to: to, requestedTo: requested.To})
	}
	return files, skipped, nil
}

type copyWalkItem struct {
	from  string
	to    string
	depth int
}

type copyProgress interface {
	AddBytesProcessed(uint64)
	AddFilesProcessed(uint64)
}

func copyManyTotal(ctx context.Context, fs *serverfs.Filesystem, files []normalizedCopyManyFile) (uint64, error) {
	var total uint64
	for _, file := range files {
		stack := []copyWalkItem{{from: file.from, to: file.to}}
		visited := 0
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			item := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			visited++
			if visited > maxCopyWalkEntries || item.depth > maxCopyWalkDepth {
				return 0, errors.New("copy traversal limit exceeded")
			}

			info, err := betterFilesLstat(fs, item.from)
			if err != nil {
				return 0, err
			}
			if info.Mode().IsRegular() {
				if uint64(info.Size()) > math.MaxUint64-total {
					return 0, errors.New("copy size overflow")
				}
				total += uint64(info.Size())
				continue
			}
			if !info.IsDir() {
				continue
			}
			entries, err := fs.ReadDirStat(item.from)
			if err != nil {
				return 0, err
			}
			for _, entry := range entries {
				childFrom := pathpkg.Join(item.from, entry.Name())
				childTo := pathpkg.Join(item.to, entry.Name())
				if ensureBetterFilesAllowed(fs, childFrom, childTo) != nil || entry.Mode()&ufs.ModeSymlink != 0 {
					continue
				}
				stack = append(stack, copyWalkItem{from: childFrom, to: childTo, depth: item.depth + 1})
			}
		}
	}
	return total, nil
}

func copyManyPath(ctx context.Context, fs *serverfs.Filesystem, from, to string, overwrite bool, operation copyProgress) error {
	stack := []copyWalkItem{{from: from, to: to}}
	visited := 0
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		visited++
		if visited > maxCopyWalkEntries || item.depth > maxCopyWalkDepth {
			return errors.New("copy traversal limit exceeded")
		}

		info, err := betterFilesLstat(fs, item.from)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := ensureCopyDestinationDirectory(fs, item.to); err != nil {
				return err
			}
			entries, err := fs.ReadDirStat(item.from)
			if err != nil {
				return err
			}
			for i := len(entries) - 1; i >= 0; i-- {
				entry := entries[i]
				childFrom := pathpkg.Join(item.from, entry.Name())
				childTo := pathpkg.Join(item.to, entry.Name())
				if ensureBetterFilesAllowed(fs, childFrom, childTo) != nil || entry.Mode()&ufs.ModeSymlink != 0 {
					continue
				}
				stack = append(stack, copyWalkItem{from: childFrom, to: childTo, depth: item.depth + 1})
			}
		case info.Mode().IsRegular():
			if !overwrite {
				if _, err := fs.UnixFS().Lstat(item.to); err == nil {
					continue
				} else if !errors.Is(err, ufs.ErrNotExist) {
					return err
				}
			}
			if err := copyManyRegularFile(ctx, fs, item.from, item.to, info, operation); err != nil {
				return err
			}
		default:
			continue
		}
	}
	return nil
}

func ensureCopyDestinationDirectory(fs *serverfs.Filesystem, destination string) error {
	info, err := fs.UnixFS().Lstat(destination)
	if err == nil {
		if !info.IsDir() || info.Mode()&ufs.ModeSymlink != 0 {
			return errBetterFilesInvalidPath
		}
		return nil
	}
	if !errors.Is(err, ufs.ErrNotExist) {
		return err
	}
	return fs.CreateDirectory(pathpkg.Base(destination), pathpkg.Dir(destination))
}

func copyManyRegularFile(ctx context.Context, fs *serverfs.Filesystem, source, destination string, sourceInfo ufs.FileInfo, operation copyProgress) error {
	file, err := fs.UnixFS().OpenFile(source, ufs.O_RDONLY|ufs.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		return errBetterFilesInvalidPath
	}

	temporary := pathpkg.Join(pathpkg.Dir(destination), ".wings-copy-"+uuid.NewString())
	installed := false
	defer func() {
		_ = file.Close()
		if !installed {
			_ = fs.Delete(temporary)
		}
	}()

	reader := &copyProgressReader{ctx: ctx, reader: io.LimitReader(file, openedInfo.Size()), operation: operation}
	if err := fs.Write(temporary, reader, openedInfo.Size(), sourceInfo.Mode()&ufs.ModePerm); err != nil {
		return err
	}
	temporaryInfo, err := fs.UnixFS().Lstat(temporary)
	if err != nil || temporaryInfo.Size() != openedInfo.Size() {
		return errors.New("source changed while it was copied")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fs.Replace(temporary, destination); err != nil {
		return err
	}
	installed = true
	operation.AddFilesProcessed(1)
	return nil
}

type copyProgressReader struct {
	ctx       context.Context
	reader    io.Reader
	operation copyProgress
}

func (r *copyProgressReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
	}
	n, err := r.reader.Read(buffer)
	if n > 0 {
		r.operation.AddBytesProcessed(uint64(n))
	}
	return n, err
}
