package router

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLargestDirectoriesOrderingPhysicalSizeAndSafety(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0, "ignored/**")
	writeBetterFilesFixture(t, fs, "/large/data.bin", make([]byte, 32*1024))
	writeBetterFilesFixture(t, fs, "/small/data.bin", make([]byte, 1024))
	writeBetterFilesFixture(t, fs, "/ignored/data.bin", make([]byte, 64*1024))

	external := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(external, "outside.bin"), make([]byte, 128*1024), 0o644))
	require.NoError(t, os.Symlink(external, filepath.Join(fs.Path(), "escape")))
	rootInfo, err := betterFilesLstat(fs, "/")
	require.NoError(t, err)
	results, err := analyzeLargestDirectories(context.Background(), fs, "/", rootInfo)
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Equal(t, "large", results[0].Name)
	require.Greater(t, results[0].Size, results[1].Size)
	require.GreaterOrEqual(t, results[0].SizePhysical, int64(0))
	for _, result := range results {
		require.NotEqual(t, "ignored", result.Name)
		require.NotEqual(t, "escape", result.Name)
	}
}

func TestLargestDirectoriesKeepsMinimumWhenFewerThanTenExist(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	for i, size := range []int{500, 400, 300, 200, 100} {
		writeBetterFilesFixture(t, fs, fmt.Sprintf("/directory-%d/data.bin", i), make([]byte, size))
	}
	rootInfo, err := betterFilesLstat(fs, "/")
	require.NoError(t, err)
	results, err := analyzeLargestDirectories(context.Background(), fs, "/", rootInfo)
	require.NoError(t, err)
	require.Len(t, results, 5)
}
