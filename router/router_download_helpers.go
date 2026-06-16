package router

import (
	"errors"
	"io"
	"mime"
	"net/http"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gabriel-vasile/mimetype"
	"github.com/gin-gonic/gin"
	"golang.org/x/sys/unix"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/router/tokens"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

type downloadFile struct {
	file ufs.File
	info ufs.FileInfo
}

func (df *downloadFile) Close() error {
	return df.file.Close()
}

func getTokenDownloadFile(c *gin.Context) (*downloadFile, bool) {
	manager := middleware.ExtractManager(c)
	token := tokens.FilePayload{}
	if err := tokens.ParseToken([]byte(c.Query("token")), &token); err != nil {
		middleware.CaptureAndAbort(c, err)
		return nil, false
	}

	s, ok := manager.Get(token.ServerUuid)
	if !ok || !token.IsUniqueRequest() || !token.HasScope(tokens.FileDownload) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return nil, false
	}

	filePath, ok := cleanDownloadFilePath(token.FilePath)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Invalid file path.",
		})
		return nil, false
	}

	return openDownloadFile(c, s.Filesystem(), filePath)
}

func openDownloadFile(c *gin.Context, fs *serverfs.Filesystem, filePath string) (*downloadFile, bool) {
	info, err := fs.UnixFS().Lstat(filePath)
	if err != nil {
		abortDownloadFileError(c, err)
		return nil, false
	}
	if info.IsDir() {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return nil, false
	}
	if !info.Mode().IsRegular() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Cannot open files of this type.",
		})
		return nil, false
	}

	file, err := fs.UnixFS().OpenFile(filePath, ufs.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		abortDownloadFileError(c, err)
		return nil, false
	}

	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		abortDownloadFileError(c, err)
		return nil, false
	}
	if openedInfo.IsDir() {
		_ = file.Close()
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return nil, false
	}
	if !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Cannot open files of this type.",
		})
		return nil, false
	}

	return &downloadFile{file: file, info: openedInfo}, true
}

func abortDownloadFileError(c *gin.Context, err error) {
	if errors.Is(err, ufs.ErrNotExist) || errors.Is(err, ufs.ErrBadPathResolution) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return
	}
	middleware.CaptureAndAbort(c, err)
}

func serveDownloadFile(c *gin.Context, df *downloadFile, disposition string, inline bool, fixedSize bool) {
	c.Header("Accept-Ranges", "bytes")
	c.Header("Content-Disposition", disposition+"; filename="+strconv.Quote(df.info.Name()))
	c.Header("Content-Type", downloadContentType(df, inline))
	c.Header("ETag", downloadETag(df.info))
	c.Header("X-Content-Type-Options", "nosniff")

	content := io.ReadSeeker(df.file)
	if fixedSize {
		content = newFixedDownloadReader(df.file, df.info.Size())
	}
	http.ServeContent(c.Writer, c.Request, df.info.Name(), df.info.ModTime(), content)
}

func downloadContentType(df *downloadFile, inline bool) string {
	if !inline {
		return "application/octet-stream"
	}
	if contentType := mime.TypeByExtension(filepath.Ext(df.info.Name())); contentType != "" {
		return contentType
	}

	m, err := mimetype.DetectReader(df.file)
	_, _ = df.file.Seek(0, io.SeekStart)
	if err == nil && m != nil {
		return m.String()
	}
	return "application/octet-stream"
}

func downloadETag(info ufs.FileInfo) string {
	return `W/"` + strconv.FormatInt(info.Size(), 16) + `-` + strconv.FormatInt(info.ModTime().UnixNano(), 16) + `"`
}

type fixedDownloadReader struct {
	reader io.ReadSeeker
	size   int64
	offset int64
}

func newFixedDownloadReader(reader io.ReadSeeker, size int64) *fixedDownloadReader {
	return &fixedDownloadReader{
		reader: reader,
		size:   size,
	}
}

func (r *fixedDownloadReader) Read(p []byte) (int, error) {
	if r.offset >= r.size {
		return 0, io.EOF
	}

	remaining := r.size - r.offset
	if int64(len(p)) > remaining {
		p = p[:int(remaining)]
	}

	n, err := r.reader.Read(p)
	if n == 0 && errors.Is(err, io.EOF) {
		for i := range p {
			p[i] = 0
		}
		r.offset += int64(len(p))
		return len(p), nil
	}

	r.offset += int64(n)
	return n, err
}

func (r *fixedDownloadReader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = r.offset + offset
	case io.SeekEnd:
		target = r.size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if target < 0 {
		return 0, errors.New("negative position")
	}
	if _, err := r.reader.Seek(target, io.SeekStart); err != nil {
		return 0, err
	}
	r.offset = target
	return target, nil
}

func cleanDownloadFilePath(value string) (string, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || strings.Contains(value, "\x00") || !utf8.ValidString(value) || len(value) > 4096 {
		return "", false
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", false
		}
	}

	cleaned := pathpkg.Clean(value)
	if cleaned == "." || cleaned == "/" || !strings.HasPrefix(cleaned, "/") {
		return "", false
	}

	return cleaned, true
}
