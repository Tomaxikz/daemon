package router

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDirectoryDownloadJWTDirectoryValidationAndIgnoredFiles(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 10, "folder/secret.txt")
	writeBetterFilesFixture(t, s.Filesystem(), "/folder/visible.txt", []byte("visible"))
	writeBetterFilesFixture(t, s.Filesystem(), "/folder/secret.txt", []byte("secret"))
	token := signBetterFilesDownloadToken(t, s.ID(), "/folder")
	request := httptest.NewRequest(http.MethodGet, "/download/directory?"+url.Values{
		"token":          {token},
		"archive_format": {"zip"},
	}.Encode(), nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "application/zip", recorder.Header().Get("Content-Type"))
	require.Contains(t, recorder.Header().Get("Content-Disposition"), "folder.zip")

	archive, err := zip.NewReader(bytes.NewReader(recorder.Body.Bytes()), int64(recorder.Body.Len()))
	require.NoError(t, err)
	files := map[string][]byte{}
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		opened, err := entry.Open()
		require.NoError(t, err)
		files[entry.Name], err = io.ReadAll(opened)
		require.NoError(t, err)
		require.NoError(t, opened.Close())
	}
	require.Equal(t, []byte("visible"), files["visible.txt"])
	require.NotContains(t, files, "secret.txt")

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/download/directory?token=invalid", nil))
	require.Equal(t, http.StatusNotFound, invalid.Code)

	fileToken := signBetterFilesDownloadToken(t, s.ID(), "/folder/visible.txt")
	fileResponse := httptest.NewRecorder()
	handler.ServeHTTP(fileResponse, httptest.NewRequest(http.MethodGet, "/download/directory?"+url.Values{"token": {fileToken}}.Encode(), nil))
	require.Equal(t, http.StatusNotFound, fileResponse.Code)

	traversalToken := signBetterFilesDownloadToken(t, s.ID(), "/../folder")
	traversalResponse := httptest.NewRecorder()
	handler.ServeHTTP(traversalResponse, httptest.NewRequest(http.MethodGet, "/download/directory?"+url.Values{"token": {traversalToken}}.Encode(), nil))
	require.Equal(t, http.StatusNotFound, traversalResponse.Code)
}
