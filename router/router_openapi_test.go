package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAPIBetterFilesCapabilitiesMatchRuntimeMethods(t *testing.T) {
	_, _, handler := newBetterFilesHTTPServer(t, 10, 10)
	request := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)

	var document struct {
		Paths map[string]map[string]interface{} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &document))
	expected := map[string][]string{
		"/upload/file":                                       {"head", "patch", "post"},
		"/download/directory":                                {"get"},
		"/api/servers/{server}/files/copy-many":              {"post"},
		"/api/servers/{server}/files/rename":                 {"put"},
		"/api/servers/{server}/files/search":                 {"get", "post"},
		"/api/servers/{server}/files/largest-directories":    {"get"},
		"/api/servers/{server}/files/fingerprints":           {"get"},
		"/api/servers/{server}/files/operations/{operation}": {"delete"},
	}
	for path, methods := range expected {
		require.Contains(t, document.Paths, path)
		for _, method := range methods {
			require.Contains(t, document.Paths[path], method, "%s %s", method, path)
		}
	}

	patch, ok := document.Paths["/upload/file"]["patch"].(map[string]interface{})
	require.True(t, ok)
	responses, ok := patch["responses"].(map[string]interface{})
	require.True(t, ok)
	for _, status := range []string{"200", "408", "409", "413", "429"} {
		require.Contains(t, responses, status)
	}
	post := document.Paths["/upload/file"]["post"].(map[string]interface{})
	capability := post["x-betterfiles-folder-upload"].(map[string]interface{})
	require.Equal(t, float64(1), capability["version"])
	require.Equal(t, "paths", capability["paths_field"])
	require.Equal(t, float64(maxMultipartUploadFiles), capability["max_files"])
	content := post["requestBody"].(map[string]interface{})["content"].(map[string]interface{})
	schema := content["multipart/form-data"].(map[string]interface{})["schema"].(map[string]interface{})
	properties := schema["properties"].(map[string]interface{})
	require.Contains(t, properties, "files")
	require.Contains(t, properties, "paths")
	postResponses := post["responses"].(map[string]interface{})
	for _, status := range []string{"200", "400", "403", "404", "409", "413"} {
		require.Contains(t, postResponses, status)
	}
}
