package router

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSearchV2GlobContentUnicodeAndIgnoredFiles(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0, "logs/**", "ignored/**")
	writeBetterFilesFixture(t, fs, "/config/paper.yml", []byte("name: HéLLo 😀\n"))
	writeBetterFilesFixture(t, fs, "/config/other.yml", []byte("name: other\n"))
	writeBetterFilesFixture(t, fs, "/logs/server.yml", []byte("name: HéLLo 😀\n"))
	writeBetterFilesFixture(t, fs, "/ignored/secret.yml", []byte("name: HéLLo 😀\n"))
	writeBetterFilesFixture(t, fs, "/config/binary.yml", []byte{'x', 0, 'y'})

	data := searchV2Payload{
		PathFilter: &searchV2PathFilter{
			Include:         []string{"**/*.yml"},
			Exclude:         []string{"**/other.yml"},
			CaseInsensitive: true,
		},
		ContentFilter: &searchV2ContentFilter{Query: "héllo 😀", MaxSearchSize: 1024, CaseInsensitive: true},
		PerPage:       100,
	}
	require.NoError(t, validateSearchV2Payload(&data))
	results, err := runSearchV2(context.Background(), fs, "/", data)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "config/paper.yml", results[0].Name)
}

func TestSearchV2SizeIncludeUnmatchedAndLimit(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/large.txt", []byte("content too large for configured search"))
	writeBetterFilesFixture(t, fs, "/small.txt", []byte("match"))
	minimum := int64(1)
	maximum := int64(100)
	data := searchV2Payload{
		SizeFilter:    &searchV2SizeFilter{Min: &minimum, Max: &maximum},
		ContentFilter: &searchV2ContentFilter{Query: "absent", MaxSearchSize: 5, IncludeUnmatched: true},
		PerPage:       1,
	}
	require.NoError(t, validateSearchV2Payload(&data))
	results, err := runSearchV2(context.Background(), fs, "/", data)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "large.txt", results[0].Name)
}

func TestSearchV2HardMaximum(t *testing.T) {
	data := searchV2Payload{PerPage: 5000}
	require.NoError(t, validateSearchV2Payload(&data))
	require.Equal(t, 500, data.PerPage)
}

func TestSearchV2ContentCaseSensitivity(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/mixed.txt", []byte("MiXeD Value"))
	data := searchV2Payload{
		ContentFilter: &searchV2ContentFilter{Query: "mixed value", MaxSearchSize: 1024},
		PerPage:       10,
	}
	require.NoError(t, validateSearchV2Payload(&data))
	results, err := runSearchV2(context.Background(), fs, "/", data)
	require.NoError(t, err)
	require.Empty(t, results)

	data.ContentFilter.CaseInsensitive = true
	results, err = runSearchV2(context.Background(), fs, "/", data)
	require.NoError(t, err)
	require.Len(t, results, 1)
}

func TestStreamSearchV2TextAcrossBufferBoundaries(t *testing.T) {
	prefix := bytes.Repeat([]byte{'a'}, 64*1024-2)
	content := append(prefix, []byte("a😀Target")...)
	matcher := newSearchV2RuneMatcher("a😀target", true)
	require.True(t, streamSearchV2Text(context.Background(), bytes.NewReader(content), int64(len(content)), matcher))

	replacement := []byte("valid � text")
	require.True(t, streamSearchV2Text(
		context.Background(),
		bytes.NewReader(replacement),
		int64(len(replacement)),
		newSearchV2RuneMatcher("�", false),
	))
	require.False(t, streamSearchV2Text(
		context.Background(),
		bytes.NewReader([]byte{'x', 0, 'y'}),
		3,
		newSearchV2RuneMatcher("x", false),
	))
	require.False(t, streamSearchV2Text(
		context.Background(),
		bytes.NewReader([]byte{0xff}),
		1,
		newSearchV2RuneMatcher("", false),
	))
	require.False(t, streamSearchV2Text(
		context.Background(),
		bytes.NewReader([]byte("too long")),
		3,
		newSearchV2RuneMatcher("too", false),
	))
}
