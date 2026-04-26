package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type ProtocolMonitor struct {
	mu       sync.RWMutex
	server   *Server
	stats    *ProtocolStats
	enabled  bool
	stopChan chan struct{}
}

func NewProtocolMonitor(s *Server) *ProtocolMonitor {
	return &ProtocolMonitor{
		server:   s,
		stats:    &ProtocolStats{},
		enabled:  false,
		stopChan: make(chan struct{}),
	}
}

func (pm *ProtocolMonitor) Start() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.enabled {
		return nil
	}

	pm.enabled = true
	go pm.run()

	return nil
}

func (pm *ProtocolMonitor) Stop() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !pm.enabled {
		return
	}

	pm.enabled = false
	close(pm.stopChan)
	pm.stopChan = make(chan struct{})
}

func (pm *ProtocolMonitor) run() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-pm.stopChan:
			return
		case <-ticker.C:
			stats, err := pm.server.GetProtocolStats(context.Background())
			if err != nil {
				pm.server.Log().WithError(err).Warn("failed to get protocol stats")
				continue
			}

			pm.mu.Lock()
			pm.stats = stats
			pm.mu.Unlock()

			if b, err := json.Marshal(stats); err == nil {
				pm.server.Events().Publish(ProtocolStatsEvent, string(b))
			}
		}
	}
}

func (pm *ProtocolMonitor) Stats() *ProtocolStats {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	return pm.stats
}
