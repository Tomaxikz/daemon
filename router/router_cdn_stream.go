package router

import (
	"mime"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/router/tokens"
)

// Handles streaming a specific file with HTTP Range support.
func getDownloadStream(c *gin.Context) {
	manager := middleware.ExtractManager(c)
	token := tokens.FilePayload{}
	if err := tokens.ParseToken([]byte(c.Query("token")), &token); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	s, ok := manager.Get(token.ServerUuid)
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return
	}

	filePath, ok := cleanDownloadStreamPath(token.FilePath)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Invalid file path.",
		})
		return
	}

	f, st, err := s.Filesystem().File(filePath)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	defer f.Close()
	if st.IsDir() {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return
	}
	if st.Mode()&os.ModeNamedPipe != 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Cannot open files of this type.",
		})
		return
	}

	contentType := st.Mimetype
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(st.Name()))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	c.Header("Accept-Ranges", "bytes")
	c.Header("Content-Disposition", "inline; filename="+strconv.Quote(st.Name()))
	c.Header("Content-Type", contentType)

	http.ServeContent(c.Writer, c.Request, st.Name(), st.ModTime(), f)
}

func cleanDownloadStreamPath(value string) (string, bool) {
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
