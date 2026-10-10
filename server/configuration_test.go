package server

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
)

func TestSyncConfiguration(t *testing.T) {
	const serverID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	cfg := &config.Configuration{AuthenticationToken: "config-test"}
	cfg.System.CrashDetection.CrashDetectionEnabled = true
	config.Set(cfg)
	s := &Server{}
	settings := []byte(`{
		"uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "meta":{"name":"game","description":"test"},
		"suspended":true, "invocation":"./start", "skip_egg_scripts":true,
		"environment":{"MODE":"test"}, "labels":{"tier":"one"},
		"allocations":{"default":{"ip":"192.0.2.1","port":25565},"mappings":{"192.0.2.1":[25565]}},
		"build":{"memory_limit":256,"disk_space":1024},
		"crash_detection_enabled":false,
		"mounts":[{"source":"/source","target":"/target","read_only":true}],
		"egg":{"id":"egg-id","file_denylist":["private/**"]},
		"container":{"image":"test:latest"}
	}`)
	process := &remote.ProcessConfiguration{}
	process.Stop.Value = "stop"
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: settings, ProcessConfiguration: process}))

	var want Configuration
	require.NoError(t, json.Unmarshal(settings, &want))
	wantJSON, err := json.Marshal(&want)
	require.NoError(t, err)
	actualJSON, err := json.Marshal(s.Config())
	require.NoError(t, err)
	require.JSONEq(t, string(wantJSON), string(actualJSON))
	require.Same(t, process, s.ProcessConfiguration())

	s.Config().SetSuspended(false)
	require.False(t, s.Config().Suspended)
	require.Error(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: []byte(`{"uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`)}))
	require.Equal(t, serverID, s.ID())
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: []byte(`{"uuid":"` + serverID + `"}`)}))
	require.Equal(t, serverID, s.ID())
	require.True(t, s.Config().CrashDetectionEnabled)
	require.Empty(t, s.Config().EnvVars)
	require.Empty(t, s.Config().Labels)
	require.Empty(t, s.Config().Allocations.Mappings)
	require.Empty(t, s.Config().Container.Image)
	require.Empty(t, s.Config().Mounts)
	require.Nil(t, s.ProcessConfiguration())

	before, err := json.Marshal(s.Config())
	require.NoError(t, err)
	require.Error(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: []byte(`{"uuid":`)}))
	after, err := json.Marshal(s.Config())
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
}

func TestSyncConfigurationConcurrent(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "config-test"})
	s := &Server{}
	data := remote.ServerConfigurationResponse{
		Settings: []byte(`{"uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","build":{"memory_limit":256,"disk_space":1024}}`),
	}
	require.NoError(t, s.SyncWithConfiguration(data))

	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(writer bool) {
			defer wg.Done()

			<-start
			for i := 0; i < 100; i++ {
				if writer {
					if err := s.SyncWithConfiguration(data); err != nil {
						t.Errorf("sync configuration: %v", err)
						return
					}

					continue
				}
				if s.ID() != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || s.MemoryLimit() != 256 || s.DiskSpace() != 1024*1024*1024 {
					t.Error("configuration reader observed an invalid value")
					return
				}

				s.Config().SetSuspended(i%2 == 0)
			}
		}(worker%2 == 0)
	}

	close(start)
	wg.Wait()
}

func newConfiguredServer(t *testing.T) *Server {
	t.Helper()
	setTestConfig(t)

	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncWithConfiguration(diskSpaceConfiguration(t, 1)); err != nil {
		t.Fatal(err)
	}
	return s
}

func diskSpaceConfiguration(t *testing.T, disk int64) remote.ServerConfigurationResponse {
	t.Helper()
	settings, err := json.Marshal(map[string]any{
		"uuid":  "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"build": map[string]any{"disk_space": disk},
	})
	if err != nil {
		t.Fatal(err)
	}
	return remote.ServerConfigurationResponse{
		Settings:             settings,
		ProcessConfiguration: &remote.ProcessConfiguration{},
	}
}

// Syncing replaces the configuration while other goroutines may be waiting to
// read it, and none of them should be left waiting forever.
func TestSyncWithConfigurationWhileReading(t *testing.T) {
	s := newConfiguredServer(t)
	responses := []remote.ServerConfigurationResponse{diskSpaceConfiguration(t, 1), diskSpaceConfiguration(t, 2)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				for j := 0; j < 500; j++ {
					if err := s.SyncWithConfiguration(responses[j%2]); err != nil {
						t.Error(err)
						return
					}
				}
			}()
			go func() {
				defer wg.Done()
				for j := 0; j < 500; j++ {
					_ = s.DiskSpace()
					_ = s.Config().GetUuid()
					_ = s.cfg.snapshot()
				}
			}()
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(time.Second * 30):
		t.Fatal("timed out waiting for configuration readers and writers to finish")
	}
	if d := s.DiskSpace(); d != 1024*1024 && d != 2*1024*1024 {
		t.Fatalf("expected disk space from one of the synced configurations, got %d", d)
	}
}

// The configuration is returned by the API, so it must keep encoding its
// values at the top level of the object.
func TestConfigurationSnapshotJSON(t *testing.T) {
	s := newConfiguredServer(t)

	b, err := json.Marshal(s.cfg.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if string(v["uuid"]) != `"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"` {
		t.Fatalf("expected uuid at the top level of the configuration, got %s", b)
	}
	if _, ok := v["build"]; !ok {
		t.Fatalf("expected build at the top level of the configuration, got %s", b)
	}
}
