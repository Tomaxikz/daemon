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
	search := operation("Search regular files with Wings-rs V2 filters", "200", "400", "401", "403", "404", "408", "413", "422", "500")
	search["description"] = "Returns only regular files. Result names are relative to root, but gitignore-style globs are anchored at the server filesystem root. Basename globs match at any depth; ordered ! negation is supported and excluded directories are pruned. Case-insensitive matching folds ASCII only. Size min is inclusive and max is exclusive. Content searches use the first 128 bytes as a UTF-8 heuristic, allow NUL bytes, and stop at the first literal match. include_unmatched includes oversized/unsearched files, not searched nonmatches or detected-binary files. No archives, virtual backup mounts, V1 POST payload, or match_context support. Unknown fields are rejected. Traversal/read limit errors return no partial results."
	search["security"] = []gin.H{{"daemonToken": []string{}}}
	search["parameters"] = []gin.H{{"in": "path", "name": "server", "required": true, "schema": gin.H{"type": "string", "format": "uuid"}}}
	search["x-search-limits"] = gin.H{
		"request_bytes": maxSearchV2RequestBytes, "results": maxSearchV2Results,
		"patterns_combined": maxSearchV2Patterns, "pattern_bytes_combined": maxSearchV2PatternBytes,
		"alternatives_per_pattern": maxSearchV2Alternatives, "entries": maxSearchV2Entries,
		"directories": maxSearchV2Directories, "depth": maxSearchV2Depth,
		"total_read_bytes": maxSearchV2TotalRead, "timeout_seconds": int(searchV2Timeout.Seconds()),
	}
	search["requestBody"] = gin.H{
		"required": true,
		"content": gin.H{"application/json": gin.H{"schema": gin.H{
			"type": "object", "additionalProperties": false,
			"properties": gin.H{
				"root": gin.H{"type": []string{"string", "null"}, "default": "/", "maxLength": betterFilesMaxPathLength, "description": "Server directory; omitted, null or empty means /. At most 4096 UTF-8 bytes; parent components, backslashes and symlinks are rejected."},
				"path_filter": gin.H{"type": []string{"object", "null"}, "default": nil, "additionalProperties": false, "properties": gin.H{
					"include":          gin.H{"type": []string{"array", "null"}, "default": []string{}, "maxItems": maxSearchV2Patterns, "items": gin.H{"type": "string", "maxLength": betterFilesMaxPathLength}, "description": "Ordered gitignore-style include overrides. Empty/omitted/null includes all files. At most 128 include+exclude patterns and 64 KiB combined, each at most 4096 UTF-8 bytes. Brace alternatives are limited to 64 expansions within a path component; nested/empty alternatives and alternatives containing slashes are unsupported."},
					"exclude":          gin.H{"type": []string{"array", "null"}, "default": []string{}, "maxItems": maxSearchV2Patterns, "items": gin.H{"type": "string", "maxLength": betterFilesMaxPathLength}, "description": "Ordered gitignore-style exclusions; directory matches prune descendants. A later ! pattern cannot re-include files below an already-pruned directory."},
					"case_insensitive": gin.H{"type": []string{"boolean", "null"}, "default": false},
				}},
				"size_filter": gin.H{"type": []string{"object", "null"}, "default": nil, "additionalProperties": false, "properties": gin.H{
					"min": gin.H{"type": []string{"integer", "null"}, "format": "int64", "minimum": 0, "default": nil, "description": "Inclusive minimum bytes; omitted/null is unbounded below (equivalent to zero)."},
					"max": gin.H{"type": []string{"integer", "null"}, "format": "int64", "minimum": 0, "default": nil, "description": "Exclusive maximum bytes; omitted/null is unbounded above. min > max is invalid; equal bounds give no results."},
				}},
				"content_filter": gin.H{"type": []string{"object", "null"}, "default": nil, "additionalProperties": false, "properties": gin.H{
					"query":             gin.H{"type": []string{"string", "null"}, "default": "", "maxLength": betterFilesMaxPathLength, "description": "Literal UTF-8 query, at most 4096 bytes. Empty/omitted/null matches every eligible file passing the UTF-8 head check; no regular expressions."},
					"max_search_size":   gin.H{"type": []string{"integer", "null"}, "format": "int64", "minimum": 0, "maximum": maxSearchV2ReadLimit, "default": defaultSearchV2ReadLimit, "description": "Files larger than this threshold are not content-searched. Explicit zero is preserved. At most this many bytes are scanned per eligible file; a growing file remains bounded. Unsearched included files may still read 128 bytes for MIME detection."},
					"include_unmatched": gin.H{"type": []string{"boolean", "null"}, "default": false, "description": "Include oversized files without applying the query; searched nonmatches and invalid-UTF-8-head files still do not match."},
					"case_insensitive":  gin.H{"type": []string{"boolean", "null"}, "default": false},
				}},
				"per_page": gin.H{"type": []string{"integer", "null"}, "minimum": 0, "default": 100, "description": "Maximum returned results; values above 500 are capped to 500. Zero returns an empty list. Omitted/null defaults to 100. Ordering is unspecified; no pagination cursor."},
			},
		}}},
	}
	search["responses"].(gin.H)["200"] = gin.H{"description": "Matching regular files; results is [] when empty.", "content": gin.H{"application/json": gin.H{"schema": gin.H{
		"type": "object", "required": []string{"results"}, "additionalProperties": false,
		"properties": gin.H{"results": gin.H{"type": "array", "maxItems": maxSearchV2Results, "items": gin.H{
			"type": "object", "required": []string{"name", "size", "size_physical", "directory", "file", "symlink", "mime", "created", "modified"},
			"properties": gin.H{
				"name":          gin.H{"type": "string", "description": "Slash-separated file path relative to root; never an absolute host path."},
				"size":          gin.H{"type": "integer", "format": "int64", "minimum": 0},
				"size_physical": gin.H{"type": "integer", "format": "int64", "minimum": 0, "description": "Allocated bytes where available, otherwise logical size."},
				"directory":     gin.H{"type": "boolean", "const": false}, "file": gin.H{"type": "boolean", "const": true}, "symlink": gin.H{"type": "boolean", "const": false},
				"mime": gin.H{"type": "string"}, "created": gin.H{"type": "string", "format": "date-time", "description": "Metadata-change time or modification-time fallback."}, "modified": gin.H{"type": "string", "format": "date-time"},
			},
		}}},
	}}}}
	for _, status := range []string{"400", "401", "403", "404", "408", "413", "422", "500"} {
		search["responses"].(gin.H)[status].(gin.H)["content"] = gin.H{"application/json": gin.H{"schema": gin.H{"type": "object", "required": []string{"error"}, "properties": gin.H{"error": gin.H{"type": "string"}, "request_id": gin.H{"type": "string"}}}}}
	}

	c.JSON(http.StatusOK, gin.H{
		"openapi": "3.1.0",
		"info": gin.H{
			"title":   "Wings Better Files API",
			"version": "1.0.0",
		},
		"components": gin.H{"securitySchemes": gin.H{"daemonToken": gin.H{"type": "http", "scheme": "bearer"}}},
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
				"post": search,
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
	case "401":
		return http.StatusUnauthorized
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
