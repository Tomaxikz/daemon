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
	multipartUpload := operation("Upload flat files or a folder batch using multipart form data", "200", "400", "403", "404", "409", "413", "500")
	multipartUpload["x-betterfiles-folder-upload"] = gin.H{
		"version": 1, "paths_field": "paths", "max_files": maxMultipartUploadFiles,
	}
	multipartUpload["description"] = "Uses the existing signed upload URL and directory query parameter. Optional paths is one JSON string array in a multipart text field, matched to files parts in order. Paths are canonical relative paths, not URL-encoded; each basename must match its files part. Without paths, filenames remain flat. One single-use token authorizes the whole request, including failures. Invalid manifests/paths/limits are preflighted; runtime failures may leave earlier files written, so obtain a fresh token before retrying."
	multipartUpload["requestBody"] = gin.H{
		"required": true,
		"content": gin.H{"multipart/form-data": gin.H{"schema": gin.H{
			"type": "object", "required": []string{"files"},
			"properties": gin.H{
				"files": gin.H{"type": "array", "minItems": 1, "items": gin.H{"type": "string", "format": "binary"}},
				"paths": gin.H{"type": "string", "description": "Optional JSON array with exactly one relative path for each files part, in the same order.", "example": `["mods/a/config.yml","mods/b/config.yml"]`},
			},
		}}},
	}

	c.JSON(http.StatusOK, gin.H{
		"openapi": "3.1.0",
		"info": gin.H{
			"title":   "Wings Better Files API",
			"version": "1.0.0",
		},
		"paths": gin.H{
			"/upload/file": gin.H{
				"post":  multipartUpload,
				"head":  operation("Read the current resumable upload offset", "200", "404"),
				"patch": operation("Append a resumable upload chunk", "200", "408", "409", "413", "429"),
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
	case "403":
		return http.StatusForbidden
	case "408":
		return http.StatusRequestTimeout
	case "409":
		return http.StatusConflict
	case "413":
		return http.StatusRequestEntityTooLarge
	case "429":
		return http.StatusTooManyRequests
	case "422":
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
