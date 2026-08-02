package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

func newBetterFilesTestFilesystem(t *testing.T, disk int64, ignored ...string) *serverfs.Filesystem {
	t.Helper()
	configuration, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	configuration.AuthenticationToken = "better-files-filesystem-test-token"
	configuration.System.User.Uid = os.Getuid()
	configuration.System.User.Gid = os.Getgid()
	config.Set(configuration)
	fs, err := serverfs.New(t.TempDir(), disk, ignored)
	require.NoError(t, err)
	return fs
}

func writeBetterFilesFixture(t *testing.T, fs *serverfs.Filesystem, name string, data []byte) {
	t.Helper()
	hostPath := filepath.Join(fs.Path(), filepath.FromSlash(strings.TrimPrefix(name, "/")))
	require.NoError(t, os.MkdirAll(filepath.Dir(hostPath), 0o755))
	require.NoError(t, os.WriteFile(hostPath, data, 0o644))
}

func readBetterFilesFixture(t *testing.T, fs *serverfs.Filesystem, name string) []byte {
	t.Helper()
	hostPath := filepath.Join(fs.Path(), filepath.FromSlash(strings.TrimPrefix(name, "/")))
	data, err := os.ReadFile(hostPath)
	require.NoError(t, err)
	return data
}
