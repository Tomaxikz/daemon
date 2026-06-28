package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/glebarez/sqlite"
	"github.com/klauspost/compress/zstd"

	"github.com/pterodactyl/wings/config"
)

const fileHistorySchema = `
CREATE TABLE IF NOT EXISTS files (
    id   INTEGER PRIMARY KEY,
    path TEXT UNIQUE NOT NULL
);

CREATE TABLE IF NOT EXISTS revisions (
    id           INTEGER PRIMARY KEY,
    file_id      INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    chain_id     INTEGER NOT NULL,
    size         INTEGER NOT NULL,
    user_id      TEXT,
    base_id      INTEGER,
    payload      BLOB NOT NULL,
    content_hash TEXT NOT NULL,
    created      INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_revisions_file_created ON revisions(file_id, created DESC);
CREATE INDEX IF NOT EXISTS idx_revisions_file_chain   ON revisions(file_id, chain_id);
CREATE INDEX IF NOT EXISTS idx_revisions_created      ON revisions(created);
`

const fileHistoryZstdDictID uint32 = 0

var fileHistorySkippedExtensions = map[string]struct{}{
	".7z":      {},
	".a":       {},
	".avi":     {},
	".bin":     {},
	".br":      {},
	".bz2":     {},
	".class":   {},
	".dat":     {},
	".db":      {},
	".dll":     {},
	".dylib":   {},
	".exe":     {},
	".gif":     {},
	".gz":      {},
	".idx":     {},
	".jar":     {},
	".jpeg":    {},
	".jpg":     {},
	".ldb":     {},
	".lz4":     {},
	".m4a":     {},
	".m4v":     {},
	".mca":     {},
	".mcc":     {},
	".mcr":     {},
	".mdb":     {},
	".mkv":     {},
	".mov":     {},
	".mp3":     {},
	".mp4":     {},
	".o":       {},
	".ogg":     {},
	".ogv":     {},
	".pak":     {},
	".pack":    {},
	".png":     {},
	".rar":     {},
	".rdb":     {},
	".so":      {},
	".sqlite":  {},
	".sqlite3": {},
	".sst":     {},
	".tar":     {},
	".tgz":     {},
	".war":     {},
	".wav":     {},
	".webm":    {},
	".webp":    {},
	".xz":      {},
	".zip":     {},
	".zst":     {},
}

type FileRevision struct {
	ID         int64     `json:"id"`
	Size       uint64    `json:"size"`
	StoredSize uint64    `json:"stored_size"`
	User       string    `json:"user,omitempty"`
	IsSnapshot bool      `json:"is_snapshot"`
	Created    time.Time `json:"created"`
}

type fileRevisionRow struct {
	id          int64
	chainID     int64
	size        uint64
	storedSize  uint64
	user        string
	baseID      sql.NullInt64
	contentHash string
	createdMs   int64
}

type historyRevisionStep struct {
	id      int64
	baseID  sql.NullInt64
	payload []byte
	hash    string
}

type fileHistoryStore struct {
	mu sync.Mutex
	db *sql.DB
}

var fileHistoryStores sync.Map

func (s *Server) RecordFileRevision(path string, before []byte, after []byte, user string) (int64, error) {
	cfg := config.Get().System.FileHistory
	if !ShouldRecordFileHistory(path, uint64(len(after))) || isLikelyBinaryHistoryContent(after) {
		return 0, nil
	}
	store, err := s.fileHistoryStore()
	if err != nil {
		return 0, err
	}
	return store.record(path, before, after, user, cfg)
}

func ShouldRecordFileHistory(path string, size uint64) bool {
	cfg := config.Get().System.FileHistory
	return cfg.Enabled && size <= cfg.FileSizeCap && isFileHistoryPathAllowed(path)
}

func (s *Server) ListFileRevisions(path string) ([]FileRevision, error) {
	if !config.Get().System.FileHistory.Enabled {
		return nil, nil
	}
	store, err := s.fileHistoryStore()
	if err != nil {
		return nil, err
	}
	return store.list(path)
}

func (s *Server) FileRevisionContent(id int64) ([]byte, string, error) {
	if !config.Get().System.FileHistory.Enabled {
		return nil, "", sql.ErrNoRows
	}
	store, err := s.fileHistoryStore()
	if err != nil {
		return nil, "", err
	}
	return store.content(id)
}

func (s *Server) ForgetFileHistory(path string) {
	if !config.Get().System.FileHistory.Enabled {
		return
	}
	store, err := s.fileHistoryStore()
	if err != nil {
		s.Log().WithError(err).Warn("failed to open file history store")
		return
	}
	if err := store.deleteFile(path); err != nil {
		s.Log().WithError(err).WithField("path", path).Warn("failed to delete file history")
	}
}

func (s *Server) RenameFileHistory(oldPath string, newPath string) {
	if !config.Get().System.FileHistory.Enabled {
		return
	}
	store, err := s.fileHistoryStore()
	if err != nil {
		s.Log().WithError(err).Warn("failed to open file history store")
		return
	}
	if err := store.renameFile(oldPath, newPath); err != nil {
		s.Log().WithError(err).WithField("from_path", oldPath).WithField("to_path", newPath).Warn("failed to rename file history")
	}
}

func (s *Server) fileHistoryStore() (*fileHistoryStore, error) {
	actual, ok := fileHistoryStores.Load(s.ID())
	if ok {
		return actual.(*fileHistoryStore), nil
	}

	dir := filepath.Join(config.Get().System.RootDirectory, "diffs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, s.ID()+".db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	store := &fileHistoryStore{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	actual, loaded := fileHistoryStores.LoadOrStore(s.ID(), store)
	if loaded {
		_ = db.Close()
		return actual.(*fileHistoryStore), nil
	}
	return store, nil
}

func (s *fileHistoryStore) init() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA synchronous=NORMAL;"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA foreign_keys=ON;"); err != nil {
		return err
	}
	_, err := s.db.Exec(fileHistorySchema)
	return err
}

func (s *fileHistoryStore) record(path string, before []byte, after []byte, user string, cfg config.FileHistoryConfiguration) (int64, error) {
	if bytes.Equal(before, after) {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	fileID, err := upsertHistoryFile(tx, path)
	if err != nil {
		return 0, err
	}

	nowMs := time.Now().UTC().UnixMilli()
	latest, err := latestFileRevision(tx, fileID)
	if err != nil {
		return 0, err
	}

	var revisionID int64
	switch {
	case latest == nil && len(before) > 0:
		baseID, err := insertHistorySnapshot(tx, fileID, "", before, nowMs-1, cfg.ZstdLevel)
		if err != nil {
			return 0, err
		}
		revisionID, err = insertCompactHistoryRevision(tx, fileID, baseID, baseID, before, user, after, nowMs, cfg.ZstdLevel)
		if err != nil {
			return 0, err
		}
	case latest == nil:
		revisionID, err = insertHistorySnapshot(tx, fileID, user, after, nowMs, cfg.ZstdLevel)
		if err != nil {
			return 0, err
		}
	default:
		if latest.contentHash == contentHash(after) {
			return latest.id, tx.Commit()
		}
		chainLength, err := currentHistoryChainLength(tx, fileID, latest.chainID)
		if err != nil {
			return 0, err
		}
		if chainLength >= int64(maxUint64(cfg.AnchorInterval, 1)) {
			revisionID, err = insertHistorySnapshot(tx, fileID, user, after, nowMs, cfg.ZstdLevel)
			if err != nil {
				return 0, err
			}
			break
		}
		previous, err := reconstructHistoryRevision(tx, latest.id, cfg.ZstdLevel)
		if err != nil {
			return 0, err
		}
		revisionID, err = insertCompactHistoryRevision(tx, fileID, latest.id, latest.chainID, previous, user, after, nowMs, cfg.ZstdLevel)
		if err != nil {
			return 0, err
		}
	}

	protectedChain, err := latestHistoryChainID(tx, fileID)
	if err != nil {
		return 0, err
	}
	if err := pruneHistory(tx, fileID, protectedChain, cfg); err != nil {
		return 0, err
	}
	if err := pruneServerHistory(tx, fileID, protectedChain, cfg); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return revisionID, nil
}

func (s *fileHistoryStore) list(path string) ([]FileRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fileID, err := findHistoryFile(s.db, path)
	if err != nil || fileID == 0 {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT id, chain_id, created, size, COALESCE(user_id, ''), base_id, LENGTH(payload), content_hash
		FROM revisions WHERE file_id = ?
		ORDER BY id DESC`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	revisions := make([]FileRevision, 0)
	for rows.Next() {
		row, err := scanHistoryRevision(rows)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, FileRevision{
			ID:         row.id,
			Size:       row.size,
			StoredSize: row.storedSize,
			User:       row.user,
			IsSnapshot: !row.baseID.Valid,
			Created:    time.UnixMilli(row.createdMs).UTC(),
		})
	}
	return revisions, rows.Err()
}

func (s *fileHistoryStore) content(id int64) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	content, err := reconstructHistoryRevision(s.db, id, config.Get().System.FileHistory.ZstdLevel)
	if err != nil {
		return nil, "", err
	}
	var p string
	if err := s.db.QueryRow(`
		SELECT files.path
		FROM revisions
		JOIN files ON files.id = revisions.file_id
		WHERE revisions.id = ?`, id).Scan(&p); err != nil {
		return nil, "", err
	}
	return content, p, nil
}

func (s *fileHistoryStore) deleteFile(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM files WHERE path = ?", path)
	return err
}

func (s *fileHistoryStore) renameFile(oldPath string, newPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("UPDATE files SET path = ? WHERE path = ?", newPath, oldPath)
	return err
}

type historyQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

func upsertHistoryFile(tx *sql.Tx, path string) (int64, error) {
	var id int64
	err := tx.QueryRow("SELECT id FROM files WHERE path = ?", path).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	res, err := tx.Exec("INSERT INTO files(path) VALUES (?)", path)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func findHistoryFile(db *sql.DB, path string) (int64, error) {
	var id int64
	err := db.QueryRow("SELECT id FROM files WHERE path = ?", path).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func latestFileRevision(q historyQuerier, fileID int64) (*fileRevisionRow, error) {
	row := q.QueryRow(`
		SELECT id, chain_id, created, size, COALESCE(user_id, ''), base_id, LENGTH(payload), content_hash
		FROM revisions WHERE file_id = ?
		ORDER BY id DESC LIMIT 1`, fileID)
	revision, err := scanHistoryRevision(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &revision, nil
}

func latestHistoryChainID(q historyQuerier, fileID int64) (sql.NullInt64, error) {
	var chainID sql.NullInt64
	err := q.QueryRow("SELECT chain_id FROM revisions WHERE file_id = ? ORDER BY id DESC LIMIT 1", fileID).Scan(&chainID)
	if err == sql.ErrNoRows {
		return sql.NullInt64{}, nil
	}
	return chainID, err
}

func currentHistoryChainLength(q historyQuerier, fileID int64, chainID int64) (int64, error) {
	var count int64
	err := q.QueryRow("SELECT COUNT(*) FROM revisions WHERE file_id = ? AND chain_id = ?", fileID, chainID).Scan(&count)
	return count, err
}

func insertHistorySnapshot(tx *sql.Tx, fileID int64, user string, content []byte, createdMs int64, level int) (int64, error) {
	payload, err := encodeHistorySnapshot(content, level)
	if err != nil {
		return 0, err
	}
	return insertEncodedHistorySnapshot(tx, fileID, user, content, payload, createdMs)
}

func insertEncodedHistorySnapshot(tx *sql.Tx, fileID int64, user string, content []byte, payload []byte, createdMs int64) (int64, error) {
	res, err := tx.Exec(`
		INSERT INTO revisions(file_id, chain_id, created, size, user_id, base_id, payload, content_hash)
		VALUES (?, 0, ?, ?, NULLIF(?, ''), NULL, ?, ?)`,
		fileID, createdMs, len(content), user, payload, contentHash(content))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec("UPDATE revisions SET chain_id = ? WHERE id = ?", id, id); err != nil {
		return 0, err
	}
	return id, nil
}

func insertCompactHistoryRevision(tx *sql.Tx, fileID int64, baseID int64, chainID int64, base []byte, user string, content []byte, createdMs int64, level int) (int64, error) {
	snapshotPayload, err := encodeHistorySnapshot(content, level)
	if err != nil {
		return 0, err
	}
	deltaPayload, deltaErr := encodeHistoryDelta(base, content, level)
	if deltaErr != nil || len(deltaPayload)*10 >= len(snapshotPayload)*9 {
		return insertEncodedHistorySnapshot(tx, fileID, user, content, snapshotPayload, createdMs)
	}
	return insertEncodedHistoryDelta(tx, fileID, baseID, chainID, user, content, deltaPayload, createdMs)
}

func insertEncodedHistoryDelta(tx *sql.Tx, fileID int64, baseID int64, chainID int64, user string, content []byte, payload []byte, createdMs int64) (int64, error) {
	res, err := tx.Exec(`
		INSERT INTO revisions(file_id, chain_id, created, size, user_id, base_id, payload, content_hash)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?)`,
		fileID, chainID, createdMs, len(content), user, baseID, payload, contentHash(content))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func pruneHistory(tx *sql.Tx, fileID int64, protectedChain sql.NullInt64, cfg config.FileHistoryConfiguration) error {
	if err := pruneOldHistoryChains(tx, fileID, maxUint64(cfg.KeepChains, 1)); err != nil {
		return err
	}
	for {
		size, err := historyFilePayloadBytes(tx, fileID)
		if err != nil || size <= cfg.PerFileDiskBudget {
			return err
		}
		freed, err := dropOldestHistoryChain(tx, fileID, protectedChain, maxUint64(cfg.KeepChains, 1))
		if err != nil || freed == 0 {
			return err
		}
	}
}

func pruneServerHistory(tx *sql.Tx, protectedFileID int64, protectedChain sql.NullInt64, cfg config.FileHistoryConfiguration) error {
	if cfg.PerServerDiskBudget == 0 {
		return nil
	}
	for {
		size, err := historyTotalPayloadBytes(tx)
		if err != nil || size <= cfg.PerServerDiskBudget {
			return err
		}
		freed, err := dropGloballyOldestHistoryChain(tx, protectedFileID, protectedChain)
		if err != nil || freed == 0 {
			return err
		}
	}
}

func pruneOldHistoryChains(tx *sql.Tx, fileID int64, keepChains uint64) error {
	rows, err := tx.Query(`
		SELECT DISTINCT chain_id FROM revisions
		WHERE file_id = ?
		ORDER BY chain_id DESC LIMIT ?`, fileID, keepChains)
	if err != nil {
		return err
	}
	defer rows.Close()

	chains := make([]int64, 0, keepChains)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		chains = append(chains, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if uint64(len(chains)) < keepChains || len(chains) == 0 {
		return nil
	}
	_, err = tx.Exec("DELETE FROM revisions WHERE file_id = ? AND chain_id < ?", fileID, chains[len(chains)-1])
	return err
}

func dropOldestHistoryChain(tx *sql.Tx, fileID int64, protected sql.NullInt64, minChains uint64) (uint64, error) {
	var count int64
	if err := tx.QueryRow("SELECT COUNT(DISTINCT chain_id) FROM revisions WHERE file_id = ?", fileID).Scan(&count); err != nil {
		return 0, err
	}
	if uint64(count) <= maxUint64(minChains, 1) {
		return 0, nil
	}

	query := "SELECT MIN(chain_id) FROM revisions WHERE file_id = ?"
	args := []any{fileID}
	if protected.Valid {
		query += " AND chain_id <> ?"
		args = append(args, protected.Int64)
	}
	var chainID sql.NullInt64
	if err := tx.QueryRow(query, args...).Scan(&chainID); err != nil || !chainID.Valid {
		return 0, err
	}
	var freed uint64
	if err := tx.QueryRow("SELECT COALESCE(SUM(LENGTH(payload)), 0) FROM revisions WHERE file_id = ? AND chain_id = ?", fileID, chainID.Int64).Scan(&freed); err != nil {
		return 0, err
	}
	_, err := tx.Exec("DELETE FROM revisions WHERE file_id = ? AND chain_id = ?", fileID, chainID.Int64)
	return freed, err
}

func dropGloballyOldestHistoryChain(tx *sql.Tx, protectedFileID int64, protectedChain sql.NullInt64) (uint64, error) {
	query := `
		SELECT r.file_id, r.chain_id
		FROM revisions r
		WHERE (SELECT COUNT(DISTINCT chain_id) FROM revisions WHERE file_id = r.file_id) > 1`
	args := []any{}
	if protectedChain.Valid {
		query += " AND NOT (r.file_id = ? AND r.chain_id = ?)"
		args = append(args, protectedFileID, protectedChain.Int64)
	}
	query += `
		GROUP BY r.file_id, r.chain_id
		ORDER BY MIN(r.created) ASC, r.chain_id ASC
		LIMIT 1`

	var fileID int64
	var chainID int64
	if err := tx.QueryRow(query, args...).Scan(&fileID, &chainID); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}

	var freed uint64
	if err := tx.QueryRow("SELECT COALESCE(SUM(LENGTH(payload)), 0) FROM revisions WHERE file_id = ? AND chain_id = ?", fileID, chainID).Scan(&freed); err != nil {
		return 0, err
	}
	_, err := tx.Exec("DELETE FROM revisions WHERE file_id = ? AND chain_id = ?", fileID, chainID)
	return freed, err
}

func historyFilePayloadBytes(tx *sql.Tx, fileID int64) (uint64, error) {
	var size uint64
	err := tx.QueryRow("SELECT COALESCE(SUM(LENGTH(payload)), 0) FROM revisions WHERE file_id = ?", fileID).Scan(&size)
	return size, err
}

func historyTotalPayloadBytes(tx *sql.Tx) (uint64, error) {
	var size uint64
	err := tx.QueryRow("SELECT COALESCE(SUM(LENGTH(payload)), 0) FROM revisions").Scan(&size)
	return size, err
}

func reconstructHistoryRevision(q historyQuerier, id int64, level int) ([]byte, error) {
	chain := make([]historyRevisionStep, 0, 8)
	current := sql.NullInt64{Int64: id, Valid: true}
	for current.Valid && len(chain) < 1000 {
		var row historyRevisionStep
		if err := q.QueryRow("SELECT id, base_id, payload, content_hash FROM revisions WHERE id = ?", current.Int64).
			Scan(&row.id, &row.baseID, &row.payload, &row.hash); err != nil {
			return nil, err
		}
		chain = append(chain, row)
		current = row.baseID
		if !row.baseID.Valid {
			break
		}
	}
	if len(chain) == 0 || chain[len(chain)-1].baseID.Valid {
		return nil, fmt.Errorf("revision %d has no snapshot anchor", id)
	}

	reverseHistoryChain(chain)
	content, err := decodeHistorySnapshot(chain[0].payload)
	if err != nil {
		return nil, err
	}
	if contentHash(content) != chain[0].hash {
		return nil, fmt.Errorf("content hash mismatch at revision %d", chain[0].id)
	}
	for _, next := range chain[1:] {
		content, err = decodeHistoryDelta(next.payload, content, level)
		if err != nil {
			return nil, err
		}
		if contentHash(content) != next.hash {
			return nil, fmt.Errorf("content hash mismatch at revision %d", next.id)
		}
	}
	return content, nil
}

func reverseHistoryChain(chain []historyRevisionStep) {
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
}

type revisionScanner interface {
	Scan(dest ...any) error
}

func scanHistoryRevision(scanner revisionScanner) (fileRevisionRow, error) {
	var row fileRevisionRow
	err := scanner.Scan(&row.id, &row.chainID, &row.createdMs, &row.size, &row.user, &row.baseID, &row.storedSize, &row.contentHash)
	return row, err
}

func encodeHistorySnapshot(content []byte, level int) ([]byte, error) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer encoder.Close()
	return encoder.EncodeAll(content, nil), nil
}

func decodeHistorySnapshot(payload []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	return decoder.DecodeAll(payload, nil)
}

func encodeHistoryDelta(base []byte, content []byte, level int) ([]byte, error) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)), zstd.WithEncoderConcurrency(1), zstd.WithEncoderDictRaw(fileHistoryZstdDictID, base))
	if err != nil {
		return nil, err
	}
	defer encoder.Close()
	payload := encoder.EncodeAll(content, nil)
	decoded, err := decodeHistoryDelta(payload, base, level)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(decoded, content) {
		return nil, fmt.Errorf("zstd delta round-trip mismatch")
	}
	return payload, nil
}

func decodeHistoryDelta(payload []byte, base []byte, level int) ([]byte, error) {
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderDictRaw(fileHistoryZstdDictID, base))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	return decoder.DecodeAll(payload, nil)
}

func contentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func maxUint64(value uint64, fallback uint64) uint64 {
	if value == 0 {
		return fallback
	}
	return value
}

func isFileHistoryPathAllowed(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return true
	}
	_, skipped := fileHistorySkippedExtensions[ext]
	return !skipped
}

func isLikelyBinaryHistoryContent(content []byte) bool {
	if len(content) == 0 {
		return false
	}
	sample := content
	if len(sample) > 8192 {
		sample = sample[:8192]
	}

	controlBytes := 0
	for _, b := range sample {
		switch b {
		case 0:
			return true
		case '\n', '\r', '\t', '\f':
			continue
		default:
			if b < 0x20 {
				controlBytes++
			}
		}
	}
	return controlBytes*100 > len(sample)*5
}
