package server

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	_ "github.com/glebarez/sqlite"

	"github.com/pterodactyl/wings/config"
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

func TestFileHistoryRecordStartsSnapshotAfterCorruptLatestRevision(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	store := &fileHistoryStore{db: db}
	if err := store.init(); err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	cfg := config.FileHistoryConfiguration{
		Enabled:             true,
		ZstdLevel:           3,
		AnchorInterval:      4,
		KeepChains:          2,
		FileSizeCap:         1024 * 1024,
		PerFileDiskBudget:   1024 * 1024,
		PerServerDiskBudget: 1024 * 1024,
	}

	first := []byte("motd=first\n")
	firstID, err := store.record("/server.properties", nil, first, "user-a", cfg)
	if err != nil {
		t.Fatalf("failed to record initial revision: %v", err)
	}

	fileID, err := findHistoryFile(db, "/server.properties")
	if err != nil {
		t.Fatalf("failed to find file: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO revisions(file_id, chain_id, created, size, user_id, base_id, payload, content_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, firstID, time.Now().UTC().UnixMilli(), len(first), "user-a", firstID, []byte("not a zstd delta"), contentHash(first)); err != nil {
		t.Fatalf("failed to insert corrupt revision: %v", err)
	}

	after := []byte("motd=recovered\n")
	recoveredID, err := store.record("/server.properties", first, after, "user-b", cfg)
	if err != nil {
		t.Fatalf("failed to record after corrupt latest revision: %v", err)
	}

	content, err := reconstructHistoryRevision(db, recoveredID, cfg.ZstdLevel)
	if err != nil {
		t.Fatalf("failed to reconstruct recovered revision: %v", err)
	}
	if !bytes.Equal(content, after) {
		t.Fatal("recovered revision content does not match")
	}
}
