package router

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"
)

func TestStreamDirectoryArchiveEverySupportedFormat(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0, "secret.txt")
	writeBetterFilesFixture(t, fs, "/folder/hello.txt", []byte("hello"))
	writeBetterFilesFixture(t, fs, "/folder/nested/world.txt", []byte("world"))
	writeBetterFilesFixture(t, fs, "/folder/secret.txt", []byte("secret"))

	for _, name := range []string{"tar", "tar_gz", "tar_xz", "tar_bz2", "tar_zstd", "zip"} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			require.NoError(t, streamDirectoryArchive(context.Background(), &output, fs, "/folder", directoryArchiveFormats[name]))
			files := readDirectoryArchiveFixture(t, name, output.Bytes())
			require.Equal(t, []byte("hello"), files["hello.txt"])
			require.Equal(t, []byte("world"), files["nested/world.txt"])
			require.NotContains(t, files, "secret.txt")
		})
	}
}

func TestStreamDirectoryArchiveCancellation(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/folder/file.txt", []byte("content"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	require.ErrorIs(t, streamDirectoryArchive(ctx, &output, fs, "/folder", directoryArchiveFormats["tar"]), context.Canceled)
}

func readDirectoryArchiveFixture(t *testing.T, format string, data []byte) map[string][]byte {
	t.Helper()
	if format == "zip" {
		reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		require.NoError(t, err)
		files := make(map[string][]byte)
		for _, entry := range reader.File {
			if entry.FileInfo().IsDir() {
				continue
			}
			opened, err := entry.Open()
			require.NoError(t, err)
			files[entry.Name], err = io.ReadAll(opened)
			require.NoError(t, err)
			require.NoError(t, opened.Close())
		}
		return files
	}

	var reader io.Reader = bytes.NewReader(data)
	switch format {
	case "tar_gz":
		opened, err := gzip.NewReader(reader)
		require.NoError(t, err)
		defer opened.Close()
		reader = opened
	case "tar_xz":
		opened, err := xz.NewReader(reader)
		require.NoError(t, err)
		reader = opened
	case "tar_bz2":
		reader = bzip2.NewReader(reader)
	case "tar_zstd":
		opened, err := zstd.NewReader(reader)
		require.NoError(t, err)
		defer opened.Close()
		reader = opened
	}

	files := make(map[string][]byte)
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if header.FileInfo().IsDir() {
			continue
		}
		files[header.Name], err = io.ReadAll(archive)
		require.NoError(t, err)
	}
	return files
}
