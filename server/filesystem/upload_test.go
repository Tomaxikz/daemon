package filesystem

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func uploadTestFilesystem(t *testing.T, openat2 bool, limit int64, ignored ...string) *Filesystem {
	t.Helper()
	config.Set(&config.Configuration{AuthenticationToken: "upload-test"})
	f, err := New(t.TempDir(), limit, ignored)
	require.NoError(t, err)
	f.unixFS.UnixFS, err = ufs.NewUnixFS(f.Path(), openat2)
	require.NoError(t, err)
	f.isTest = true
	return f
}

func TestWriteUploadRejectsLinksAndSpecialFilesAtTheSink(t *testing.T) {
	for _, openat2 := range []bool{false, true} {
		t.Run(map[bool]string{true: "openat2", false: "openat"}[openat2], func(t *testing.T) {
			f := uploadTestFilesystem(t, openat2, 100, "protected")
			outside := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0o600))
			require.NoError(t, os.MkdirAll(filepath.Join(f.Path(), "protected/nested"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(f.Path(), "protected/nested/file"), []byte("inside"), 0o600))
			require.NoError(t, os.Symlink(outside, filepath.Join(f.Path(), "escape")))
			require.NoError(t, os.Symlink("protected", filepath.Join(f.Path(), "alias")))
			require.NoError(t, os.Symlink(filepath.Join(outside, "file"), filepath.Join(f.Path(), "leaf")))
			require.NoError(t, os.Link(filepath.Join(f.Path(), "protected/nested/file"), filepath.Join(f.Path(), "hardlink")))
			require.NoError(t, unix.Mkfifo(filepath.Join(f.Path(), "fifo"), 0o600))
			for _, name := range []string{"escape/file", "escape/new/file", "alias/nested/file", "leaf", "hardlink", "fifo", "protected/nested/file"} {
				require.Error(t, f.WriteUpload(name, bytes.NewBufferString("bad"), 3), name)
			}
			data, err := os.ReadFile(filepath.Join(outside, "file"))
			require.NoError(t, err)
			require.Equal(t, "outside", string(data))
			data, err = os.ReadFile(filepath.Join(f.Path(), "protected/nested/file"))
			require.NoError(t, err)
			require.Equal(t, "inside", string(data))
			require.NoDirExists(t, filepath.Join(outside, "new"))
		})
	}
}

func TestWriteUploadReservesQuotaAcrossConcurrentFiles(t *testing.T) {
	f := uploadTestFilesystem(t, false, 10)
	start := make(chan struct{})
	var successes atomic.Int32
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			<-start
			if err := f.WriteUpload(name, bytes.NewBufferString("1234567890"), 10); err == nil {
				successes.Add(1)
			}
		}(name)
	}
	close(start)
	wg.Wait()
	require.Equal(t, int32(1), successes.Load())
	require.Equal(t, int64(10), f.CachedUsage())
	var actual int64
	entries, err := os.ReadDir(f.Path())
	require.NoError(t, err)
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(t, err)
		actual += info.Size()
	}
	require.Equal(t, int64(10), actual)
}

func TestWriteUploadTracksOverwriteShortReadAndParentOwnership(t *testing.T) {
	f := uploadTestFilesystem(t, false, 10)
	settings := config.Get()
	settings.System.User.Uid = os.Getuid()
	settings.System.User.Gid = os.Getgid()
	config.Set(settings)
	f.isTest = false
	require.NoError(t, f.WriteUpload("a/b/file", bytes.NewBufferString("1234567890"), 10))
	for _, name := range []string{"a", "a/b", "a/b/file"} {
		var stat unix.Stat_t
		require.NoError(t, unix.Stat(filepath.Join(f.Path(), name), &stat))
		require.Equal(t, uint32(os.Getuid()), stat.Uid)
		require.Equal(t, uint32(os.Getgid()), stat.Gid)
	}
	require.NoError(t, f.WriteUpload("a/b/file", bytes.NewBufferString("12"), 2))
	require.Equal(t, int64(2), f.CachedUsage())
	require.ErrorIs(t, f.WriteUpload("a/b/short", bytes.NewBufferString("123"), 8), io.EOF)
	require.Equal(t, int64(5), f.CachedUsage())
	require.Error(t, f.WriteUpload("a/b/file", bytes.NewBufferString("1234567890"), 10))
	data, err := os.ReadFile(filepath.Join(f.Path(), "a/b/file"))
	require.NoError(t, err)
	require.Equal(t, "12", string(data))
	f.SetDiskLimit(-1)
	require.Error(t, f.WriteUpload("disabled/file", bytes.NewReader(nil), 0))
	require.NoDirExists(t, filepath.Join(f.Path(), "disabled"))
}
