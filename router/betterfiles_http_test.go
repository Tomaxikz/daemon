package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/router/tokens"
	"github.com/pterodactyl/wings/server"
)

func newBetterFilesHTTPServer(t *testing.T, uploadLimitMB, diskMB int64, ignored ...string) (*server.Manager, *server.Server, http.Handler) {
	t.Helper()
	root := t.TempDir()
	next, err := config.NewAtPath(filepath.Join(root, "config.yml"))
	require.NoError(t, err)
	next.AuthenticationToken = "better-files-http-test-token"
	next.Api.UploadLimit = uploadLimitMB
	next.System.Data = filepath.Join(root, "volumes")
	next.System.RootDirectory = root
	next.System.User.Uid = os.Getuid()
	next.System.User.Gid = os.Getgid()
	next.System.FileHistory.Enabled = false
	config.Set(next)

	serverUUID := uuid.NewString()
	settings, err := json.Marshal(map[string]interface{}{
		"uuid":      serverUUID,
		"build":     map[string]interface{}{"disk_space": diskMB},
		"container": map[string]interface{}{"image": "alpine:3.20"},
		"egg":       map[string]interface{}{"file_denylist": ignored},
	})
	require.NoError(t, err)
	manager := server.NewEmptyManager(nil)
	s, err := manager.InitServer(remote.ServerConfigurationResponse{Settings: settings})
	require.NoError(t, err)
	manager.Add(s)
	t.Cleanup(s.CtxCancel)
	return manager, s, Configure(manager, nil)
}

func signBetterFilesUploadToken(t *testing.T, serverUUID string) string {
	t.Helper()
	now := time.Now().Add(time.Second)
	payload := tokens.UploadPayload{
		Payload: jwt.Payload{
			IssuedAt:       jwt.NumericDate(now),
			ExpirationTime: jwt.NumericDate(now.Add(time.Hour)),
		},
		Scoped:     tokens.Scoped{Scope: string(tokens.FileUpload)},
		ServerUuid: serverUUID,
		UserUuid:   uuid.NewString(),
		UniqueId:   uuid.NewString(),
	}
	signed, err := jwt.Sign(payload, config.GetJwtAlgorithm())
	require.NoError(t, err)
	return string(signed)
}

func signBetterFilesDownloadToken(t *testing.T, serverUUID, filePath string) string {
	t.Helper()
	now := time.Now().Add(time.Second)
	payload := tokens.FilePayload{
		Payload: jwt.Payload{
			IssuedAt:       jwt.NumericDate(now),
			ExpirationTime: jwt.NumericDate(now.Add(time.Hour)),
		},
		Scoped:     tokens.Scoped{Scope: string(tokens.FileDownload)},
		FilePath:   filePath,
		ServerUuid: serverUUID,
		UserUuid:   uuid.NewString(),
		UniqueId:   uuid.NewString(),
	}
	signed, err := jwt.Sign(payload, config.GetJwtAlgorithm())
	require.NoError(t, err)
	return string(signed)
}

func performResumableRequest(handler http.Handler, method, token, directory, filename string, offset int64, length *int64, complete bool, body io.Reader) *httptest.ResponseRecorder {
	query := url.Values{"token": {token}, "directory": {directory}, "file": {filename}}
	request := httptest.NewRequest(method, "/upload/file?"+query.Encode(), body)
	if method == http.MethodPatch {
		request.Header.Set("Content-Type", "application/offset+octet-stream")
		request.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		if length != nil {
			request.Header.Set("Upload-Length", strconv.FormatInt(*length, 10))
		}
		if complete {
			request.Header.Set("Upload-Complete", "?1")
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(&deadlineResponseRecorder{ResponseRecorder: recorder}, request)
	return recorder
}

type deadlineResponseRecorder struct {
	*httptest.ResponseRecorder
}

func (*deadlineResponseRecorder) SetReadDeadline(time.Time) error {
	return nil
}

func readServerHTTPFixture(t *testing.T, s *server.Server, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.Filesystem().Path(), filepath.FromSlash(strings.TrimPrefix(name, "/"))))
	require.NoError(t, err)
	return data
}
