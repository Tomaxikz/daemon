package router

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	pathpkg "path"
	"strconv"
	"strings"
	"unicode"

	"github.com/dsnet/compress/bzip2"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/router/tokens"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

type directoryArchiveFormat struct {
	name      string
	extension string
	mime      string
}

var directoryArchiveFormats = map[string]directoryArchiveFormat{
	"tar":      {name: "tar", extension: "tar", mime: "application/x-tar"},
	"tar_gz":   {name: "tar_gz", extension: "tar.gz", mime: "application/gzip"},
	"tar_xz":   {name: "tar_xz", extension: "tar.xz", mime: "application/x-xz"},
	"tar_bz2":  {name: "tar_bz2", extension: "tar.bz2", mime: "application/x-bzip2"},
	"tar_zstd": {name: "tar_zstd", extension: "tar.zst", mime: "application/zstd"},
	"zip":      {name: "zip", extension: "zip", mime: "application/zip"},
}

const (
	maxDirectoryArchiveEntries = 500000
	maxDirectoryArchiveDepth   = 256
)

func getDownloadDirectory(c *gin.Context) {
	manager := middleware.ExtractManager(c)
	token := tokens.FilePayload{}
	if err := tokens.ParseToken([]byte(c.Query("token")), &token); err != nil {
		abortDirectoryDownload(c)
		return
	}
	s, ok := manager.Get(token.ServerUuid)
	if !ok || token.Denylisted() || !token.IsUniqueRequest() || !token.HasScope(tokens.FileDownload) {
		abortDirectoryDownload(c)
		return
	}

	formatName := c.Query("archive_format")
	if formatName == "" {
		formatName = "tar_gz"
	}
	format, ok := directoryArchiveFormats[formatName]
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "Unsupported directory archive format."})
		return
	}
	root, err := normalizeBetterFilesRoot(token.FilePath)
	if err != nil || ensureBetterFilesAllowed(s.Filesystem(), root) != nil {
		abortDirectoryDownload(c)
		return
	}
	info, err := betterFilesLstat(s.Filesystem(), root)
	if err != nil || !info.IsDir() {
		abortDirectoryDownload(c)
		return
	}

	name := pathpkg.Base(root)
	if root == "/" || name == "." || name == "/" {
		name = "server-files"
	}
	filename := safeArchiveFilename(name) + "." + format.extension
	c.Header("Content-Disposition", "attachment; filename="+strconv.Quote(filename))
	c.Header("Content-Type", format.mime)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)

	if err := streamDirectoryArchive(c.Request.Context(), c.Writer, s.Filesystem(), root, format); err != nil && !errors.Is(err, context.Canceled) {
		s.Log().WithError(err).Warn("directory archive stream ended with an error")
	}
}

func abortDirectoryDownload(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "The requested directory was not found on this server."})
}

func safeArchiveFilename(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character > unicode.MaxASCII || character < 0x20 || character == 0x7f || character == '/' || character == '\\' {
			builder.WriteByte('_')
		} else {
			builder.WriteRune(character)
		}
	}
	if builder.Len() == 0 {
		return "directory"
	}
	return builder.String()
}

func streamDirectoryArchive(ctx context.Context, output io.Writer, fs *serverfs.Filesystem, root string, format directoryArchiveFormat) error {
	if format.name == "zip" {
		writer := zip.NewWriter(output)
		err := writeZipDirectory(ctx, writer, fs, root)
		closeErr := writer.Close()
		if err != nil {
			return err
		}
		return closeErr
	}

	compressed, closeCompressed, err := directoryArchiveCompressor(output, format.name)
	if err != nil {
		return err
	}
	writer := tar.NewWriter(compressed)
	err = writeTarDirectory(ctx, writer, fs, root)
	closeTarErr := writer.Close()
	closeCompressionErr := closeCompressed()
	if err != nil {
		return err
	}
	if closeTarErr != nil {
		return closeTarErr
	}
	return closeCompressionErr
}

func directoryArchiveCompressor(output io.Writer, format string) (io.Writer, func() error, error) {
	switch format {
	case "tar":
		return output, func() error { return nil }, nil
	case "tar_gz":
		writer, err := gzip.NewWriterLevel(output, gzip.DefaultCompression)
		if err != nil {
			return nil, nil, err
		}
		return writer, writer.Close, nil
	case "tar_xz":
		writer, err := xz.NewWriter(output)
		if err != nil {
			return nil, nil, err
		}
		return writer, writer.Close, nil
	case "tar_bz2":
		writer, err := bzip2.NewWriter(output, &bzip2.WriterConfig{Level: 6})
		if err != nil {
			return nil, nil, err
		}
		return writer, writer.Close, nil
	case "tar_zstd":
		writer, err := zstd.NewWriter(output, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, nil, err
		}
		return writer, writer.Close, nil
	default:
		return nil, nil, errors.New("unsupported archive format")
	}
}

type directoryArchiveItem struct {
	path  string
	rel   string
	depth int
}

func writeTarDirectory(ctx context.Context, writer *tar.Writer, fs *serverfs.Filesystem, root string) error {
	buffer := make([]byte, 64*1024)
	return walkDirectoryArchive(ctx, fs, root, func(item directoryArchiveItem, info ufs.FileInfo, file io.Reader) error {
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = item.rel
		if info.IsDir() {
			header.Name += "/"
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if file == nil {
			return nil
		}
		written, err := io.CopyBuffer(writerOnly{Writer: writer}, file, buffer)
		if err != nil {
			return err
		}
		if written != info.Size() {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
}

func writeZipDirectory(ctx context.Context, writer *zip.Writer, fs *serverfs.Filesystem, root string) error {
	buffer := make([]byte, 64*1024)
	return walkDirectoryArchive(ctx, fs, root, func(item directoryArchiveItem, info ufs.FileInfo, file io.Reader) error {
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = item.rel
		if info.IsDir() {
			header.Name += "/"
			header.Method = zip.Store
		} else {
			header.Method = zip.Deflate
		}
		destination, err := writer.CreateHeader(header)
		if err != nil || file == nil {
			return err
		}
		written, err := io.CopyBuffer(writerOnly{Writer: destination}, file, buffer)
		if err != nil {
			return err
		}
		if written != info.Size() {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
}

func walkDirectoryArchive(
	ctx context.Context,
	fs *serverfs.Filesystem,
	root string,
	visit func(directoryArchiveItem, ufs.FileInfo, io.Reader) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := fs.ReadDirStat(root)
	if err != nil {
		return err
	}
	stack := make([]directoryArchiveItem, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		stack = append(stack, directoryArchiveItem{path: pathpkg.Join(root, entries[i].Name()), rel: entries[i].Name(), depth: 1})
	}

	visited := 0
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		visited++
		if visited > maxDirectoryArchiveEntries || item.depth > maxDirectoryArchiveDepth {
			return errors.New("directory archive traversal limit exceeded")
		}
		if pathpkg.IsAbs(item.rel) || hasParentPathComponent(item.rel) || ensureBetterFilesAllowed(fs, item.path) != nil {
			continue
		}
		info, err := fs.UnixFS().Lstat(item.path)
		if err != nil {
			return err
		}
		if info.Mode()&ufs.ModeSymlink != 0 {
			continue
		}
		if info.IsDir() {
			if err := visit(item, info, nil); err != nil {
				return err
			}
			children, err := fs.ReadDirStat(item.path)
			if err != nil {
				return err
			}
			for i := len(children) - 1; i >= 0; i-- {
				stack = append(stack, directoryArchiveItem{
					path:  pathpkg.Join(item.path, children[i].Name()),
					rel:   pathpkg.Join(item.rel, children[i].Name()),
					depth: item.depth + 1,
				})
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		file, err := fs.UnixFS().OpenFile(item.path, ufs.O_RDONLY|ufs.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		reader := &fingerprintContextReader{ctx: ctx, reader: io.LimitReader(file, info.Size())}
		err = visit(item, info, reader)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
