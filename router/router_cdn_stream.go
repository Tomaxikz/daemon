package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/router/tokens"
)

// Handles streaming a specific file with HTTP Range support.
func getDownloadStream(c *gin.Context) {
	df, ok := getTokenStreamFile(c)
	if !ok {
		return
	}
	defer df.Close()

	serveDownloadFile(c, df, "inline", true, false)
}

func getTokenStreamFile(c *gin.Context) (*downloadFile, bool) {
	manager := middleware.ExtractManager(c)
	token := tokens.FilePayload{}
	if err := tokens.ParseToken([]byte(c.Query("token")), &token); err != nil {
		middleware.CaptureAndAbort(c, err)
		return nil, false
	}

	s, ok := manager.Get(token.ServerUuid)
	if !ok || token.Denylisted() || !token.HasScope(tokens.FileDownload) {
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
