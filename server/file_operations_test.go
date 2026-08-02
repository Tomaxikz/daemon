package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	wingsevents "github.com/pterodactyl/wings/events"
)

func testOperationServer(t *testing.T, id string) *Server {
	t.Helper()
	s, err := New(nil)
	require.NoError(t, err)
	s.cfg.Uuid = id
	t.Cleanup(s.CtxCancel)
	return s
}

func waitForOperationEvent(t *testing.T, events <-chan []byte, topic string) wingsevents.Event {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case raw := <-events:
			event := wingsevents.MustDecode(raw)
			if event.Topic == topic {
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %q", topic)
		}
	}
}

func operationEventArgs(t *testing.T, event wingsevents.Event) []string {
	t.Helper()
	values, ok := event.Data.([]interface{})
	require.True(t, ok)
	args := make([]string, len(values))
	for index, value := range values {
		args[index], ok = value.(string)
		require.True(t, ok)
	}
	return args
}

func TestFileOperationProgressCompletionAndCleanup(t *testing.T) {
	s := testOperationServer(t, "server-a")
	eventChannel := make(chan []byte, 16)
	s.Events().On(eventChannel)
	t.Cleanup(func() { s.Events().Off(eventChannel) })

	op, done, err := s.FileOperations().Start(context.Background(), "copy_many", func(_ context.Context, operation *FileOperation) error {
		operation.SetBytesTotal(500)
		operation.AddBytesProcessed(100)
		operation.AddFilesProcessed(2)
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, <-done)

	progress := waitForOperationEvent(t, eventChannel, OperationProgressEvent)
	args := operationEventArgs(t, progress)
	require.Len(t, args, 2)
	require.Equal(t, op.Identifier(), args[0])
	var snapshot FileOperationSnapshot
	require.NoError(t, json.Unmarshal([]byte(args[1]), &snapshot))
	require.Equal(t, "copy_many", snapshot.Type)
	require.Equal(t, "server-a", snapshot.ServerUUID)

	completed := waitForOperationEvent(t, eventChannel, OperationCompletedEvent)
	require.Equal(t, []string{op.Identifier()}, operationEventArgs(t, completed))
	require.Eventually(t, func() bool { return s.FileOperations().Count() == 0 }, time.Second, time.Millisecond)
}

func TestFileOperationErrorAndCleanup(t *testing.T) {
	s := testOperationServer(t, "server-errors")
	eventChannel := make(chan []byte, 16)
	s.Events().On(eventChannel)
	t.Cleanup(func() { s.Events().Off(eventChannel) })

	op, done, err := s.FileOperations().Start(context.Background(), "copy_many", func(context.Context, *FileOperation) error {
		return errors.New("/host/private/path")
	})
	require.NoError(t, err)
	require.Error(t, <-done)
	event := waitForOperationEvent(t, eventChannel, OperationErrorEvent)
	require.Equal(t, []string{op.Identifier(), "file operation failed"}, operationEventArgs(t, event))
	require.Zero(t, s.FileOperations().Count())
}

func TestFileOperationCancellationAndServerIsolation(t *testing.T) {
	first := testOperationServer(t, "server-first")
	second := testOperationServer(t, "server-second")
	eventChannel := make(chan []byte, 16)
	first.Events().On(eventChannel)
	t.Cleanup(func() { first.Events().Off(eventChannel) })

	started := make(chan struct{})
	op, done, err := first.FileOperations().Start(context.Background(), "copy_many", func(ctx context.Context, operation *FileOperation) error {
		close(started)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				operation.AddBytesProcessed(1)
			}
		}
	})
	require.NoError(t, err)
	<-started
	require.False(t, second.FileOperations().Cancel(op.Identifier()))
	require.True(t, first.FileOperations().Cancel(op.Identifier()))
	require.ErrorIs(t, <-done, ErrFileOperationCanceled)
	event := waitForOperationEvent(t, eventChannel, OperationErrorEvent)
	require.Equal(t, []string{op.Identifier(), "operation cancelled"}, operationEventArgs(t, event))
	require.Zero(t, first.FileOperations().Count())
}

func TestFileOperationRaceSafeProgress(t *testing.T) {
	s := testOperationServer(t, "server-race")
	var readers sync.WaitGroup
	op, done, err := s.FileOperations().Start(context.Background(), "copy_many", func(_ context.Context, operation *FileOperation) error {
		for i := 0; i < 10000; i++ {
			operation.AddBytesProcessed(1)
			operation.AddFilesProcessed(1)
		}
		return nil
	})
	require.NoError(t, err)
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 1000; j++ {
				_ = op.Snapshot()
			}
		}()
	}
	readers.Wait()
	require.NoError(t, <-done)
}
