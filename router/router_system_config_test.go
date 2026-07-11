package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/server"
	"github.com/stretchr/testify/require"
)

func TestConfigureRegistersSystemConfigurationRoute(t *testing.T) {
	r := Configure(server.NewEmptyManager(nil), nil)
	for _, route := range r.Routes() {
		if route.Method == http.MethodGet && route.Path == "/api/system/config" {
			return
		}
	}
	t.Fatal("GET /api/system/config is not registered")
}

func TestSystemConfigurationCapabilityRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := config.Get()
	next := *previous
	next.AuthenticationToken = "native-collaboration-test-token"
	next.Token.Token = "native-collaboration-test-token"
	config.Set(&next)
	t.Cleanup(func() { config.Set(previous) })

	r := gin.New()
	protected := r.Group("/api", middleware.RequireAuthorization())
	protected.GET("/system/config", getSystemConfiguration)

	request := func(token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/system/config", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r.ServeHTTP(w, req)
		return w
	}

	require.Equal(t, http.StatusUnauthorized, request("").Code)
	require.Equal(t, http.StatusForbidden, request("wrong-token").Code)

	w := request("native-collaboration-test-token")
	require.Equal(t, http.StatusOK, w.Code)
	var response map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, map[string]any{
		"system": map[string]any{
			"file_collaboration": map[string]any{"enabled": true},
		},
	}, response)
}
