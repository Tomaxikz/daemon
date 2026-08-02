package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const maxFileOperationsPerServer = 64

var (
	ErrFileOperationLimit    = errors.New("too many file operations are active")
	ErrFileOperationCanceled = errors.New("operation cancelled")
)

type FileOperationSnapshot struct {
	Type           string    `json:"type"`
	ServerUUID     string    `json:"server_uuid"`
	StartTime      time.Time `json:"start_time"`
	BytesProcessed uint64    `json:"bytes_processed"`
	BytesTotal     uint64    `json:"bytes_total"`
	FilesProcessed uint64    `json:"files_processed"`
	Cancelled      bool      `json:"cancelled"`
}

type FileOperation struct {
	identifier string
	typ        string
	serverUUID string
	startTime  time.Time
	cancel     context.CancelFunc

	bytesProcessed atomic.Uint64
	bytesTotal     atomic.Uint64
	filesProcessed atomic.Uint64
	cancelled      atomic.Bool
}

func (o *FileOperation) Identifier() string { return o.identifier }

func (o *FileOperation) AddBytesProcessed(value uint64) {
	o.bytesProcessed.Add(value)
}

func (o *FileOperation) SetBytesTotal(value uint64) {
	o.bytesTotal.Store(value)
}

func (o *FileOperation) AddFilesProcessed(value uint64) {
	o.filesProcessed.Add(value)
}

func (o *FileOperation) Snapshot() FileOperationSnapshot {
	return FileOperationSnapshot{
		Type:           o.typ,
		ServerUUID:     o.serverUUID,
		StartTime:      o.startTime,
		BytesProcessed: o.bytesProcessed.Load(),
		BytesTotal:     o.bytesTotal.Load(),
		FilesProcessed: o.filesProcessed.Load(),
		Cancelled:      o.cancelled.Load(),
	}
}

type FileOperationManager struct {
	server *Server

	mu         sync.RWMutex
	operations map[string]*FileOperation
}

func NewFileOperationManager(server *Server) *FileOperationManager {
	return &FileOperationManager{server: server, operations: make(map[string]*FileOperation)}
}

// Start registers and runs one bounded, server-local file operation. The
// returned channel always receives exactly one terminal result.
func (m *FileOperationManager) Start(
	parent context.Context,
	typ string,
	work func(context.Context, *FileOperation) error,
) (*FileOperation, <-chan error, error) {
	if parent == nil {
		parent = m.server.Context()
	}

	ctx, cancel := context.WithCancel(m.server.Context())
	stopParent := context.AfterFunc(parent, cancel)
	op := &FileOperation{
		identifier: uuid.NewString(),
		typ:        typ,
		serverUUID: m.server.ID(),
		startTime:  time.Now().UTC(),
		cancel:     cancel,
	}

	m.mu.Lock()
	if len(m.operations) >= maxFileOperationsPerServer {
		m.mu.Unlock()
		stopParent()
		cancel()
		return nil, nil, ErrFileOperationLimit
	}
	m.operations[op.identifier] = op
	m.mu.Unlock()

	result := make(chan error, 1)
	go func() {
		defer stopParent()
		defer cancel()

		progressDone := make(chan struct{})
		progressStopped := make(chan struct{})
		go m.publishProgressUntilDone(op, progressDone, progressStopped)
		err := work(ctx, op)
		close(progressDone)
		<-progressStopped

		m.mu.Lock()
		delete(m.operations, op.identifier)
		cancelled := op.cancelled.Load() || ctx.Err() != nil
		m.mu.Unlock()

		switch {
		case cancelled:
			err = ErrFileOperationCanceled
			m.publishArgs(OperationErrorEvent, op.identifier, ErrFileOperationCanceled.Error())
		case err != nil:
			m.server.Log().WithError(err).WithField("operation", op.identifier).Warn("file operation failed")
			m.publishArgs(OperationErrorEvent, op.identifier, safeFileOperationError(err))
		default:
			m.publishArgs(OperationCompletedEvent, op.identifier)
		}

		result <- err
		close(result)
	}()

	return op, result, nil
}

func (m *FileOperationManager) publishProgressUntilDone(op *FileOperation, done <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	m.publishProgress(op)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			m.publishProgress(op)
		}
	}
}

func (m *FileOperationManager) publishProgress(op *FileOperation) {
	payload, err := json.Marshal(op.Snapshot())
	if err != nil {
		return
	}
	m.publishArgs(OperationProgressEvent, op.identifier, string(payload))
}

func (m *FileOperationManager) publishArgs(event string, args ...string) {
	m.server.Events().Publish(event, args)
}

func (m *FileOperationManager) Cancel(identifier string) bool {
	m.mu.RLock()
	op, ok := m.operations[identifier]
	if ok {
		op.cancelled.Store(true)
		op.cancel()
	}
	m.mu.RUnlock()
	return ok
}

func (m *FileOperationManager) CancelAll() {
	m.mu.RLock()
	for _, op := range m.operations {
		op.cancelled.Store(true)
		op.cancel()
	}
	m.mu.RUnlock()
}

func (m *FileOperationManager) Has(identifier string) bool {
	m.mu.RLock()
	_, ok := m.operations[identifier]
	m.mu.RUnlock()
	return ok
}

func (m *FileOperationManager) Count() int {
	m.mu.RLock()
	count := len(m.operations)
	m.mu.RUnlock()
	return count
}

func safeFileOperationError(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrFileOperationCanceled) {
		return ErrFileOperationCanceled.Error()
	}
	return "file operation failed"
}
