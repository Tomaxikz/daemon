package router

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// getOpenAPI exposes the concrete Better Files capability surface. Better
// Files performs exact method detection against these path keys, so this list
// intentionally contains only routes mounted by Configure.
func getOpenAPI(c *gin.Context) {
	operation := func(summary string, statuses ...string) gin.H {
		responses := gin.H{}
		for _, status := range statuses {
			responses[status] = gin.H{"description": http.StatusText(statusCode(status))}
		}
		return gin.H{"summary": summary, "responses": responses}
	}

	c.JSON(http.StatusOK, gin.H{
		"openapi": "3.1.0",
		"info": gin.H{
			"title":   "Wings Better Files API",
			"version": "1.0.0",
		},
		"paths": gin.H{
			"/upload/file": gin.H{
				"post":  operation("Upload files using multipart form data", "200"),
				"head":  operation("Read the current resumable upload offset", "200", "404"),
				"patch": operation("Append a resumable upload chunk", "200", "409", "413"),
			},
			"/download/directory": gin.H{
				"get": operation("Stream a directory archive", "200", "404"),
			},
			"/api/servers/{server}/files/copy-many": gin.H{
				"post": operation("Copy multiple files or directories", "200", "202", "422"),
			},
			"/api/servers/{server}/files/rename": gin.H{
				"put": operation("Rename multiple files", "204", "400", "422"),
			},
			"/api/servers/{server}/files/search": gin.H{
				"get":  operation("Search files using the V1 query contract", "200", "400"),
				"post": operation("Search files using structured filters", "200", "422"),
			},
			"/api/servers/{server}/files/largest-directories": gin.H{
				"get": operation("Analyze directory disk usage", "200", "404", "422"),
			},
			"/api/servers/{server}/files/fingerprints": gin.H{
				"get": operation("Fingerprint files", "200", "422"),
			},
			"/api/servers/{server}/files/operations/{operation}": gin.H{
				"delete": operation("Cancel a file operation", "204", "404", "422"),
			},
		},
	})
}

func statusCode(value string) int {
	switch value {
	case "200":
		return http.StatusOK
	case "202":
		return http.StatusAccepted
	case "204":
		return http.StatusNoContent
	case "400":
		return http.StatusBadRequest
	case "404":
		return http.StatusNotFound
	case "409":
		return http.StatusConflict
	case "413":
		return http.StatusRequestEntityTooLarge
	case "422":
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
