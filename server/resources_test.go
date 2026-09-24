package server

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/server/filesystem"
	"github.com/pterodactyl/wings/system"
)

func TestProcSnapshot(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "resource-test"})
	fs, err := filesystem.New(t.TempDir(), 0, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, fs.UnixFS().Close())
	})
	s := &Server{fs: fs}
	s.resources.State = system.NewAtomicString(environment.ProcessRunningState)
	s.resources.UpdateStats(environment.Stats{Memory: 7, Network: environment.NetworkStats{RxBytes: 12}})

	snapshot := s.Proc()
	data, err := json.Marshal(&snapshot)
	require.NoError(t, err)
	var payload struct {
		Memory  uint64                   `json:"memory_bytes"`
		Disk    int64                    `json:"disk_bytes"`
		State   string                   `json:"state"`
		Network environment.NetworkStats `json:"network"`
	}
	require.NoError(t, json.Unmarshal(data, &payload))
	require.Equal(t, uint64(7), payload.Memory)
	require.Equal(t, uint64(12), payload.Network.RxBytes)
	require.Equal(t, fs.CachedUsage(), payload.Disk)
	require.Equal(t, environment.ProcessRunningState, payload.State)

	snapshot.UpdateStats(environment.Stats{Memory: 99})
	require.Equal(t, uint64(99), snapshot.Memory)
	require.Equal(t, uint64(7), s.Proc().Memory)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()

		for i := 0; i < 100; i++ {
			s.resources.UpdateStats(environment.Stats{Memory: uint64(i)})
			s.resources.Reset()
		}
	}()
	go func() {
		defer wg.Done()

		for i := 0; i < 100; i++ {
			copy := s.Proc()
			copy.UpdateStats(environment.Stats{Memory: 200})
		}
	}()
	wg.Wait()
}
