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

// BFM's capability check requires a POST JSON request schema containing these
// properties. A version string or a GET/POST route alone is not sufficient.
func bfmSearchV2Schema(document map[string]interface{}) map[string]interface{} {
	paths, _ := document["paths"].(map[string]interface{})
	operation, _ := paths["/api/servers/{server}/files/search"].(map[string]interface{})
	post, _ := operation["post"].(map[string]interface{})
	body, _ := post["requestBody"].(map[string]interface{})
	content, _ := body["content"].(map[string]interface{})
	jsonBody, _ := content["application/json"].(map[string]interface{})
	schema, _ := jsonBody["schema"].(map[string]interface{})
	properties, _ := schema["properties"].(map[string]interface{})
	for _, name := range []string{"path_filter", "size_filter", "content_filter", "per_page"} {
		if _, present := properties[name]; !present {
			return nil
		}
	}
	return schema
}

func TestOpenAPISearchV2SatisfiesBFMCapabilityDetection(t *testing.T) {
	_, _, handler := newBetterFilesHTTPServer(t, 10, 10)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var document map[string]interface{}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &document))
	schema := bfmSearchV2Schema(document)
	require.NotNil(t, schema)
	require.Equal(t, false, schema["additionalProperties"])
	properties := schema["properties"].(map[string]interface{})
	require.Contains(t, properties, "root")
	require.NotContains(t, properties, "match_context")
	for _, name := range []string{"path_filter", "size_filter", "content_filter"} {
		filter := properties[name].(map[string]interface{})
		require.ElementsMatch(t, []interface{}{"object", "null"}, filter["type"])
		require.Nil(t, filter["default"])
		require.Equal(t, false, filter["additionalProperties"])
	}
	path := properties["path_filter"].(map[string]interface{})["properties"].(map[string]interface{})
	for _, field := range []string{"include", "exclude", "case_insensitive"} {
		require.Contains(t, path, field)
	}
	content := properties["content_filter"].(map[string]interface{})["properties"].(map[string]interface{})
	for _, field := range []string{"query", "max_search_size", "include_unmatched", "case_insensitive"} {
		require.Contains(t, content, field)
	}
	readLimit := content["max_search_size"].(map[string]interface{})
	require.Equal(t, float64(0), readLimit["minimum"])
	require.Equal(t, float64(defaultSearchV2ReadLimit), readLimit["default"])
	require.Equal(t, float64(maxSearchV2ReadLimit), readLimit["maximum"])
	post := document["paths"].(map[string]interface{})["/api/servers/{server}/files/search"].(map[string]interface{})["post"].(map[string]interface{})
	responses := post["responses"].(map[string]interface{})
	for _, code := range []string{"200", "400", "401", "403", "404", "408", "413", "422", "500"} {
		require.Contains(t, responses, code)
	}
	resultSchema := responses["200"].(map[string]interface{})["content"].(map[string]interface{})["application/json"].(map[string]interface{})["schema"].(map[string]interface{})
	results := resultSchema["properties"].(map[string]interface{})["results"].(map[string]interface{})
	require.Equal(t, "array", results["type"])
	require.Equal(t, float64(maxSearchV2Results), results["maxItems"])
}

func TestOpenAPILegacyAndSchemaLessSearchDoNotAdvertiseAdvancedSearch(t *testing.T) {
	for _, fixture := range []string{
		`{"info":{"version":"999"},"paths":{}}`,
		`{"paths":{"/api/servers/{server}/files/search":{"get":{}}}}`,
		`{"paths":{"/api/servers/{server}/files/search":{"get":{},"post":{}}}}`,
		`{"paths":{"/api/servers/{server}/files/search":{"post":{"requestBody":{"content":{"application/json":{"schema":{"properties":{"query":{},"root":{}}}}}}}}}}`,
	} {
		var document map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(fixture), &document))
		require.Nil(t, bfmSearchV2Schema(document))
	}
}
