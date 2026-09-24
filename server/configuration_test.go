package server

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
)

func TestSyncConfiguration(t *testing.T) {
	cfg := &config.Configuration{AuthenticationToken: "config-test"}
	cfg.System.CrashDetection.CrashDetectionEnabled = true
	config.Set(cfg)
	s := &Server{}
	settings := []byte(`{
		"uuid":"server-id", "meta":{"name":"game","description":"test"},
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
	require.NoError(t, s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: []byte(`{"uuid":"replacement"}`)}))
	require.Equal(t, "replacement", s.ID())
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
		Settings: []byte(`{"uuid":"server-id","build":{"memory_limit":256,"disk_space":1024}}`),
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
				if s.ID() != "server-id" || s.MemoryLimit() != 256 || s.DiskSpace() != 1024*1024*1024 {
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
