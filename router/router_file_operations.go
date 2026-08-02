package router

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pterodactyl/wings/router/middleware"
)

func deleteServerFileOperation(c *gin.Context) {
	identifier := c.Param("operation")
	if _, err := uuid.Parse(identifier); err != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "The operation identifier is malformed."})
		return
	}

	s := middleware.ExtractServer(c)
	if !s.FileOperations().Cancel(identifier) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "The requested operation was not found on this server."})
		return
	}
	c.Status(http.StatusNoContent)
}
