package router

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type testCopyProgress struct {
	bytes   atomic.Uint64
	files   atomic.Uint64
	started chan struct{}
	once    sync.Once
}

func (p *testCopyProgress) AddBytesProcessed(value uint64) {
	p.bytes.Add(value)
	if p.started != nil && value > 0 {
		p.once.Do(func() { close(p.started) })
	}
}

func (p *testCopyProgress) AddFilesProcessed(value uint64) {
	p.files.Add(value)
}

func TestCopyManyRecursiveMultipleAndOverwrite(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/source/one.txt", []byte("one"))
	writeBetterFilesFixture(t, fs, "/source/nested/two.txt", []byte("two"))
	writeBetterFilesFixture(t, fs, "/single.txt", []byte("single"))
	writeBetterFilesFixture(t, fs, "/destination/one.txt", []byte("old"))

	progress := &testCopyProgress{}
	require.NoError(t, copyManyPath(context.Background(), fs, "/source", "/destination", true, progress))
	require.NoError(t, copyManyPath(context.Background(), fs, "/single.txt", "/single-copy.txt", true, progress))
	require.Equal(t, []byte("one"), readBetterFilesFixture(t, fs, "/destination/one.txt"))
	require.Equal(t, []byte("two"), readBetterFilesFixture(t, fs, "/destination/nested/two.txt"))
	require.Equal(t, []byte("single"), readBetterFilesFixture(t, fs, "/single-copy.txt"))
	require.Equal(t, uint64(3), progress.files.Load())
}

func TestCopyManySkipsExistingAndRejectsTraversal(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/source.txt", []byte("source"))
	writeBetterFilesFixture(t, fs, "/existing.txt", []byte("existing"))
	files, skipped, err := prepareCopyMany(fs, "/", []copyManyFile{{From: "source.txt", To: "existing.txt"}}, false)
	require.NoError(t, err)
	require.Empty(t, files)
	require.Len(t, skipped, 1)
	require.Equal(t, "existing.txt", skipped[0].Name)

	_, _, err = prepareCopyMany(fs, "/", []copyManyFile{{From: "../source.txt", To: "copy.txt"}}, false)
	require.Error(t, err)
}

func TestCopyManyCancellationLeavesExistingDestinationUntouched(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	sourcePath := filepath.Join(fs.Path(), "large.bin")
	file, err := os.Create(sourcePath)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(128*1024*1024))
	require.NoError(t, file.Close())
	writeBetterFilesFixture(t, fs, "/destination.bin", []byte("original"))

	ctx, cancel := context.WithCancel(context.Background())
	progress := &testCopyProgress{started: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- copyManyPath(ctx, fs, "/large.bin", "/destination.bin", true, progress)
	}()
	select {
	case <-progress.started:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("copy did not begin")
	}
	require.Error(t, <-result)
	require.Equal(t, []byte("original"), readBetterFilesFixture(t, fs, "/destination.bin"))
	entries, err := os.ReadDir(fs.Path())
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), ".wings-copy-"))
	}
}

func TestPrepareRenameRejectsDuplicatesTraversalAndExistingDestination(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/one.txt", []byte("one"))
	writeBetterFilesFixture(t, fs, "/existing.txt", []byte("existing"))

	_, err := prepareRenameFiles(fs, "/", []safeRenameFile{{From: "one.txt", To: "same.txt"}, {From: "missing.txt", To: "same.txt"}})
	require.ErrorIs(t, err, errDuplicateRenameDestination)
	_, err = prepareRenameFiles(fs, "/", []safeRenameFile{{From: "../one.txt", To: "new.txt"}})
	require.Error(t, err)
	_, err = prepareRenameFiles(fs, "/", []safeRenameFile{{From: "one.txt", To: "existing.txt"}})
	require.ErrorIs(t, err, errRenameDestinationExists)

	require.NoError(t, os.MkdirAll(filepath.Join(fs.Path(), "directory"), 0o755))
	_, err = prepareRenameFiles(fs, "/", []safeRenameFile{{From: "directory", To: "directory/nested"}})
	require.Error(t, err)
	require.NoError(t, os.Symlink("one.txt", filepath.Join(fs.Path(), "link.txt")))
	_, err = prepareRenameFiles(fs, "/", []safeRenameFile{{From: "link.txt", To: "renamed-link.txt"}})
	require.Error(t, err)
}

func TestPrepareRenameSupportsMultipleFiles(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/one.txt", []byte("one"))
	writeBetterFilesFixture(t, fs, "/two.txt", []byte("two"))
	files, err := prepareRenameFiles(fs, "/", []safeRenameFile{
		{From: "one.txt", To: "renamed/one.txt"},
		{From: "two.txt", To: "renamed/two.txt"},
	})
	require.NoError(t, err)
	for _, file := range files {
		require.NoError(t, fs.RenameNoReplace(file.from, file.to))
	}
	require.Equal(t, []byte("one"), readBetterFilesFixture(t, fs, "/renamed/one.txt"))
	require.Equal(t, []byte("two"), readBetterFilesFixture(t, fs, "/renamed/two.txt"))
}

func TestRenameNoReplacePreservesExistingDestination(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/source.txt", []byte("source"))
	writeBetterFilesFixture(t, fs, "/destination.txt", []byte("destination"))
	require.ErrorIs(t, fs.RenameNoReplace("/source.txt", "/destination.txt"), os.ErrExist)
	require.Equal(t, []byte("source"), readBetterFilesFixture(t, fs, "/source.txt"))
	require.Equal(t, []byte("destination"), readBetterFilesFixture(t, fs, "/destination.txt"))
}
