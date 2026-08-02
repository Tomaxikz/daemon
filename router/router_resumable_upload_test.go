package router

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/server"
)

func TestResumableUploadBehavior(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 20)
	token := signBetterFilesUploadToken(t, s.ID())
	var activities atomic.Int64
	previousActivity := saveResumableUploadActivity
	saveResumableUploadActivity = func(_ *server.Server, _, _, _, _ string) { activities.Add(1) }
	t.Cleanup(func() { saveResumableUploadActivity = previousActivity })

	total := int64(11)
	first := performResumableRequest(handler, http.MethodPatch, token, "/uploads", "hello.txt", 0, &total, false, bytes.NewBufferString("hello "))
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, "6", first.Header().Get("Upload-Offset"))
	head := performResumableRequest(handler, http.MethodHead, token, "/uploads", "hello.txt", 0, nil, false, nil)
	require.Equal(t, http.StatusOK, head.Code)
	require.Equal(t, "6", head.Header().Get("Upload-Offset"))
	second := performResumableRequest(handler, http.MethodPatch, token, "/uploads", "hello.txt", 6, &total, true, bytes.NewBufferString("world"))
	require.Equal(t, http.StatusOK, second.Code)
	require.Equal(t, "11", second.Header().Get("Upload-Offset"))
	require.Equal(t, []byte("hello world"), readServerHTTPFixture(t, s, "/uploads/hello.txt"))
	require.Equal(t, int64(1), activities.Load())

	emptyLength := int64(0)
	empty := performResumableRequest(handler, http.MethodPatch, token, "/uploads", "empty.txt", 0, &emptyLength, true, bytes.NewReader(nil))
	require.Equal(t, http.StatusOK, empty.Code)
	info, err := os.Stat(filepath.Join(s.Filesystem().Path(), "uploads", "empty.txt"))
	require.NoError(t, err)
	require.Zero(t, info.Size())
	require.Equal(t, int64(2), activities.Load())
}

func TestResumableUploadIncorrectOffsetAndFinalLengthRollback(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 20)
	token := signBetterFilesUploadToken(t, s.ID())
	previousActivity := saveResumableUploadActivity
	saveResumableUploadActivity = func(_ *server.Server, _, _, _, _ string) {}
	t.Cleanup(func() { saveResumableUploadActivity = previousActivity })

	total := int64(4)
	require.Equal(t, http.StatusOK, performResumableRequest(handler, http.MethodPatch, token, "/", "offset.txt", 0, &total, false, bytes.NewBufferString("ab")).Code)
	wrong := performResumableRequest(handler, http.MethodPatch, token, "/", "offset.txt", 0, &total, false, bytes.NewBufferString("zz"))
	require.Equal(t, http.StatusConflict, wrong.Code)
	require.Equal(t, "2", wrong.Header().Get("Upload-Offset"))
	require.Equal(t, []byte("ab"), readServerHTTPFixture(t, s, "/offset.txt"))

	lengthMismatch := int64(5)
	mismatch := performResumableRequest(handler, http.MethodPatch, token, "/", "mismatch.txt", 0, &lengthMismatch, true, bytes.NewBufferString("abc"))
	require.Equal(t, http.StatusConflict, mismatch.Code)
	require.Equal(t, []byte{}, readServerHTTPFixture(t, s, "/mismatch.txt"))
}

func TestResumableUploadConcurrentChunksCannotInterleave(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 20)
	token := signBetterFilesUploadToken(t, s.ID())
	total := int64(4)
	responses := make(chan int, 2)
	var workers sync.WaitGroup
	for _, content := range []string{"aaaa", "bbbb"} {
		content := content
		workers.Add(1)
		go func() {
			defer workers.Done()
			responses <- performResumableRequest(handler, http.MethodPatch, token, "/", "concurrent.txt", 0, &total, false, bytes.NewBufferString(content)).Code
		}()
	}
	workers.Wait()
	close(responses)
	counts := map[int]int{}
	for status := range responses {
		counts[status]++
	}
	require.Equal(t, 1, counts[http.StatusOK])
	require.Equal(t, 1, counts[http.StatusConflict])
	data := readServerHTTPFixture(t, s, "/concurrent.txt")
	require.True(t, bytes.Equal(data, []byte("aaaa")) || bytes.Equal(data, []byte("bbbb")))
}

func TestResumableUploadLimitsQuotaAndIgnoredPath(t *testing.T) {
	_, limitedServer, limitedHandler := newBetterFilesHTTPServer(t, 1, 10)
	limitedToken := signBetterFilesUploadToken(t, limitedServer.ID())
	tooLarge := int64(1024*1024 + 1)
	response := performResumableRequest(limitedHandler, http.MethodPatch, limitedToken, "/", "large.bin", 0, &tooLarge, false, bytes.NewReader(nil))
	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)

	_, quotaServer, quotaHandler := newBetterFilesHTTPServer(t, 10, 1)
	quotaToken := signBetterFilesUploadToken(t, quotaServer.ID())
	body := bytes.Repeat([]byte{'x'}, 1024*1024+1)
	quotaLength := int64(len(body))
	response = performResumableRequest(quotaHandler, http.MethodPatch, quotaToken, "/", "quota.bin", 0, &quotaLength, false, bytes.NewReader(body))
	require.Equal(t, http.StatusInsufficientStorage, response.Code)

	_, ignoredServer, ignoredHandler := newBetterFilesHTTPServer(t, 10, 10, "private/**")
	ignoredToken := signBetterFilesUploadToken(t, ignoredServer.ID())
	ignoredLength := int64(1)
	response = performResumableRequest(ignoredHandler, http.MethodPatch, ignoredToken, "/private", "secret.txt", 0, &ignoredLength, true, bytes.NewBufferString("x"))
	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestResumableUploadOffsetHeaderIsDecimalBytes(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
	token := signBetterFilesUploadToken(t, s.ID())
	content := []byte("😀")
	length := int64(len(content))
	response := performResumableRequest(handler, http.MethodPatch, token, "/", "utf8.txt", 0, &length, false, bytes.NewReader(content))
	require.Equal(t, http.StatusOK, response.Code)
	offset, err := strconv.ParseInt(response.Header().Get("Upload-Offset"), 10, 64)
	require.NoError(t, err)
	require.Equal(t, int64(4), offset)
}
