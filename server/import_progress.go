package server

import (
	"sync"
	"time"

	"github.com/apex/log"
)

type ImportProgress struct {
	mu             sync.Mutex
	Mode           string
	Status         string
	TotalFiles     int64
	ProcessedFiles int64
	TotalBytes     int64
	ProcessedBytes int64
	CurrentFile    string
	Percentage     float64
	Error          string
	lastPublished  time.Time
	completedAt    time.Time
}

type ImportProgressSnapshot struct {
	CurrentFile    string  `json:"current_file"`
	Mode           string  `json:"mode"`
	Status         string  `json:"status"`
	ProcessedFiles int64   `json:"processed_files"`
	TotalFiles     int64   `json:"total_files"`
	ProcessedBytes int64   `json:"processed_bytes"`
	TotalBytes     int64   `json:"total_bytes"`
	Percentage     float64 `json:"percentage"`
	Error          string  `json:"error,omitempty"`
}

func (s *Server) GetImportProgressSnapshot() (ImportProgressSnapshot, bool) {
	if s == nil {
		return ImportProgressSnapshot{}, false
	}

	if progress := getImportProgress(s.ID()); progress != nil {
		return progress.Snapshot(), true
	}

	return ImportProgressSnapshot{}, false
}

func (s *Server) IsImporting() bool {
	if s == nil {
		return false
	}
	progress := getImportProgress(s.ID())
	return progress != nil && progress.IsImporting()
}

func (p *ImportProgress) IsImporting() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.Status != "completed" && p.Status != "failed"
}

func (p *ImportProgress) IsExpiredTerminal(ttl time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.Status != "completed" && p.Status != "failed" {
		return false
	}

	return !p.completedAt.IsZero() && time.Since(p.completedAt) > ttl
}

func (p *ImportProgress) ShouldScan() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.Mode == "scan"
}

func (p *ImportProgress) SetStatus(s *Server, status string) {
	p.mu.Lock()
	p.Status = status
	p.mu.Unlock()

	p.Publish(s, true)
}

func (p *ImportProgress) SetTotals(s *Server, totalFiles, totalBytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.TotalFiles = totalFiles
	p.TotalBytes = totalBytes
	p.recalculatePercentage()
	p.publishLocked(s, true)
}

func (p *ImportProgress) SetScanEstimate(s *Server, totalFiles, totalBytes int64, currentFile string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.TotalFiles = totalFiles
	p.TotalBytes = totalBytes
	p.CurrentFile = currentFile
	p.recalculatePercentage()
	p.publishLocked(s, false)
}

func (p *ImportProgress) SetError(s *Server, message string) {
	p.mu.Lock()
	p.Status = "failed"
	p.Error = message
	p.completedAt = time.Now()
	p.mu.Unlock()

	p.Publish(s, true)
	s.Events().Publish(ServerImporterCompletedEvent, p.Snapshot())
}

func (p *ImportProgress) Complete(s *Server) {
	p.mu.Lock()
	p.Status = "completed"
	p.completedAt = time.Now()
	if p.Mode == "live" {
		p.TotalFiles = p.ProcessedFiles
		p.TotalBytes = p.ProcessedBytes
	}
	p.recalculatePercentage()
	p.mu.Unlock()

	p.Publish(s, true)
	s.Events().Publish(ServerImporterCompletedEvent, p.Snapshot())
}

func (p *ImportProgress) IncrementProcessed(s *Server, currentFile string, bytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.ProcessedFiles++
	if bytes > 0 {
		p.ProcessedBytes += bytes
	}
	p.CurrentFile = currentFile
	p.recalculatePercentage()

	if p.ProcessedFiles == 1 || p.ProcessedFiles%10 == 0 || time.Since(p.lastPublished) >= time.Second {
		s.Log().WithFields(log.Fields{
			"current_file":    currentFile,
			"processed_files": p.ProcessedFiles,
			"total_files":     p.TotalFiles,
			"processed_bytes": p.ProcessedBytes,
			"total_bytes":     p.TotalBytes,
			"percentage":      p.Percentage,
		}).Debug("Import progress update")
	}

	p.publishLocked(s, p.ProcessedFiles == 1 || p.ProcessedFiles%10 == 0)
}

func (p *ImportProgress) Snapshot() ImportProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	return ImportProgressSnapshot{
		CurrentFile:    p.CurrentFile,
		Mode:           p.Mode,
		Status:         p.Status,
		ProcessedFiles: p.ProcessedFiles,
		TotalFiles:     p.TotalFiles,
		ProcessedBytes: p.ProcessedBytes,
		TotalBytes:     p.TotalBytes,
		Percentage:     p.Percentage,
		Error:          p.Error,
	}
}

func (p *ImportProgress) Publish(s *Server, force bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.publishLocked(s, force)
}

func (p *ImportProgress) recalculatePercentage() {
	if p.TotalBytes > 0 {
		p.Percentage = float64(p.ProcessedBytes) / float64(p.TotalBytes) * 100
		return
	}
	if p.TotalFiles > 0 {
		p.Percentage = float64(p.ProcessedFiles) / float64(p.TotalFiles) * 100
		return
	}
	p.Percentage = 0
}

func (p *ImportProgress) publishLocked(s *Server, force bool) {
	if s == nil {
		return
	}
	if !force && time.Since(p.lastPublished) < 500*time.Millisecond {
		return
	}
	p.lastPublished = time.Now()
	s.Events().Publish(ServerImporterProgressEvent, ImportProgressSnapshot{
		CurrentFile:    p.CurrentFile,
		Mode:           p.Mode,
		Status:         p.Status,
		ProcessedFiles: p.ProcessedFiles,
		TotalFiles:     p.TotalFiles,
		ProcessedBytes: p.ProcessedBytes,
		TotalBytes:     p.TotalBytes,
		Percentage:     p.Percentage,
		Error:          p.Error,
	})
}
