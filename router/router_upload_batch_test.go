package router

import (
	"bytes"
	"encoding/json"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pterodactyl/wings/server"
	"github.com/stretchr/testify/require"
)

type multipartTestFile struct {
	name string
	data []byte
}

func performMultipartUpload(t *testing.T, handler http.Handler, token, directory string, files []multipartTestFile, fields [][2]string, manifestFirst bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	writeFields := func() {
		for _, field := range fields {
			require.NoError(t, w.WriteField(field[0], field[1]))
		}
	}
	if manifestFirst {
		writeFields()
	}
	for _, file := range files {
		part, err := w.CreateFormFile("files", file.name)
		require.NoError(t, err)
		_, err = part.Write(file.data)
		require.NoError(t, err)
	}
	if !manifestFirst {
		writeFields()
	}
	require.NoError(t, w.Close())
	request := httptest.NewRequest(http.MethodPost, "/upload/file?"+url.Values{"token": {token}, "directory": {directory}, "total_size": {"0"}}.Encode(), &body)
	request.Header.Set("Content-Type", w.FormDataContentType())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func batchPaths(t *testing.T, paths ...string) [][2]string {
	t.Helper()
	encoded, err := json.Marshal(paths)
	require.NoError(t, err)
	return [][2]string{{"paths", string(encoded)}}
}

func quietUploadActivity(t *testing.T) {
	t.Helper()
	previous := saveUploadActivity
	saveUploadActivity = func(*server.Server, string, string, string, string) {}
	t.Cleanup(func() { saveUploadActivity = previous })
}

func TestMultipartFolderBatchPreservesPathsAndDuplicateBasenames(t *testing.T) {
	quietUploadActivity(t)
	for _, first := range []bool{true, false} {
		_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
		var activities [][2]string
		saveUploadActivity = func(_ *server.Server, _, _, name, directory string) {
			activities = append(activities, [2]string{directory, name})
		}
		files := []multipartTestFile{{"config.yml", []byte("one")}, {"mods/b/config.yml", []byte("two")}, {"empty.txt", nil}}
		response := performMultipartUpload(t, handler, signBetterFilesUploadToken(t, s.ID()), "/uploads", files, batchPaths(t, "mods/a/config.yml", "mods/b/config.yml", "資料/empty.txt"), first)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, []byte("one"), readServerHTTPFixture(t, s, "uploads/mods/a/config.yml"))
		require.Equal(t, []byte("two"), readServerHTTPFixture(t, s, "uploads/mods/b/config.yml"))
		require.Empty(t, readServerHTTPFixture(t, s, "uploads/資料/empty.txt"))
		require.NoFileExists(t, filepath.Join(s.Filesystem().Path(), "uploads/config.yml"))
		require.Equal(t, [][2]string{{"/uploads/mods/a", "config.yml"}, {"/uploads/mods/b", "config.yml"}, {"/uploads/資料", "empty.txt"}}, activities)
		require.Equal(t, int64(6), s.Filesystem().CachedUsage())
	}
}

func TestMultipartFlatUploadsAndSingleUseTokensRemainCompatible(t *testing.T) {
	quietUploadActivity(t)
	_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
	token := signBetterFilesUploadToken(t, s.ID())
	files := []multipartTestFile{{"file.txt", []byte("first")}, {"folder/other.txt", []byte("second")}}
	response := performMultipartUpload(t, handler, token, "/existing", files, nil, false)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, []byte("first"), readServerHTTPFixture(t, s, "existing/file.txt"))
	require.Equal(t, []byte("second"), readServerHTTPFixture(t, s, "existing/other.txt"))
	response = performMultipartUpload(t, handler, token, "/existing", files, nil, false)
	require.Equal(t, http.StatusNotFound, response.Code)
	require.Equal(t, http.StatusNotFound, performResumableRequest(handler, http.MethodHead, token, "/existing", "file.txt", 0, nil, false, nil).Code)
	newToken := signBetterFilesUploadToken(t, s.ID())
	require.Equal(t, http.StatusOK, performResumableRequest(handler, http.MethodHead, newToken, "/existing", "file.txt", 0, nil, false, nil).Code)
	response = performMultipartUpload(t, handler, newToken, "/existing", files, nil, false)
	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestMultipartFlatQuotaPreservesSequentialOverwriteBehavior(t *testing.T) {
	quietUploadActivity(t)
	_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
	s.Filesystem().SetDiskLimit(10)
	require.NoError(t, s.Filesystem().WriteUpload("a", bytes.NewBufferString("1234567890"), 10))
	response := performMultipartUpload(t, handler, signBetterFilesUploadToken(t, s.ID()), "/", []multipartTestFile{{"a", nil}, {"b", []byte("1234567890")}}, nil, false)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Empty(t, readServerHTTPFixture(t, s, "a"))
	require.Equal(t, []byte("1234567890"), readServerHTTPFixture(t, s, "b"))
	require.Equal(t, int64(10), s.Filesystem().CachedUsage())

	_, s, handler = newBetterFilesHTTPServer(t, 10, 10)
	s.Filesystem().SetDiskLimit(10)
	response = performMultipartUpload(t, handler, signBetterFilesUploadToken(t, s.ID()), "/", []multipartTestFile{{"a", []byte("1234567890")}, {"a", []byte("abcdefghij")}}, nil, false)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, []byte("abcdefghij"), readServerHTTPFixture(t, s, "a"))
	require.Equal(t, int64(10), s.Filesystem().CachedUsage())
}

func TestMultipartFolderBatchRejectsInvalidManifestBeforeWrites(t *testing.T) {
	quietUploadActivity(t)
	tests := []struct {
		name   string
		fields [][2]string
	}{
		{"empty", [][2]string{{"paths", ""}}}, {"null", [][2]string{{"paths", "null"}}},
		{"object", [][2]string{{"paths", `{"file.txt":"folder/file.txt"}`}}},
		{"short", batchPaths(t, "good.txt")}, {"long", batchPaths(t, "good.txt", "folder/file.txt", "extra.txt")},
		{"duplicate field", [][2]string{{"paths", `["good.txt","file.txt"]`}, {"paths", `["good.txt","file.txt"]`}}},
		{"duplicate destination", batchPaths(t, "same/good.txt", "same/good.txt")},
		{"null entry", [][2]string{{"paths", `["good.txt",null]`}}},
		{"invalid UTF8", [][2]string{{"paths", "[\"good.txt\",\"\xff/file.txt\"]"}}},
		{"mismatched basename", batchPaths(t, "good.txt", "folder/wrong.txt")},
		{"extra JSON", [][2]string{{"paths", `["good.txt","file.txt"] []`}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
			response := performMultipartUpload(t, handler, signBetterFilesUploadToken(t, s.ID()), "/new", []multipartTestFile{{"good.txt", []byte("good")}, {"file.txt", []byte("bad")}}, test.fields, false)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			require.NoDirExists(t, filepath.Join(s.Filesystem().Path(), "new"))
		})
	}
}

func TestMultipartFolderBatchRejectsTraversalAndPathAliases(t *testing.T) {
	quietUploadActivity(t)
	for _, value := range []string{"../file.txt", "/file.txt", "a/../../file.txt", "a\\file.txt", "C:/file.txt", "a//file.txt", "a/./file.txt", "a/../file.txt", "./file.txt", "a/\x00/file.txt", "", ".", strings.Repeat("a", 256) + "/file.txt", strings.Repeat("dir/", 1100) + "file.txt"} {
		t.Run(value, func(t *testing.T) {
			_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
			response := performMultipartUpload(t, handler, signBetterFilesUploadToken(t, s.ID()), "/new", []multipartTestFile{{"good.txt", []byte("good")}, {"file.txt", []byte("bad")}}, batchPaths(t, "good.txt", value), false)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			require.NoDirExists(t, filepath.Join(s.Filesystem().Path(), "new"))
		})
	}
}

func TestMultipartFolderBatchRejectsDuplicatesAndFileDirectoryConflicts(t *testing.T) {
	quietUploadActivity(t)
	for _, paths := range [][]string{{"a/file.txt", "a/file.txt"}, {"a", "a/file.txt"}, {"a/file.txt", "a"}} {
		_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
		files := []multipartTestFile{}
		for _, p := range paths {
			files = append(files, multipartTestFile{filepath.Base(p), []byte("data")})
		}
		response := performMultipartUpload(t, handler, signBetterFilesUploadToken(t, s.ID()), "/new", files, batchPaths(t, paths...), false)
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		require.NoDirExists(t, filepath.Join(s.Filesystem().Path(), "new"))
	}
}

func TestMultipartFolderBatchIgnoredPathsAndSymlinks(t *testing.T) {
	quietUploadActivity(t)
	for _, rule := range []string{"secret", "/secret", "secret/*"} {
		_, s, handler := newBetterFilesHTTPServer(t, 10, 10, rule)
		response := performMultipartUpload(t, handler, signBetterFilesUploadToken(t, s.ID()), "/", []multipartTestFile{{"good.txt", []byte("good")}, {"file.txt", []byte("bad")}}, batchPaths(t, "good.txt", "secret/nested/file.txt"), false)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		require.NoFileExists(t, filepath.Join(s.Filesystem().Path(), "good.txt"))
	}
	for _, target := range []string{"link/file.txt", "link/nested/file.txt", "file.txt"} {
		_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
		outside := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outside, "file.txt"), []byte("protected"), 0o600))
		if target == "file.txt" {
			require.NoError(t, os.Symlink(filepath.Join(outside, "file.txt"), filepath.Join(s.Filesystem().Path(), "file.txt")))
		} else {
			require.NoError(t, os.Symlink(outside, filepath.Join(s.Filesystem().Path(), "link")))
		}
		response := performMultipartUpload(t, handler, signBetterFilesUploadToken(t, s.ID()), "/", []multipartTestFile{{"good.txt", []byte("good")}, {"file.txt", []byte("bad")}}, batchPaths(t, "good.txt", target), false)
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		require.NoFileExists(t, filepath.Join(s.Filesystem().Path(), "good.txt"))
		data, err := os.ReadFile(filepath.Join(outside, "file.txt"))
		require.NoError(t, err)
		require.Equal(t, "protected", string(data))
		require.NoDirExists(t, filepath.Join(outside, "nested"))
	}
}

func TestMultipartFolderBatchLimitsQuotaAndRejectedTokenReuse(t *testing.T) {
	quietUploadActivity(t)
	for _, limits := range [][2]int64{{1, 10}, {10, 1}} {
		_, s, handler := newBetterFilesHTTPServer(t, limits[0], limits[1])
		token := signBetterFilesUploadToken(t, s.ID())
		files := []multipartTestFile{{"a", bytes.Repeat([]byte("a"), 600*1024)}, {"b", bytes.Repeat([]byte("b"), 600*1024)}}
		response := performMultipartUpload(t, handler, token, "/new", files, batchPaths(t, "a", "nested/b"), false)
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		require.NoDirExists(t, filepath.Join(s.Filesystem().Path(), "new"))
		response = performMultipartUpload(t, handler, token, "/", []multipartTestFile{{"a", []byte("x")}}, nil, false)
		require.Equal(t, http.StatusNotFound, response.Code)
	}
	_, s, handler := newBetterFilesHTTPServer(t, 1, 10)
	response := performMultipartUpload(t, handler, signBetterFilesUploadToken(t, s.ID()), "/", []multipartTestFile{{"big", bytes.Repeat([]byte("x"), 10*1024*1024)}}, batchPaths(t, "folder/big"), false)
	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code, response.Body.String())
	require.NoDirExists(t, filepath.Join(s.Filesystem().Path(), "folder"))
}

func TestMultipartFolderBatchPlanRejectsSizeOverflowAndFileCount(t *testing.T) {
	for _, sizes := range [][]int64{{-1}, {math.MaxInt64, 1}, {math.MaxInt64/2 + 1, math.MaxInt64/2 + 1}} {
		form := &multipart.Form{File: map[string][]*multipart.FileHeader{"files": {}}}
		for _, size := range sizes {
			form.File["files"] = append(form.File["files"], &multipart.FileHeader{Filename: "file.txt", Size: size})
		}
		_, err := multipartUploadTargets(form, "/", math.MaxInt64)
		require.Error(t, err)
	}
	form := &multipart.Form{Value: map[string][]string{"paths": {"[]"}}, File: map[string][]*multipart.FileHeader{"files": make([]*multipart.FileHeader, maxMultipartUploadFiles+1)}}
	_, err := multipartUploadTargets(form, "/", 10)
	require.Error(t, err)
	delete(form.Value, "paths")
	for i := range form.File["files"] {
		form.File["files"][i] = &multipart.FileHeader{Filename: "file.txt"}
	}
	targets, err := multipartUploadTargets(form, "/", 10)
	require.NoError(t, err, "flat uploads retain the multipart parser's original file-count limit")
	require.Len(t, targets, maxMultipartUploadFiles+1)
}
