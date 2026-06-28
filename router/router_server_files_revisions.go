package router

import (
	"bytes"
	"database/sql"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/filesystem"
)

func getServerFileRevisions(c *gin.Context) {
	s := ExtractServer(c)
	filePath := normalizeFileRevisionPath(c.Query("file"))
	if filePath == "/" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid file path."})
		return
	}
	if err := s.Filesystem().IsIgnored(filePath); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	revisions, err := s.ListFileRevisions(filePath)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"revisions": revisions})
}

func getServerFileRevision(c *gin.Context) {
	s := ExtractServer(c)
	revisionID, ok := parseFileRevisionID(c)
	if !ok {
		return
	}
	content, filePath, err := s.FileRevisionContent(revisionID)
	if err != nil {
		abortFileRevisionError(c, err)
		return
	}
	if err := s.Filesystem().IsIgnored(filePath); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.Header("Content-Length", strconv.Itoa(len(content)))
	c.Header("X-File-Path", filePath)
	c.Data(http.StatusOK, "application/octet-stream", content)
}

func postServerFileRevisionRestore(c *gin.Context) {
	s := ExtractServer(c)
	revisionID, ok := parseFileRevisionID(c)
	if !ok {
		return
	}
	content, filePath, err := s.FileRevisionContent(revisionID)
	if err != nil {
		abortFileRevisionError(c, err)
		return
	}
	if err := s.Filesystem().IsIgnored(filePath); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	before, _ := captureFileRevisionContent(s, filePath)
	if err := s.Filesystem().Write(filePath, bytes.NewReader(content), int64(len(content)), 0o644); err != nil {
		if filesystem.IsErrorCode(err, filesystem.ErrCodeIsDirectory) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "Cannot restore revision, name conflicts with an existing directory by the same name.",
			})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}
	restored, ok := captureFileRevisionContent(s, filePath)
	if ok {
		user := strings.TrimSpace(c.Query("user"))
		if _, err := s.RecordFileRevision(filePath, before, restored, user); err != nil {
			s.Log().WithError(err).WithField("path", filePath).Warn("failed to record restored file revision")
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"file":        filePath,
		"revision_id": revisionID,
		"restored":    true,
	})
}

func parseFileRevisionID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("revision"), 10, 64)
	if err != nil || id <= 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid file revision."})
		return 0, false
	}
	return id, true
}

func abortFileRevisionError(c *gin.Context, err error) {
	if err == sql.ErrNoRows {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "File revision was not found."})
		return
	}
	middleware.CaptureAndAbort(c, err)
}

func normalizeFileRevisionPath(value string) string {
	value = "/" + strings.TrimLeft(value, "/")
	if value == "/." {
		return "/"
	}
	return value
}

func captureFileRevisionContent(s *server.Server, filePath string) ([]byte, bool) {
	limit := int64(config.Get().System.FileHistory.FileSizeCap)
	if limit <= 0 {
		return nil, false
	}

	f, stat, err := s.Filesystem().File(filePath)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if stat.IsDir() || stat.Size() < 0 || stat.Size() > limit || !server.ShouldRecordFileHistory(filePath, uint64(stat.Size())) {
		return nil, false
	}
	content, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(content)) > limit {
		return nil, false
	}
	return content, true
}
