package router

import (
	"net/http"

	"github.com/gin-gonic/gin"
	ws "github.com/pterodactyl/wings/router/websocket"
)

type systemConfigurationResponse struct {
	System struct {
		FileCollaboration struct {
			Enabled     bool `json:"enabled"`
			FileSizeCap int  `json:"file_size_cap"`
		} `json:"file_collaboration"`
	} `json:"system"`
}

// getSystemConfiguration reports optional daemon capabilities to the Panel.
// This route is protected by the same daemon-token middleware as /api/system.
func getSystemConfiguration(c *gin.Context) {
	var response systemConfigurationResponse
	response.System.FileCollaboration.Enabled = ws.NativeFileCollaborationConfigured()
	response.System.FileCollaboration.FileSizeCap = ws.NativeFileCollaborationFileSizeCap()
	c.JSON(http.StatusOK, response)
}
