package router

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/router/websocket"
)

type betterFilesCollaborationRevokeRequest struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}

func postServerBetterFilesCollaborationRevoke(c *gin.Context) {
	s := middleware.ExtractServer(c)

	var request betterFilesCollaborationRevokeRequest
	_ = c.ShouldBindJSON(&request)

	mode := strings.TrimSpace(request.Mode)
	if mode != "edit" && mode != "view" {
		mode = ""
	}

	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = "Live collaboration was revoked by an administrator."
	}
	if len(reason) > 160 {
		reason = reason[:160]
	}

	closed := websocket.CloseBetterFilesCollaborationSessions(s.ID(), mode, reason)
	c.JSON(http.StatusOK, gin.H{"closed": closed})
}
