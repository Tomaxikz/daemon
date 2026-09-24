package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/internal/networkpolicy"
	"github.com/pterodactyl/wings/system"
)

const networkTestID = "12345678-1234-4234-8234-123456789abc"

func TestNetworkStatusCancellationDoesNotKill(t *testing.T) {
	for _, test := range []struct {
		name     string
		deadline bool
		drift    bool
	}{
		{name: "caller cancellation"},
		{name: "verification deadline", deadline: true},
		{name: "genuine topology drift still fails closed", drift: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var inspections, kills atomic.Int32
			name, _, _, err := networkpolicy.Names(networkTestID)
			require.NoError(t, err)
			e := networkTestEnvironment(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/json"):
					labels := map[string]string{"Service": "Pterodactyl", "ContainerType": "server_process"}
					if !test.drift {
						labels[networkOwner] = networkTestID
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(container.InspectResponse{
						ContainerJSONBase: &container.ContainerJSONBase{
							ID: "status-container", Name: "/" + networkTestID,
							State:      &container.State{Running: inspections.Add(1) == 1},
							HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(name)},
						},
						Config: &container.Config{Labels: labels},
					})
				case strings.Contains(r.URL.Path, "/networks/"):
					if !test.deadline {
						cancel()
					}
					<-r.Context().Done()
				case strings.HasSuffix(r.URL.Path, "/kill"):
					kills.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			e.Configuration.SetSettings(assignedSettings())
			p := testPolicy(t, 1, 1)
			p.Body.Bandwidth.Upload = 8_000
			p.PolicyHash, err = p.CalculatedHash()
			require.NoError(t, err)
			require.NoError(t, networkpolicy.Save(config.Get().System.RootDirectory, e.Id, p))
			e.storeNetworkStatus(e.appliedNetworkStatus(p, "status-container"))

			status, err := e.NetworkPolicy(ctx)
			require.NoError(t, err)
			if test.drift {
				require.Equal(t, "failed", status.State)
				require.Equal(t, int32(1), kills.Load())
			} else {
				require.Equal(t, "pending", status.State)
				require.Eventually(t, func() bool { return !e.networkReconcile.Load() }, time.Second, time.Millisecond)
				require.GreaterOrEqual(t, inspections.Load(), int32(2), "independent worker must recheck")
				require.Zero(t, kills.Load(), "cancellation alone must never kill the server")
			}
		})
	}
}

type networkTestTransport func(*http.Request) (*http.Response, error)

func (f networkTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestQueuedNetworkCheckGetsFreshBudgetAndAvoidsMutations(t *testing.T) {
	e := networkTestEnvironment(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.True(t, strings.HasSuffix(r.URL.Path, "/json"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "healthy", State: &container.State{Running: true}},
		})
	})
	e.Configuration.SetSettings(assignedSettings())
	p := testPolicy(t, 1, 1)
	require.NoError(t, networkpolicy.Save(config.Get().System.RootDirectory, e.Id, p))
	e.networkContainerID = "healthy"
	e.networkReconcile.Store(true)
	budget := make(chan time.Duration, 1)
	base := e.client.HTTPClient().Transport
	if base == nil {
		base = http.DefaultTransport
	}
	transport := networkTestTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if ok {
			budget <- time.Until(deadline)
		}
		return base.RoundTrip(r)
	})
	cli, err := client.NewClientWithOpts(client.WithHost(e.client.DaemonHost()), client.WithVersion("1.48"),
		client.WithHTTPClient(&http.Client{Transport: transport}))
	require.NoError(t, err)
	e.client = cli
	t.Cleanup(func() { _ = cli.Close() })

	e.networkMu.Lock()
	locked := true
	defer func() {
		if locked {
			e.networkMu.Unlock()
		}
	}()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { e.reconcilePendingNetwork(waitCtx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	e.networkMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("queued check did not finish")
	}
	select {
	case remaining := <-budget:
		require.Greater(t, remaining, 9*time.Second, "verification must not inherit the lock-wait deadline")
	default:
		t.Fatal("verification did not make a bounded Docker request")
	}
	require.Equal(t, "applied", e.networkStatusSnapshot().State)
	require.False(t, e.networkReconcile.Load())
}

func TestCanceledNetworkWorkDoesNotAcquireLockOrInspect(t *testing.T) {
	e := networkTestEnvironment(t, func(http.ResponseWriter, *http.Request) { t.Error("canceled work queried Docker") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.networkMu.Lock()
	defer e.networkMu.Unlock()
	e.networkReconcile.Store(true)
	e.reconcilePendingNetwork(ctx)
	require.False(t, e.networkReconcile.Load())
	_, err := e.NetworkPolicy(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, e.ReconcileNetwork(ctx), context.Canceled)
	require.ErrorIs(t, e.reconcileNetwork(ctx), context.Canceled)
}

func TestRuntimeBindingFollowsAcceptedPolicy(t *testing.T) {
	e := networkTestEnvironment(t, dockerNotFound)
	cfg := config.Get()
	cfg.Docker.NetworkPolicy.Runtime = "wings-nsm"
	config.Set(cfg)
	ctx := context.Background()
	settings := assignedSettings()
	p := testPolicy(t, 1, 1)
	require.NoError(t, e.SetNetworkSettings(ctx, settings, &p))

	host := &container.HostConfig{
		Runtime: "wings-nsm",
		Annotations: map[string]string{
			networkpolicy.RuntimeServerKey:     e.Id,
			networkpolicy.RuntimeGenerationKey: "abcdefab-1234-4234-8234-123456789abc",
		},
	}
	id := strings.Repeat("a", 64)
	require.NoError(t, e.bindNetworkRuntime(ctx, id, host))
	c := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: id, HostConfig: host},
	}
	require.NoError(t, e.verifyRuntimeContainer(p, c))

	next := testPolicy(t, 2, 1)
	require.NoError(t, e.SetNetworkSettings(ctx, settings, &next))
	binding, err := networkpolicy.LoadRuntimeBinding(cfg.System.RootDirectory, e.Id)
	require.NoError(t, err)
	require.Equal(t, next.Revision, binding.Revision)
	require.Equal(t, next.PolicyHash, binding.PolicyHash)
	require.Equal(t, settings.Allocations.Mappings, binding.Allocations)
	require.NoError(t, e.verifyRuntimeContainer(next, c))
	require.Error(t, e.verifyRuntimeContainer(p, c))

	// Retry repairs an interrupted policy/binding write without a new revision.
	binding.Revision, binding.PolicyHash = p.Revision, p.PolicyHash
	require.NoError(t, networkpolicy.SaveRuntimeBinding(cfg.System.RootDirectory, binding))
	require.NoError(t, e.SetNetworkSettings(ctx, settings, &next))
	require.NoError(t, e.verifyRuntimeContainer(next, c))

	c.ID = strings.Repeat("b", 64)
	require.Error(t, e.verifyRuntimeContainer(next, c))
	c.ID = id
	host.Annotations[networkpolicy.RuntimeGenerationKey] = "abcdefab-1234-4234-8234-123456789abd"
	require.Error(t, e.verifyRuntimeContainer(next, c))
	host.Runtime = "runc"
	require.ErrorContains(t, e.verifyRuntimeContainer(next, c), "recreating")

	require.NoError(t, e.removeNetworkState(ctx))
	_, err = networkpolicy.LoadRuntimeBinding(cfg.System.RootDirectory, e.Id)
	require.True(t, os.IsNotExist(err), "%v", err)
	loaded, err := networkpolicy.Load(cfg.System.RootDirectory, e.Id)
	require.NoError(t, err)
	require.Equal(t, uint64(0), loaded.Revision)
}

func TestRuntimeBindingRequiresLeaseBeforePolicyWrite(t *testing.T) {
	e := networkTestEnvironment(t, dockerNotFound)
	cfg := config.Get()
	cfg.Docker.NetworkPolicy.Runtime = "wings-nsm"
	config.Set(cfg)
	lease, err := networkpolicy.LockRuntime(context.Background(), cfg.System.RootDirectory, e.Id)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, e.saveNetworkPolicy(ctx, testPolicy(t, 1, 1), true), context.DeadlineExceeded)
	loaded, err := networkpolicy.Load(cfg.System.RootDirectory, e.Id)
	require.NoError(t, err)
	require.Zero(t, loaded.Revision)
}

func networkTestEnvironment(t *testing.T, handler http.HandlerFunc) *Environment {
	t.Helper()
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.System.RootDirectory = t.TempDir()
	cfg.AuthenticationToken = "nsm-network-test-token"
	cfg.Docker.UsePerformantInspect = false
	config.Set(cfg)

	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	cli, err := client.NewClientWithOpts(client.WithHost(api.URL), client.WithVersion("1.48"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, cli.Close())
	})
	e := &Environment{
		Id:            networkTestID,
		client:        cli,
		Configuration: environment.NewConfiguration(environment.Settings{}, nil),
		st:            system.NewAtomicString(environment.ProcessOfflineState),
		emitter:       events.NewBus(),
	}
	t.Cleanup(func() {
		require.Eventually(t, func() bool {
			return !e.networkReconcile.Load()
		}, time.Second, 5*time.Millisecond)
	})
	return e
}

func testPolicy(t *testing.T, revision uint64, allocationID int64) networkpolicy.Policy {
	t.Helper()
	p := networkpolicy.Policy{
		APIVersion: 1,
		Revision:   revision,
		Allocations: []networkpolicy.Allocation{
			{
				ID:   allocationID,
				IP:   "192.0.2.1",
				Port: 25565,
			},
		},
		ResolvedGroups: []networkpolicy.ResolvedGroup{},
		Body: networkpolicy.Rules{
			Groups: []networkpolicy.Group{},
			Firewall: networkpolicy.Firewall{
				DefaultInbound:  "allow",
				DefaultOutbound: "allow",
				Chains:          []networkpolicy.Chain{},
				Rules:           []networkpolicy.Rule{},
			},
			Bandwidth: networkpolicy.Bandwidth{},
		},
	}
	hash, err := p.CalculatedHash()
	require.NoError(t, err)
	p.PolicyHash = hash
	return p
}

func assignedSettings() environment.Settings {
	return environment.Settings{
		Allocations: environment.Allocations{
			Mappings: map[string][]int{"192.0.2.1": {25565}},
		},
	}
}

func dockerNotFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"missing"}`))
}

func TestNetworkPolicyBeforeConfiguration(t *testing.T) {
	e := networkTestEnvironment(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("an unconfigured status read must not call Docker")
	})
	status, err := e.NetworkPolicy(context.Background())
	require.NoError(t, err)
	require.Equal(t, networkpolicy.Status{APIVersion: 1, State: "pending"}, status)
	encoded, err := json.Marshal(status)
	require.NoError(t, err)
	require.JSONEq(t, `{"api_version":1,"state":"pending","applied_revision":0,"applied_hash":null}`, string(encoded))
}

func TestExplicitlyDisabledNetworkPolicyReportsSetting(t *testing.T) {
	e := networkTestEnvironment(t, func(http.ResponseWriter, *http.Request) {
		t.Error("disabled policies must not query Docker")
	})
	config.Update(func(cfg *config.Configuration) {
		cfg.Docker.NetworkPolicy.Enabled = false
	})
	err := e.checkPolicySupport(context.Background(), networkpolicy.Empty(), networkpolicy.Empty(), environment.Allocations{})
	require.ErrorIs(t, err, ErrNetworkUnsupported)
	require.ErrorContains(t, err, "docker.network_policy.enabled is false")
}

func TestSetNetworkSettingsWithoutPolicyKeepsStockPath(t *testing.T) {
	e := networkTestEnvironment(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("a nil policy with no durable policy must not call Docker")
	})
	settings := assignedSettings()
	require.NoError(t, e.SetNetworkSettings(context.Background(), settings, nil))
	require.Equal(t, settings.Allocations.Mappings, e.Configuration.Allocations().Mappings)
}

func TestUnconfiguredPolicyWithAdminCeilingRemainsPending(t *testing.T) {
	e := networkTestEnvironment(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("an offline unconfigured status read must not call Docker")
	})
	e.Configuration.SetSettings(assignedSettings())
	cfg := config.Get()
	cfg.Docker.NetworkPolicy.MaxUploadBPS = 1_000_000
	config.Set(cfg)

	status, err := e.NetworkPolicy(context.Background())
	require.NoError(t, err)
	require.Equal(t, "pending", status.State)
	require.Zero(t, status.AppliedRevision)
	require.Nil(t, status.AppliedHash)
	verified := e.appliedNetworkStatus(networkpolicy.Empty(), "docker-id")
	require.Equal(t, "pending", verified.State)
	require.True(t, e.networkVerified.Load())
}

func TestNetworkPolicyRevisionRules(t *testing.T) {
	e := networkTestEnvironment(t, dockerNotFound)
	owned := assignedSettings().Allocations.Mappings
	old := testPolicy(t, 2, 1)

	lower := testPolicy(t, 1, 1)
	require.ErrorIs(t, e.validatePolicyUpdate(old, lower, owned), ErrNetworkConflict)

	changedRules := old
	changedRules.Body.Firewall.DefaultInbound = "drop"
	hash, err := changedRules.CalculatedHash()
	require.NoError(t, err)
	changedRules.PolicyHash = hash
	require.ErrorIs(t, e.validatePolicyUpdate(old, changedRules, owned), ErrNetworkConflict)

	changedAllocationIdentity := testPolicy(t, 2, 9)
	require.NoError(t, e.validatePolicyUpdate(old, changedAllocationIdentity, owned))
}

func TestUpdateRejectsUnrepresentableBandwidthBeforePersistence(t *testing.T) {
	e := networkTestEnvironment(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("backend-invalid policy must be rejected before Docker access")
	})
	e.Configuration.SetSettings(assignedSettings())
	p := testPolicy(t, 1, 1)
	p.Body.Bandwidth.Upload = 1
	hash, err := p.CalculatedHash()
	require.NoError(t, err)
	p.PolicyHash = hash

	_, err = e.UpdateNetworkPolicy(context.Background(), p)
	require.ErrorIs(t, err, ErrNetworkInvalid)
	stored, loadErr := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
	require.NoError(t, loadErr)
	require.Zero(t, stored.Revision)
}

func TestUpdateNetworkPolicyPersistsAndRejectsStaleRevision(t *testing.T) {
	e := networkTestEnvironment(t, dockerNotFound)
	e.Configuration.SetSettings(assignedSettings())
	cfg := config.Get()
	cfg.Docker.NetworkPolicy.Enabled = true
	config.Set(cfg)

	desired := testPolicy(t, 2, 1)
	status, err := e.UpdateNetworkPolicy(context.Background(), desired)
	require.NoError(t, err)
	require.Equal(t, "pending", status.State)
	require.Equal(t, desired, status.Desired)

	_, err = e.UpdateNetworkPolicy(context.Background(), testPolicy(t, 1, 1))
	require.ErrorIs(t, err, ErrNetworkConflict)
	stored, err := networkpolicy.Load(cfg.System.RootDirectory, e.Id)
	require.NoError(t, err)
	require.Equal(t, desired, stored)

	staleSettings := environment.Settings{
		Allocations: environment.Allocations{
			Mappings: map[string][]int{"198.51.100.9": {19132}},
		},
	}
	stale := testPolicy(t, 1, 1)
	err = e.SetNetworkSettings(context.Background(), staleSettings, &stale)
	require.ErrorIs(t, err, ErrNetworkConflict)
	require.Equal(t, assignedSettings().Allocations.Mappings, e.Configuration.Allocations().Mappings)
}

func TestConcurrentAllocationOnlyUpdatesAreSerialized(t *testing.T) {
	e := networkTestEnvironment(t, dockerNotFound)
	e.Configuration.SetSettings(assignedSettings())
	cfg := config.Get()
	cfg.Docker.NetworkPolicy.Enabled = true
	config.Set(cfg)
	_, err := e.UpdateNetworkPolicy(context.Background(), testPolicy(t, 4, 1))
	require.NoError(t, err)

	policies := []networkpolicy.Policy{testPolicy(t, 4, 2), testPolicy(t, 4, 3)}
	var wg sync.WaitGroup
	errs := make(chan error, len(policies))
	for _, policy := range policies {
		wg.Add(1)
		go func(p networkpolicy.Policy) {
			defer wg.Done()

			_, err := e.UpdateNetworkPolicy(context.Background(), p)
			errs <- err
		}(policy)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	stored, err := networkpolicy.Load(cfg.System.RootDirectory, e.Id)
	require.NoError(t, err)
	require.Contains(t, []string{policies[0].PolicyHash, policies[1].PolicyHash}, stored.PolicyHash)
}

func TestSameHashHealthyPolicyDoesNotScheduleReconciliation(t *testing.T) {
	var mu sync.Mutex
	inspections := 0
	e := networkTestEnvironment(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inspections++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{
				ID:    "docker-id",
				Name:  "/" + networkTestID,
				State: &container.State{Running: true},
			},
			Config: &container.Config{
				Labels: map[string]string{
					"Service": "Pterodactyl", "ContainerType": "server_process",
				},
			},
		}))
	})
	e.Configuration.SetSettings(assignedSettings())
	cfg := config.Get()
	cfg.Docker.NetworkPolicy.Enabled = true
	config.Set(cfg)
	p := testPolicy(t, 6, 1)
	require.NoError(t, networkpolicy.Save(cfg.System.RootDirectory, e.Id, p))
	e.storeNetworkStatus(e.appliedNetworkStatus(p, "docker-id"))

	status, err := e.UpdateNetworkPolicy(context.Background(), p)
	require.NoError(t, err)
	require.Equal(t, "applied", status.State)
	require.False(t, e.networkReconcile.Load())
	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, 2, inspections)
}

func TestNetworkStatusDoesNotWaitForReconciliation(t *testing.T) {
	e := networkTestEnvironment(t, dockerNotFound)
	p := testPolicy(t, 7, 1)
	e.storeNetworkStatus(e.pendingNetworkStatus(p))
	e.networkMu.Lock()
	defer e.networkMu.Unlock()

	started := time.Now()
	status, err := e.NetworkPolicy(context.Background())
	require.NoError(t, err)
	require.Less(t, time.Since(started), 100*time.Millisecond)
	require.Equal(t, uint64(7), status.Desired.Revision)
}

func TestInvalidSyncedPolicyStopsRunningContainer(t *testing.T) {
	var mu sync.Mutex
	var killed bool
	e := networkTestEnvironment(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1.48/containers/"+networkTestID+"/kill" {
			mu.Lock()
			killed = true
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}

		require.NoError(t, json.NewEncoder(w).Encode(container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{
				ID:    "docker-id",
				Name:  "/" + networkTestID,
				State: &container.State{Running: true},
			},
			Config: &container.Config{
				Labels: map[string]string{
					"Service": "Pterodactyl", "ContainerType": "server_process",
				},
			},
		}))
	})
	invalid := testPolicy(t, 1, 1)
	invalid.PolicyHash = "invalid"
	err := e.SetNetworkSettings(context.Background(), assignedSettings(), &invalid)
	require.ErrorIs(t, err, ErrNetworkInvalid)
	mu.Lock()
	defer mu.Unlock()

	require.True(t, killed)
	require.Equal(t, "failed", e.networkStatusSnapshot().State)
	require.Empty(t, e.Configuration.Allocations().Mappings)
}

func TestRejectedSyncedPolicyDoesNotPublishStagedSettings(t *testing.T) {
	e := networkTestEnvironment(t, dockerNotFound)
	oldSettings := assignedSettings()
	e.Configuration.SetSettings(oldSettings)
	staged := environment.Settings{
		Allocations: environment.Allocations{
			Mappings: map[string][]int{"198.51.100.9": {19132}},
		},
	}
	p := networkpolicy.Policy{
		APIVersion: 1,
		Revision:   1,
		Allocations: []networkpolicy.Allocation{
			{
				ID:   2,
				IP:   "198.51.100.9",
				Port: 19132,
			},
		},
		ResolvedGroups: []networkpolicy.ResolvedGroup{},
		Body: networkpolicy.Rules{
			Groups: []networkpolicy.Group{},
			Firewall: networkpolicy.Firewall{
				Enabled:         true,
				DefaultInbound:  "drop",
				DefaultOutbound: "allow",
				Chains:          []networkpolicy.Chain{},
				Rules:           []networkpolicy.Rule{},
			},
		},
	}
	hash, err := p.CalculatedHash()
	require.NoError(t, err)
	p.PolicyHash = hash

	err = e.SetNetworkSettings(context.Background(), staged, &p)
	require.ErrorIs(t, err, ErrNetworkUnsupported)
	require.Equal(t, oldSettings.Allocations.Mappings, e.Configuration.Allocations().Mappings)
}

func TestCorruptDurablePolicyDoesNotPublishStagedSettings(t *testing.T) {
	e := networkTestEnvironment(t, dockerNotFound)
	oldSettings := assignedSettings()
	e.Configuration.SetSettings(oldSettings)
	dir := filepath.Join(config.Get().System.RootDirectory, "network-policy")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, networkTestID+".json"), []byte(`{"broken":`), 0o600))
	staged := environment.Settings{
		Allocations: environment.Allocations{
			Mappings: map[string][]int{"198.51.100.9": {19132}},
		},
	}

	require.Error(t, e.SetNetworkSettings(context.Background(), staged, nil))
	require.Equal(t, oldSettings.Allocations.Mappings, e.Configuration.Allocations().Mappings)
}

func TestCorruptSavedStateFailsClosedAfterAppliedStatus(t *testing.T) {
	var mu sync.Mutex
	var killed bool
	e := networkTestEnvironment(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1.48/containers/"+networkTestID+"/kill" {
			mu.Lock()
			killed = true
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}

		require.NoError(t, json.NewEncoder(w).Encode(container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{
				ID:    "docker-id",
				Name:  "/" + networkTestID,
				State: &container.State{Running: true},
			},
			Config: &container.Config{
				Labels: map[string]string{
					"Service": "Pterodactyl", "ContainerType": "server_process",
				},
			},
		}))
	})
	p := testPolicy(t, 5, 1)
	e.storeNetworkStatus(e.appliedNetworkStatus(p, "docker-id"))
	dir := filepath.Join(config.Get().System.RootDirectory, "network-policy")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, networkTestID+".json"), []byte(`{"broken":`), 0o600))

	status, err := e.NetworkPolicy(context.Background())
	require.NoError(t, err)
	require.Equal(t, "failed", status.State)
	require.Equal(t, uint64(5), status.AppliedRevision)
	require.NotNil(t, status.AppliedHash)
	mu.Lock()
	defer mu.Unlock()

	require.True(t, killed)
}

func TestRejectNetworkPolicyRefusesForeignContainer(t *testing.T) {
	var killed bool
	e := networkTestEnvironment(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1.48/containers/"+networkTestID+"/kill" {
			killed = true
			w.WriteHeader(http.StatusNoContent)
			return
		}

		require.NoError(t, json.NewEncoder(w).Encode(container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{
				ID:    "foreign",
				Name:  "/" + networkTestID,
				State: &container.State{Running: true},
			},
			Config: &container.Config{Labels: map[string]string{"ContainerType": "untrusted"}},
		}))
	})
	cause := errors.New("invalid staged policy")
	err := e.RejectNetworkPolicy(context.Background(), cause)
	require.ErrorIs(t, err, cause)
	require.False(t, killed)
	require.Equal(t, "failed", e.networkStatusSnapshot().State)
}

func TestNetworkMonitorRecoversAndStopsWithServerContext(t *testing.T) {
	e := networkTestEnvironment(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{
				ID:    "docker-id",
				Name:  "/" + networkTestID,
				State: &container.State{Running: true},
			},
			Config: &container.Config{
				Labels: map[string]string{
					"Service": "Pterodactyl", "ContainerType": "server_process",
				},
			},
		}))
	})
	e.Configuration.SetSettings(assignedSettings())
	p := testPolicy(t, 3, 1)
	require.NoError(t, networkpolicy.Save(config.Get().System.RootDirectory, e.Id, p))
	e.storeNetworkStatus(e.failNetworkStatus(p, errors.New("drift")))

	ctx, cancel := context.WithCancel(context.Background())
	done := e.monitorNetwork(ctx, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		return e.networkStatusSnapshot().State == "applied"
	}, time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("network monitor did not stop with server context")
	}

	require.False(t, e.networkMonitor.Load())
}

func TestNetworkCheckDelaySpreadsServers(t *testing.T) {
	const interval = 30 * time.Second
	seen := make(map[time.Duration]bool)
	for _, id := range []string{"server-a", "server-b", "server-c", networkTestID} {
		delay := networkCheckDelay(id, interval)
		require.GreaterOrEqual(t, delay, interval/2)
		require.Less(t, delay, interval)
		require.Equal(t, delay, networkCheckDelay(id, interval))
		seen[delay] = true
	}
	require.Len(t, seen, 4)
	require.Equal(t, time.Nanosecond, networkCheckDelay(networkTestID, time.Nanosecond))
}

func TestStartPublishesPendingBeforeContainerInspection(t *testing.T) {
	inspecting := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e := networkTestEnvironment(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1.48/containers/"+networkTestID+"/json" {
			once.Do(func() {
				close(inspecting)
			})
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"inspection failed"}`))
	})
	p := testPolicy(t, 8, 1)
	require.NoError(t, networkpolicy.Save(config.Get().System.RootDirectory, e.Id, p))
	e.storeNetworkStatus(e.appliedNetworkStatus(p, "old-container"))

	errCh := make(chan error, 1)
	go func() {
		errCh <- e.Start(context.Background())
	}()
	select {
	case <-inspecting:
	case <-time.After(time.Second):
		t.Fatal("start did not inspect the container")
	}

	require.Equal(t, "pending", e.networkStatusSnapshot().State)
	close(release)
	require.Error(t, <-errCh)
}

func TestNetworkRefusesForeignBridgesAndEndpoints(t *testing.T) {
	e := networkTestEnvironment(t, dockerNotFound)
	name, bridge, _, err := networkpolicy.Names(e.Id)
	require.NoError(t, err)
	n := network.Inspect{
		Name:    name,
		Driver:  "bridge",
		Scope:   "local",
		Labels:  map[string]string{networkOwner: e.Id},
		Options: map[string]string{"com.docker.network.bridge.name": bridge},
	}
	require.NoError(t, e.checkNetwork(n, ""))
	n.Containers = map[string]network.EndpointResource{"other-tenant": {}}
	require.Error(t, e.checkNetwork(n, "our-container"))
	n.Containers = nil
	n.Labels[networkOwner] = "other-tenant"
	require.Error(t, e.checkNetwork(n, ""))
}

func TestNetworkBindingOrder(t *testing.T) {
	a := nat.PortMap{
		"25565/tcp": {
			{HostIP: "192.0.2.1", HostPort: "25565"},
			{HostIP: "192.0.2.2", HostPort: "25565"},
		},
	}
	b := nat.PortMap{
		"25565/tcp": {
			{HostIP: "192.0.2.2", HostPort: "25565"},
			{HostIP: "192.0.2.1", HostPort: "25565"},
		},
	}
	require.True(t, samePortBindings(a, b))
	b["25565/tcp"] = b["25565/tcp"][:1]
	require.False(t, samePortBindings(a, b))
}

func TestDeletedEnvironmentRejectsPolicyWork(t *testing.T) {
	e := networkTestEnvironment(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("deleted environment must not call Docker")
	})
	e.networkDeleted = true
	_, err := e.UpdateNetworkPolicy(context.Background(), testPolicy(t, 1, 1))
	require.ErrorIs(t, err, ErrNetworkConflict)
	require.ErrorIs(t, e.ReconcileNetwork(context.Background()), ErrNetworkConflict)
}
