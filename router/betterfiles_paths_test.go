package router

import (
	"testing"

	"github.com/stretchr/testify/require"

	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

func TestNormalizeBetterFilesPaths(t *testing.T) {
	root, err := normalizeBetterFilesRoot("/plugins/./config")
	require.NoError(t, err)
	require.Equal(t, "/plugins/config", root)

	joined, err := joinBetterFilesPath(root, "emoji-😀/paper.yml")
	require.NoError(t, err)
	require.Equal(t, "/plugins/config/emoji-😀/paper.yml", joined)

	for _, invalid := range []string{"../escape", "folder/../escape", "/absolute", "nul\x00file", `windows\path`} {
		_, err := joinBetterFilesPath(root, invalid)
		require.Error(t, err, invalid)
	}
}

func TestEnsureBetterFilesAllowedChecksAncestors(t *testing.T) {
	fs, err := serverfs.New(t.TempDir(), 0, []string{"private/**", "*.secret"})
	require.NoError(t, err)
	require.Error(t, ensureBetterFilesAllowed(fs, "/private/nested/file.txt"))
	require.Error(t, ensureBetterFilesAllowed(fs, "/config/password.secret"))
	require.NoError(t, ensureBetterFilesAllowed(fs, "/config/paper.yml"))
}
