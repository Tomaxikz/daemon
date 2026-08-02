package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
)

func TestSetAccessControlHeadersAllowsRangeStreaming(t *testing.T) {
	config.Set(&config.Configuration{
		PanelLocation:           "https://panel.example.com",
		AllowedOrigins:          []string{"https://cdn.example.com"},
		AllowCORSPrivateNetwork: true,
		AuthenticationToken:     "token",
		AuthenticationTokenId:   "token-id",
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SetAccessControlHeaders())
	router.GET("/download/stream", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	router.GET("/download/file", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	router.GET("/download/directory", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	router.PATCH("/upload/file", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	t.Run("stream route exposes range headers", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodOptions, "/download/stream", nil)
		request.Header.Set("Origin", "https://cdn.example.com")
		request.Header.Set("Access-Control-Request-Headers", "range")
		recorder := httptest.NewRecorder()

		router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusNoContent {
			t.Fatalf("expected status %d, got %d", http.StatusNoContent, recorder.Code)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://cdn.example.com" {
			t.Fatalf("expected allowed origin to match request origin, got %q", got)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Range") {
			t.Fatalf("expected Range in allowed headers, got %q", got)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "If-Range") {
			t.Fatalf("expected If-Range in allowed headers, got %q", got)
		}
		if got := recorder.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Content-Range") {
			t.Fatalf("expected Content-Range in exposed headers, got %q", got)
		}
		if got := recorder.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Last-Modified") {
			t.Fatalf("expected Last-Modified in exposed headers, got %q", got)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
			t.Fatalf("expected private network CORS header, got %q", got)
		}
	})

	t.Run("directory archive route exposes download headers", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodOptions, "/download/directory", nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if got := recorder.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Content-Disposition") {
			t.Fatalf("expected Content-Disposition in exposed headers, got %q", got)
		}
	})

	t.Run("resumable upload route allows and exposes offset headers", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodOptions, "/upload/file", nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if got := recorder.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Upload-Offset") || !strings.Contains(got, "Upload-Length") {
			t.Fatalf("expected resumable upload headers, got %q", got)
		}
		if got := recorder.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Upload-Offset") {
			t.Fatalf("expected Upload-Offset in exposed headers, got %q", got)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "HEAD") || !strings.Contains(got, "PATCH") {
			t.Fatalf("expected resumable upload methods, got %q", got)
		}
	})

	t.Run("download route keeps default headers", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodOptions, "/download/file", nil)
		request.Header.Set("Origin", "https://cdn.example.com")
		request.Header.Set("Access-Control-Request-Headers", "range")
		recorder := httptest.NewRecorder()

		router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusNoContent {
			t.Fatalf("expected status %d, got %d", http.StatusNoContent, recorder.Code)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Headers"); strings.Contains(got, "Range") {
			t.Fatalf("expected default headers without Range, got %q", got)
		}
		if got := recorder.Header().Get("Access-Control-Expose-Headers"); got != "" {
			t.Fatalf("expected no exposed headers on normal download route, got %q", got)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Private-Network"); got != "" {
			t.Fatalf("expected no stream private network header, got %q", got)
		}
	})
}
