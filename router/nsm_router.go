package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/router/middleware"
)

func getServerProtocolStats(c *gin.Context) {
	s := ExtractServer(c)

	stats, err := s.GetProtocolStats(c.Request.Context())
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": stats,
	})
}
