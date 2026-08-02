package router

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileFingerprintsKnownValues(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/fixture.txt", []byte("hello world\n"))
	expected := map[string]string{
		"md5":        "6f5902ac237024bdd0c176cb93063dc4",
		"crc32":      "af083b2d",
		"sha1":       "22596363b3de40b06f981fb85d82312e8c0ed511",
		"sha224":     "95041dd60ab08c0bf5636d50be85fe9790300f39eb84602858a9b430",
		"sha256":     "a948904f2f0f479b8f8197694b30184b0d2ed1c1cd2a1ec0fb85d299a192a447",
		"sha384":     "6b3b69ff0a404f28d75e98a066d3fc64fffd9940870cc68bece28545b9a75086b343d7a1366838083e4b8f3ca6fd3c80",
		"sha512":     "db3974a97f2407b7cae1ae637c0030687a11913274d578492558e39c16c017de84eacdc8c62fe34ee4e12b4b1428817f09b6a2760c3f8a664ceae94d2434a593",
		"curseforge": "2824650221",
	}
	for algorithm, expectedValue := range expected {
		t.Run(algorithm, func(t *testing.T) {
			result := fingerprintFiles(context.Background(), fs, algorithm, []fingerprintJob{{key: "fixture.txt", path: "/fixture.txt"}})
			require.Equal(t, expectedValue, result["fixture.txt"])
		})
	}
}

func TestFileFingerprintsMultipleAndInvalidEntries(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/one.txt", []byte("one"))
	writeBetterFilesFixture(t, fs, "/two.txt", []byte("two"))
	result := fingerprintFiles(context.Background(), fs, "sha256", []fingerprintJob{
		{key: "one.txt", path: "/one.txt"},
		{key: "two.txt", path: "/two.txt"},
		{key: "missing.txt", path: "/missing.txt"},
		{key: "directory", path: "/"},
	})
	require.Len(t, result, 2)
	require.Contains(t, result, "one.txt")
	require.Contains(t, result, "two.txt")
	require.NotContains(t, result, "missing.txt")
	require.NotContains(t, result, "directory")
}

func TestCRC32FingerprintIsAlwaysEightHexDigits(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/empty.txt", nil)
	result := fingerprintFiles(context.Background(), fs, "crc32", []fingerprintJob{{key: "empty.txt", path: "/empty.txt"}})
	require.Equal(t, "00000000", result["empty.txt"])
}
