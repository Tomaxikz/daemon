package server

import (
	"sync"

	"github.com/apex/log"
)

type ImportProgress struct {
	mu             sync.Mutex
	TotalFiles     int64
	ProcessedFiles int64
	CurrentFile    string
	Percentage     float64
}

type ImportProgressSnapshot struct {
	CurrentFile    string  `json:"current_file"`
	ProcessedFiles int64   `json:"processed_files"`
	TotalFiles     int64   `json:"total_files"`
	Percentage     float64 `json:"percentage"`
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
	return getImportProgress(s.ID()) != nil
}

func (p *ImportProgress) SetTotalFiles(total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.TotalFiles = total
	if p.TotalFiles > 0 && p.ProcessedFiles > 0 {
		p.Percentage = float64(p.ProcessedFiles) / float64(p.TotalFiles) * 100
	}
}

func (p *ImportProgress) IncrementProcessed(s *Server, currentFile string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.ProcessedFiles++
	p.CurrentFile = currentFile

	if p.TotalFiles > 0 {
		p.Percentage = float64(p.ProcessedFiles) / float64(p.TotalFiles) * 100
	}

	if p.ProcessedFiles == 1 || p.ProcessedFiles%10 == 0 {
		s.Log().WithFields(log.Fields{
			"current_file":    currentFile,
			"processed_files": p.ProcessedFiles,
			"total_files":     p.TotalFiles,
			"percentage":      p.Percentage,
		}).Debug("Import progress update")
	}
}

func (p *ImportProgress) Snapshot() ImportProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	return ImportProgressSnapshot{
		CurrentFile:    p.CurrentFile,
		ProcessedFiles: p.ProcessedFiles,
		TotalFiles:     p.TotalFiles,
		Percentage:     p.Percentage,
	}
}
