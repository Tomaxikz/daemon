package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/internal/models"
	"github.com/pterodactyl/wings/internal/networkpolicy"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

func TestConfigurationNetworkPolicy(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "nsm-config-test"})
	fixture, err := os.ReadFile("../internal/networkpolicy/testdata/shared.json")
	require.NoError(t, err)
	settings := func(policy json.RawMessage, ip string) json.RawMessage {
		data, err := json.Marshal(map[string]interface{}{
			"uuid":           "12345678-1234-4234-8234-123456789abc",
			"allocations":    map[string]interface{}{"mappings": map[string][]int{ip: {25565}}},
			"network_policy": policy,
		})
		require.NoError(t, err)
		return data
	}

	s := &Server{}
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: settings(fixture, "192.0.2.1")}))
	require.NotNil(t, s.cfg.NetworkPolicy)
	require.Equal(t, uint64(1), s.cfg.NetworkPolicy.Revision)
	require.Equal(t, "667d839d9a720983bb7e5ad84066a4f559577bbfbd53c3a1dafe93b44a8581de", s.cfg.NetworkPolicy.PolicyHash)

	for _, policy := range []json.RawMessage{
		[]byte(`{}`),
		[]byte(`{"api_version":1}`),
		[]byte(`[]`),
		[]byte(strings.Replace(string(fixture), `"chain_id"`, `"unsupported"`, 1)),
	} {
		require.Error(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: settings(policy, "192.0.2.1")}))
		require.Equal(t, uint64(1), s.cfg.NetworkPolicy.Revision, "invalid sync must not replace configuration")
	}

	require.Error(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: settings(fixture, "192.0.2.2")}))

	for _, data := range []json.RawMessage{
		[]byte(`{"uuid":"12345678-1234-4234-8234-123456789abc"}`),
		settings(nil, "192.0.2.1"),
	} {
		fresh := &Server{}
		require.NoError(t, fresh.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: data}))
		require.Nil(t, fresh.cfg.NetworkPolicy, "unconfigured servers retain the stock contract")
	}
}

const policySyncTestID = "12345678-1234-4234-8234-123456789abc"

type policyTestEnvironment struct {
	config      *environment.Configuration
	rejected    chan error
	setPolicies chan *networkpolicy.Policy
	setErr      error
}

func newPolicyTestEnvironment() *policyTestEnvironment {
	return &policyTestEnvironment{
		config:      environment.NewConfiguration(environment.Settings{}, nil),
		rejected:    make(chan error, 4),
		setPolicies: make(chan *networkpolicy.Policy, 4),
	}
}

func (e *policyTestEnvironment) SetNetworkSettings(
	_ context.Context,
	settings environment.Settings,
	policy *networkpolicy.Policy,
) error {
	e.setPolicies <- policy
	if e.setErr != nil {
		return e.setErr
	}

	e.config.SetSettings(settings)
	return nil
}

func (e *policyTestEnvironment) RejectNetworkPolicy(_ context.Context, cause error) error {
	e.rejected <- cause
	return cause
}

func (e *policyTestEnvironment) Type() string {
	return "test"
}

func (e *policyTestEnvironment) Config() *environment.Configuration {
	return e.config
}

func (e *policyTestEnvironment) Events() *events.Bus {
	return events.NewBus()
}

func (e *policyTestEnvironment) Exists() (bool, error) {
	return true, nil
}

func (e *policyTestEnvironment) IsRunning(context.Context) (bool, error) {
	return false, nil
}

func (e *policyTestEnvironment) InSituUpdate() error {
	return nil
}

func (e *policyTestEnvironment) OnBeforeStart(context.Context) error {
	return nil
}

func (e *policyTestEnvironment) Start(context.Context) error {
	return nil
}

func (e *policyTestEnvironment) Stop(context.Context) error {
	return nil
}

func (e *policyTestEnvironment) WaitForStop(context.Context, time.Duration, bool) error {
	return nil
}

func (e *policyTestEnvironment) Terminate(context.Context, string) error {
	return nil
}

func (e *policyTestEnvironment) Destroy() error {
	return nil
}

func (e *policyTestEnvironment) ExitState() (uint32, bool, error) {
	return 0, false, nil
}

func (e *policyTestEnvironment) Create() error {
	return nil
}

func (e *policyTestEnvironment) Attach(context.Context) error {
	return nil
}

func (e *policyTestEnvironment) SendCommand(string) error {
	return nil
}

func (e *policyTestEnvironment) Readlog(int) ([]string, error) {
	return nil, nil
}

func (e *policyTestEnvironment) State() string {
	return environment.ProcessOfflineState
}

func (e *policyTestEnvironment) SetState(string) {}

func (e *policyTestEnvironment) Uptime(context.Context) (int64, error) {
	return 0, nil
}

func (e *policyTestEnvironment) SetLogCallback(func([]byte)) {}

func TestInvalidLivePolicyFailsClosedWithoutPublishingConfiguration(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "nsm-live-test"})
	valid := policySettings(t, policySyncTestID, sharedPolicyRevision(t, 1))
	s := &Server{}
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: valid}))

	env := newPolicyTestEnvironment()
	s.Environment = env
	invalid := policySettings(t, policySyncTestID, json.RawMessage(`{}`))
	require.Error(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: invalid}))
	require.Equal(t, uint64(1), s.cfg.NetworkPolicy.Revision)

	select {
	case err := <-env.rejected:
		require.ErrorContains(t, err, "network_policy")
	default:
		t.Fatal("invalid live policy was not sent to fail-closed handling")
	}
}

func TestEnvironmentRejectionDoesNotCommitStagedConfiguration(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "nsm-reject-test"})
	s := &Server{}
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{
		Settings: policySettings(t, policySyncTestID, sharedPolicyRevision(t, 1)),
	}))

	env := newPolicyTestEnvironment()
	env.setErr = errors.New("stale policy")
	s.Environment = env
	err := s.SyncWithConfiguration(remote.ServerConfigurationResponse{
		Settings: policySettings(t, policySyncTestID, sharedPolicyRevision(t, 2)),
	})
	require.ErrorContains(t, err, "stale policy")
	require.Equal(t, uint64(1), s.cfg.NetworkPolicy.Revision)
}

func TestInitialInvalidPolicyUsesTrustedServerID(t *testing.T) {
	cfg := &config.Configuration{AuthenticationToken: "nsm-initial-test"}
	cfg.System.RootDirectory = t.TempDir()
	config.Set(cfg)
	m := NewEmptyManager(nil)
	var rejectedID string
	m.rejectInitial = func(_ context.Context, id string, _ *Configuration, cause error) error {
		rejectedID = id
		return cause
	}

	_, err := m.initServer(policySyncTestID, remote.ServerConfigurationResponse{
		Settings: policySettings(t, policySyncTestID, json.RawMessage(`{}`)),
	})
	require.Error(t, err)
	require.Equal(t, policySyncTestID, rejectedID)

	rejectedID = ""
	_, err = m.initServer(policySyncTestID, remote.ServerConfigurationResponse{
		Settings: policySettings(t, "22345678-1234-4234-8234-123456789abc", json.RawMessage(`{}`)),
	})
	require.ErrorContains(t, err, "trusted server UUID")
	require.Empty(t, rejectedID, "an untrusted settings UUID must never select a quarantine target")

	var durable networkpolicy.Policy
	require.NoError(t, networkpolicy.Decode(sharedPolicyRevision(t, 1), &durable))
	require.NoError(t, networkpolicy.Save(cfg.System.RootDirectory, policySyncTestID, durable))
	rejectedID = ""
	_, err = m.initServer(policySyncTestID, remote.ServerConfigurationResponse{Settings: json.RawMessage(`{"uuid":`)})
	require.Error(t, err)
	require.Equal(t, policySyncTestID, rejectedID, "malformed settings must quarantine the trusted server when durable restrictions exist")

	rejectedID = ""
	_, err = m.initServer(policySyncTestID, remote.ServerConfigurationResponse{
		Settings: policySettings(t, "22345678-1234-4234-8234-123456789abc", nil),
	})
	require.Error(t, err)
	require.Equal(t, policySyncTestID, rejectedID, "durable-policy quarantine must use the trusted list UUID, never the mismatched settings UUID")
}

func TestSyncSerializesFetchThroughConfigurationCommit(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "nsm-sync-test"})
	client := &serialSyncClient{
		responses: []remote.ServerConfigurationResponse{
			{Settings: policySettings(t, policySyncTestID, sharedPolicyRevision(t, 1))},
			{Settings: policySettings(t, policySyncTestID, sharedPolicyRevision(t, 2))},
		},
		firstStarted:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
	s, err := New(client)
	require.NoError(t, err)
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{
		Settings: policySettings(t, policySyncTestID, sharedPolicyRevision(t, 1)),
	}))
	s.fs, err = filesystem.New(filepath.Join(t.TempDir(), policySyncTestID), 0, nil)
	require.NoError(t, err)
	s.Environment = newPolicyTestEnvironment()

	results := make(chan error, 2)
	go func() {
		results <- s.Sync()
	}()
	<-client.firstStarted
	go func() {
		results <- s.Sync()
	}()
	select {
	case <-client.secondStarted:
		t.Fatal("second fetch began before the first sync transaction committed")
	case <-time.After(50 * time.Millisecond):
	}

	close(client.releaseFirst)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.Equal(t, uint64(2), s.cfg.NetworkPolicy.Revision)
}

func TestManagerInitAbortsBeforePublishingInvalidNSMServer(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "nsm-manager-test"})
	client := &serialSyncClient{
		servers: []remote.RawServerData{
			{
				Uuid:                 policySyncTestID,
				Settings:             policySettings(t, policySyncTestID, sharedPolicyRevision(t, 1)),
				ProcessConfiguration: json.RawMessage(`{`),
			},
		},
	}
	m := NewEmptyManager(client)
	rejected := make(chan string, 1)
	m.rejectInitial = func(_ context.Context, id string, _ *Configuration, cause error) error {
		rejected <- id
		return cause
	}

	err := m.init(context.Background())
	require.ErrorContains(t, err, "initial network policy rejected")
	require.Equal(t, policySyncTestID, <-rejected)
	require.Zero(t, m.Len())
}

func sharedPolicyRevision(t *testing.T, revision uint64) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("../internal/networkpolicy/testdata/shared.json")
	require.NoError(t, err)
	var policy networkpolicy.Policy
	require.NoError(t, networkpolicy.Decode(data, &policy))
	policy.Revision = revision
	policy.PolicyHash, err = policy.CalculatedHash()
	require.NoError(t, err)
	data, err = json.Marshal(policy)
	require.NoError(t, err)
	return data
}

func policySettings(t *testing.T, id string, policy json.RawMessage) json.RawMessage {
	t.Helper()
	settings := map[string]any{
		"uuid":        id,
		"allocations": map[string]any{"mappings": map[string][]int{"192.0.2.1": {25565}}},
	}
	if policy != nil {
		settings["network_policy"] = policy
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	return data
}

type serialSyncClient struct {
	mu            sync.Mutex
	responses     []remote.ServerConfigurationResponse
	firstStarted  chan struct{}
	secondStarted chan struct{}
	releaseFirst  chan struct{}
	calls         int
	servers       []remote.RawServerData
}

func (c *serialSyncClient) GetServerConfiguration(_ context.Context, id string) (remote.ServerConfigurationResponse, error) {
	if id != policySyncTestID {
		return remote.ServerConfigurationResponse{}, errors.New("unexpected server ID")
	}

	c.mu.Lock()
	call := c.calls
	c.calls++
	response := c.responses[call]
	c.mu.Unlock()
	if call == 0 {
		close(c.firstStarted)
		<-c.releaseFirst
	} else if call == 1 {
		close(c.secondStarted)
	}
	return response, nil
}

func (c *serialSyncClient) GetBackupRemoteUploadURLs(
	context.Context,
	string,
	int64,
) (remote.BackupRemoteUploadResponse, error) {
	return remote.BackupRemoteUploadResponse{}, nil
}

func (c *serialSyncClient) GetInstallationScript(context.Context, string) (remote.InstallationScript, error) {
	return remote.InstallationScript{}, nil
}

func (c *serialSyncClient) GetServers(context.Context, int) ([]remote.RawServerData, error) {
	return c.servers, nil
}

func (c *serialSyncClient) ResetServersState(context.Context) error {
	return nil
}

func (c *serialSyncClient) SetArchiveStatus(context.Context, string, bool) error {
	return nil
}

func (c *serialSyncClient) SetBackupStatus(context.Context, string, remote.BackupRequest) error {
	return nil
}

func (c *serialSyncClient) SendRestorationStatus(context.Context, string, bool) error {
	return nil
}

func (c *serialSyncClient) SetInstallationStatus(
	context.Context,
	string,
	remote.InstallStatusRequest,
) error {
	return nil
}

func (c *serialSyncClient) SetTransferStatus(context.Context, string, bool) error {
	return nil
}

func (c *serialSyncClient) ValidateSftpCredentials(context.Context, remote.SftpAuthRequest) (remote.SftpAuthResponse, error) {
	return remote.SftpAuthResponse{}, nil
}

func (c *serialSyncClient) SendActivityLogs(context.Context, []models.Activity) error {
	return nil
}

func (c *serialSyncClient) SetCredentials(string, string) {}
