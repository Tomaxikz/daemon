package router

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/router/tokens"
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
	emptyToken := signBetterFilesUploadToken(t, s.ID())
	empty := performResumableRequest(handler, http.MethodPatch, emptyToken, "/uploads", "empty.txt", 0, &emptyLength, true, bytes.NewReader(nil))
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
	mismatchToken := signBetterFilesUploadToken(t, s.ID())
	mismatch := performResumableRequest(handler, http.MethodPatch, mismatchToken, "/", "mismatch.txt", 0, &lengthMismatch, true, bytes.NewBufferString("abc"))
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

func TestResumableUploadTokenIsBoundToOneTargetAndRetiredOnCompletion(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 20)
	token := signBetterFilesUploadToken(t, s.ID())
	previousActivity := saveResumableUploadActivity
	saveResumableUploadActivity = func(_ *server.Server, _, _, _, _ string) {}
	t.Cleanup(func() { saveResumableUploadActivity = previousActivity })
	total := int64(4)

	first := performResumableRequest(handler, http.MethodPatch, token, "/", "bound.txt", 0, &total, false, bytes.NewBufferString("ab"))
	require.Equal(t, http.StatusOK, first.Code)

	replayed := performResumableRequest(handler, http.MethodPatch, token, "/", "other.txt", 0, &total, false, bytes.NewBufferString("xx"))
	require.Equal(t, http.StatusNotFound, replayed.Code)
	replayedHead := performResumableRequest(handler, http.MethodHead, token, "/", "other.txt", 0, nil, false, nil)
	require.Equal(t, http.StatusNotFound, replayedHead.Code)

	completed := performResumableRequest(handler, http.MethodPatch, token, "/", "bound.txt", 2, &total, true, bytes.NewBufferString("cd"))
	require.Equal(t, http.StatusOK, completed.Code)
	require.Equal(t, []byte("abcd"), readServerHTTPFixture(t, s, "/bound.txt"))

	retired := performResumableRequest(handler, http.MethodHead, token, "/", "bound.txt", 0, nil, false, nil)
	require.Equal(t, http.StatusNotFound, retired.Code)
}

func TestResumableUploadSessionsAreBoundedAndPruneExpiredEntries(t *testing.T) {
	sessions := newResumableUploadSessionRegistry(1, 1)
	now := time.Now()
	first := &tokens.UploadPayload{
		Payload:  jwt.Payload{ExpirationTime: jwt.NumericDate(now.Add(time.Hour))},
		UniqueId: fmt.Sprintf("%s-first-%d", t.Name(), now.UnixNano()),
	}
	second := &tokens.UploadPayload{
		Payload:  jwt.Payload{ExpirationTime: jwt.NumericDate(now.Add(time.Hour))},
		UniqueId: fmt.Sprintf("%s-second-%d", t.Name(), now.UnixNano()),
	}

	require.True(t, sessions.bind(first, "server", "/first.txt"))
	require.False(t, sessions.bind(first, "server", "/other.txt"))
	require.False(t, sessions.bind(second, "server", "/second.txt"), "a full live registry must reject a new session")

	sessions.mu.Lock()
	expired := sessions.sessions[first.UniqueId]
	expired.expires = now.Add(-time.Second)
	sessions.sessions[first.UniqueId] = expired
	sessions.nextExpiry = expired.expires
	sessions.mu.Unlock()

	require.True(t, sessions.bind(second, "server", "/second.txt"), "expired entries must be pruned before rejecting a new session")
	require.Equal(t, 1, sessions.byServer["server"])
}

func TestResumableUploadCompletedSessionConsumesLongLivedTokenUntilJWTExpiry(t *testing.T) {
	sessions := newResumableUploadSessionRegistry(1, 1)
	now := time.Now()
	expires := jwt.NumericDate(now.Add(2 * time.Hour))
	token := &tokens.UploadPayload{
		Payload:  jwt.Payload{ExpirationTime: expires},
		UniqueId: fmt.Sprintf("%s-%d", t.Name(), now.UnixNano()),
	}

	require.True(t, sessions.bind(token, "server", "/file.txt"))
	sessions.retire(token.UniqueId, "server", "/file.txt")

	sessions.mu.Lock()
	consumed := sessions.sessions[token.UniqueId]
	sessions.mu.Unlock()
	require.True(t, consumed.consumed)
	require.Equal(t, "server", consumed.serverID)
	require.Empty(t, consumed.target)
	require.Equal(t, expires.Time, consumed.expires)
	require.False(t, sessions.bind(token, "server", "/file.txt"))
	require.False(t, sessions.bind(token, "server", "/other.txt"))
}

func TestResumableUploadSessionPerServerCapPreservesOtherServers(t *testing.T) {
	sessions := newResumableUploadSessionRegistry(3, 2)
	now := time.Now()
	newToken := func(name string) *tokens.UploadPayload {
		return &tokens.UploadPayload{
			Payload:  jwt.Payload{ExpirationTime: jwt.NumericDate(now.Add(time.Hour))},
			UniqueId: fmt.Sprintf("%s-%s-%d", t.Name(), name, now.UnixNano()),
		}
	}

	serverATombstone := newToken("a-1")
	require.True(t, sessions.bind(serverATombstone, "server-a", "/one.txt"))
	sessions.retire(serverATombstone.UniqueId, "server-a", "/one.txt")
	require.True(t, sessions.bind(newToken("a-2"), "server-a", "/two.txt"))
	require.False(t, sessions.bind(newToken("a-3"), "server-a", "/three.txt"), "active sessions and tombstones must share the per-server cap")
	require.True(t, sessions.bind(newToken("b-1"), "server-b", "/one.txt"), "another server must retain its own capacity")
	require.False(t, sessions.bind(newToken("b-2"), "server-b", "/two.txt"), "the global cap must still be enforced")
	require.Equal(t, 2, sessions.byServer["server-a"])
	require.Equal(t, 1, sessions.byServer["server-b"])
}

func TestResumableUploadAdmissionLimitsAndReleases(t *testing.T) {
	admissions := newResumableUploadAdmissionRegistry(3, 2, 1)

	releaseA, ok := admissions.acquire("server-a", "token-a")
	require.True(t, ok)
	_, ok = admissions.acquire("server-a", "token-a")
	require.False(t, ok, "the per-token limit must be enforced")

	releaseB, ok := admissions.acquire("server-a", "token-b")
	require.True(t, ok)
	_, ok = admissions.acquire("server-a", "token-c")
	require.False(t, ok, "the per-server limit must be enforced")

	releaseC, ok := admissions.acquire("server-b", "token-c")
	require.True(t, ok)
	_, ok = admissions.acquire("server-c", "token-d")
	require.False(t, ok, "the global limit must be enforced")

	releaseA()
	releaseA()
	releaseD, ok := admissions.acquire("server-c", "token-d")
	require.True(t, ok, "releasing a slot must permit a new upload")

	releaseB()
	releaseC()
	releaseD()
	require.Zero(t, admissions.global)
	require.Empty(t, admissions.byServer)
	require.Empty(t, admissions.byToken)
}

func TestResumableUploadPathLockDoesNotQueue(t *testing.T) {
	locks := resumableUploadLockRegistry{active: make(map[string]struct{})}
	release, ok := locks.acquire("server\x00/file.txt")
	require.True(t, ok)
	_, ok = locks.acquire("server\x00/file.txt")
	require.False(t, ok, "a second request for the same path must fail without waiting")

	release()
	release, ok = locks.acquire("server\x00/file.txt")
	require.True(t, ok)
	release()
}

func TestResumableUploadDeadlineReaderFailsClosedWhenUnsupported(t *testing.T) {
	reader := newResumableUploadDeadlineReader(httptest.NewRecorder(), bytes.NewBufferString("body"), time.Second)
	_, err := reader.Read(make([]byte, 1))
	require.ErrorIs(t, err, http.ErrNotSupported)
}

func TestResumableUploadIdleBodyTimesOutAndReleasesResources(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 20)
	token := signBetterFilesUploadToken(t, s.ID())
	previousActivity := saveResumableUploadActivity
	saveResumableUploadActivity = func(_ *server.Server, _, _, _, _ string) {}
	t.Cleanup(func() { saveResumableUploadActivity = previousActivity })
	timedHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := context.WithValue(request.Context(), resumableUploadIdleTimeoutContextKey{}, 50*time.Millisecond)
		handler.ServeHTTP(writer, request.WithContext(ctx))
	})
	testServer := httptest.NewServer(timedHandler)
	t.Cleanup(testServer.Close)

	serverURL, err := url.Parse(testServer.URL)
	require.NoError(t, err)
	connection, err := net.DialTimeout("tcp", serverURL.Host, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	require.NoError(t, connection.SetDeadline(time.Now().Add(2*time.Second)))

	query := url.Values{"token": {token}, "directory": {"/"}, "file": {"stalled.txt"}}
	requestTarget := "/upload/file?" + query.Encode()
	_, err = fmt.Fprintf(connection,
		"PATCH %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/offset+octet-stream\r\nUpload-Offset: 0\r\nUpload-Length: 5\r\nContent-Length: 5\r\nConnection: close\r\n\r\n",
		requestTarget,
		serverURL.Host,
	)
	require.NoError(t, err)
	_, err = connection.Write([]byte("he"))
	require.NoError(t, err)

	started := time.Now()
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPatch})
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusRequestTimeout, response.StatusCode)
	require.Less(t, time.Since(started), time.Second)

	require.Eventually(t, func() bool {
		uploadAdmissions.mu.Lock()
		admissionsReleased := uploadAdmissions.byServer[s.ID()] == 0
		uploadAdmissions.mu.Unlock()
		uploadLocks.mu.Lock()
		_, pathLocked := uploadLocks.active[s.ID()+"\x00/stalled.txt"]
		uploadLocks.mu.Unlock()
		return admissionsReleased && !pathLocked
	}, time.Second, 10*time.Millisecond)
	require.Empty(t, readServerHTTPFixture(t, s, "/stalled.txt"), "a timed-out partial chunk must be rolled back")

	retryRequest, err := http.NewRequest(http.MethodPatch, testServer.URL+requestTarget, bytes.NewBufferString("hello"))
	require.NoError(t, err)
	retryRequest.Header.Set("Content-Type", "application/offset+octet-stream")
	retryRequest.Header.Set("Upload-Offset", "0")
	retryRequest.Header.Set("Upload-Length", "5")
	retryRequest.Header.Set("Upload-Complete", "?1")
	retryResponse, err := testServer.Client().Do(retryRequest)
	require.NoError(t, err)
	t.Cleanup(func() { _ = retryResponse.Body.Close() })
	require.Equal(t, http.StatusOK, retryResponse.StatusCode)
	require.Equal(t, []byte("hello"), readServerHTTPFixture(t, s, "/stalled.txt"))
}
