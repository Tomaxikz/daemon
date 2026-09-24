package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/networkpolicy"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server"
)

func TestNetworkPolicyRoutesAuthorizationAndContract(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.NewAtPath(filepath.Join(root, "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationToken = "nsm-test-token"
	cfg.System.RootDirectory = root
	cfg.System.Data = filepath.Join(root, "volumes")
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	cfg.System.DiskCheckInterval = 0
	cfg.Docker.NetworkPolicy.Enabled = false
	config.Set(cfg)

	id := uuid.NewString()
	settings, err := json.Marshal(map[string]interface{}{
		"uuid":      id,
		"container": map[string]string{"image": "alpine:3.20"},
		"allocations": map[string]interface{}{
			"mappings": map[string][]int{"192.0.2.1": {25565}},
		},
	})
	require.NoError(t, err)

	m := server.NewEmptyManager(nil)
	s, err := m.InitServer(remote.ServerConfigurationResponse{Settings: settings})
	require.NoError(t, err)
	m.Add(s)
	t.Cleanup(s.CtxCancel)
	handler := Configure(m, nil)

	fixture, err := os.ReadFile("../internal/networkpolicy/testdata/shared.json")
	require.NoError(t, err)
	for _, test := range []struct {
		name, method, path, body string
		auth                     bool
		status                   int
	}{
		{"read requires auth", "GET", "/network-policy", "", false, 401},
		{"write requires auth", "PUT", "/network-policy", string(fixture), false, 401},
		{"capabilities require auth", "GET", "/network-policy/capabilities", "", false, 401},
		{"initial status", "GET", "/network-policy", "", true, 200},
		{"disabled backend", "GET", "/network-policy/capabilities", "", true, 501},
		{"old incompatible envelope", "PUT", "/network-policy", `{"expected_revision":0,"policy":{}}`, true, 400},
		{"incomplete envelope", "PUT", "/network-policy", `{"api_version":1,"policy":{}}`, true, 400},
		{"null envelope", "PUT", "/network-policy", "null", true, 400},
		{"oversized envelope", "PUT", "/network-policy", strings.Repeat(" ", networkpolicy.MaxBody+1), true, 413},
		{"valid policy unsupported backend", "PUT", "/network-policy", string(fixture), true, 501},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/api/servers/"+id+test.path, strings.NewReader(test.body))
			if test.auth {
				request.Header.Set("Authorization", "Bearer "+config.Get().Token.Token)
			}
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, request)
			require.Equal(t, test.status, out.Code, out.Body.String())
			require.Contains(t, out.Header().Get("Content-Type"), "application/json")
			if test.name == "initial status" {
				require.JSONEq(t, `{"api_version":1,"state":"pending","applied_revision":0,"applied_hash":null}`, out.Body.String())
			}
		})
	}

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		request := httptest.NewRequest(method, "/api/servers/"+uuid.NewString()+"/network-policy", strings.NewReader(string(fixture)))
		request.Header.Set("Authorization", "Bearer "+config.Get().Token.Token)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, request)
		require.Equal(t, 404, out.Code)
	}

	other, err := m.InitServer(remote.ServerConfigurationResponse{
		Settings: []byte(`{"uuid":"22345678-1234-4234-8234-123456789abc","allocations":{"mappings":{"192.0.2.2":[25566]}}}`),
	})
	require.NoError(t, err)
	m.Add(other)
	t.Cleanup(other.CtxCancel)
	request := httptest.NewRequest(http.MethodPut, "/api/servers/"+other.ID()+"/network-policy", strings.NewReader(string(fixture)))
	request.Header.Set("Authorization", "Bearer "+config.Get().Token.Token)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, request)
	require.Equal(t, http.StatusUnprocessableEntity, out.Code, out.Body.String())

	found := false
	for _, route := range handler.Routes() {
		if route.Method == "GET" && route.Path == "/api/servers/:server/stats/protocols" {
			found = true
		}
	}

	require.True(t, found, "legacy NSM statistics route must remain registered")
}
