package server

import (
	"bytes"
	"testing"
)

func TestFileHistoryDeltaRoundTrip(t *testing.T) {
	base := bytes.Repeat([]byte("server-start-line=old\n"), 128)
	content := bytes.ReplaceAll(base, []byte("old"), []byte("new"))

	payload, err := encodeHistoryDelta(base, content, 3)
	if err != nil {
		t.Fatalf("encodeHistoryDelta returned error: %v", err)
	}

	decoded, err := decodeHistoryDelta(payload, base, 3)
	if err != nil {
		t.Fatalf("decodeHistoryDelta returned error: %v", err)
	}
	if !bytes.Equal(decoded, content) {
		t.Fatal("decoded delta does not match original content")
	}
}

func TestFileHistorySnapshotRoundTrip(t *testing.T) {
	content := []byte("echo starting server\njava -jar server.jar\n")

	payload, err := encodeHistorySnapshot(content, 3)
	if err != nil {
		t.Fatalf("encodeHistorySnapshot returned error: %v", err)
	}

	decoded, err := decodeHistorySnapshot(payload)
	if err != nil {
		t.Fatalf("decodeHistorySnapshot returned error: %v", err)
	}
	if !bytes.Equal(decoded, content) {
		t.Fatal("decoded snapshot does not match original content")
	}
}

func TestFileHistoryPathFilter(t *testing.T) {
	allowed := []string{
		"/ServerStart.bat",
		"/server.properties",
		"/config/custom.dat.txt",
		"/no-extension",
	}
	for _, path := range allowed {
		if !isFileHistoryPathAllowed(path) {
			t.Fatalf("expected %s to be tracked", path)
		}
	}

	skipped := []string{
		"/server.jar",
		"/world/region/r.0.0.mca",
		"/backup.tar.gz",
		"/database.sqlite",
		"/image.PNG",
	}
	for _, path := range skipped {
		if isFileHistoryPathAllowed(path) {
			t.Fatalf("expected %s to be skipped", path)
		}
	}
}

func TestFileHistoryBinaryContentFilter(t *testing.T) {
	if isLikelyBinaryHistoryContent([]byte("line one\nline two\n")) {
		t.Fatal("expected text content to be tracked")
	}
	if !isLikelyBinaryHistoryContent([]byte{0x50, 0x4b, 0x03, 0x04, 0x00}) {
		t.Fatal("expected nul-containing content to be skipped")
	}
	if !isLikelyBinaryHistoryContent([]byte{0x01, 0x02, 0x03, 0x04, 't', 'e', 'x', 't'}) {
		t.Fatal("expected control-heavy content to be skipped")
	}
}
