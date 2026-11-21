// Package db provides SQLite persistence for chunk queue state, live configuration
// overrides, quota cost ledger, and the audit event log (ADR-0001, ADR-0004).
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/erinwnash/Shepherd/internal/eventlog"
	"github.com/erinwnash/Shepherd/internal/quota"
	_ "modernc.org/sqlite"
)

// ChunkRecord represents the state of a chunk in the execution queue.
type ChunkRecord struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Sprint        string    `json:"sprint"`
	Status        string    `json:"status"` // pending, running, done, blocked, skipped, held
	Tier          string    `json:"tier,omitempty"`
	WorkerID      string    `json:"worker_id,omitempty"`
	AttemptNo     int       `json:"attempt_no"`
	Hash          string    `json:"hash"`
	BlockedReason string    `json:"blocked_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// PinTier/PinModel are the operator's model assignment (chunk_pins). Empty means automatic routing.
	PinTier  string `json:"pin_tier,omitempty"`
	PinModel string `json:"pin_model,omitempty"`
	// WaitingOn explains why a queued chunk is not runnable yet (unmet dependencies, file overlap).
	// It is computed by the controller when listing; it is never stored.
	WaitingOn string `json:"waiting_on,omitempty"`
}

// Store coordinates SQLite tables for the controller daemon.
type Store struct {
	DB       *sql.DB
	EventLog *eventlog.Log
}

// Open creates or connects to the SQLite database at the given path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create db directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	// Enable WAL for performance and concurrent readers
	if _, err := db.Exec("PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set pragma: %w", err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS chunks (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		sprint TEXT NOT NULL,
		status TEXT NOT NULL,
		tier TEXT NOT NULL DEFAULT '',
		worker_id TEXT NOT NULL DEFAULT '',
		attempt_no INTEGER NOT NULL DEFAULT 0,
		hash TEXT NOT NULL,
		blocked_reason TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_chunks_status ON chunks (status);

	CREATE TABLE IF NOT EXISTS config_overrides (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_by TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS quota_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		chunk_id TEXT NOT NULL,
		pool TEXT NOT NULL,
		window TEXT NOT NULL,
		cost_pct INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_ledger_pool_win ON quota_ledger (pool, window);

	CREATE TABLE IF NOT EXISTS workers (
		id TEXT PRIMARY KEY,
		pubkey_hash TEXT NOT NULL UNIQUE,
		role TEXT NOT NULL DEFAULT 'worker',
		status TEXT NOT NULL DEFAULT 'active',
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_workers_pubkey ON workers (pubkey_hash);

	CREATE TABLE IF NOT EXISTS leases (
		id TEXT PRIMARY KEY,
		worker_id TEXT NOT NULL,
		chunk_id TEXT NOT NULL,
		pool TEXT NOT NULL,
		estimated_pct INTEGER NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		expires_at TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_leases_pool_status ON leases (pool, status);

	CREATE TABLE IF NOT EXISTS cost_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		chunk_id TEXT NOT NULL,
		sprint TEXT NOT NULL,
		tier TEXT NOT NULL,
		tokens_used INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0.0
	);
	CREATE INDEX IF NOT EXISTS idx_cost_chunk ON cost_ledger (chunk_id);
	CREATE INDEX IF NOT EXISTS idx_cost_sprint ON cost_ledger (sprint);

	CREATE TABLE IF NOT EXISTS chunk_pins (
		chunk_id TEXT PRIMARY KEY,
		tier TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS held_patches (
		chunk_id TEXT PRIMARY KEY,
		patch TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	elog, err := eventlog.New(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init event log: %w", err)
	}

	return &Store{
		DB:       db,
		EventLog: elog,
	}, nil
}

// Close closes the underlying SQLite database.
func (s *Store) Close() error {
	return s.DB.Close()
}

// UpsertChunk inserts or updates a chunk record.
func (s *Store) UpsertChunk(c ChunkRecord) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}

	stmt := `
	INSERT INTO chunks (id, title, sprint, status, tier, worker_id, attempt_no, hash, blocked_reason, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		title = excluded.title,
		sprint = excluded.sprint,
		status = excluded.status,
		tier = excluded.tier,
		worker_id = excluded.worker_id,
		attempt_no = excluded.attempt_no,
		hash = excluded.hash,
		blocked_reason = excluded.blocked_reason,
		updated_at = excluded.updated_at
	`
	_, err := s.DB.Exec(stmt, c.ID, c.Title, c.Sprint, c.Status, c.Tier, c.WorkerID, c.AttemptNo, c.Hash, c.BlockedReason, c.CreatedAt.Format(time.RFC3339), now)
	return err
}

// GetChunk queries one chunk by its unique ID.
func (s *Store) GetChunk(id string) (*ChunkRecord, error) {
	row := s.DB.QueryRow("SELECT "+chunkColumns+chunkFrom+" WHERE c.id = ?", id)
	c, err := scanChunk(row)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// chunkColumns/chunkFrom read a chunk together with its model assignment, so a plan reload
// (which upserts the chunk row) can never drop an operator's pin.
const (
	chunkColumns = "c.id, c.title, c.sprint, c.status, c.tier, c.worker_id, c.attempt_no, c.hash, c.blocked_reason, c.created_at, c.updated_at, COALESCE(p.tier, ''), COALESCE(p.model, '')"
	chunkFrom    = " FROM chunks c LEFT JOIN chunk_pins p ON p.chunk_id = c.id"
)

type rowScanner interface{ Scan(dest ...any) error }

func scanChunk(r rowScanner) (ChunkRecord, error) {
	var c ChunkRecord
	var crAt, upAt string
	if err := r.Scan(&c.ID, &c.Title, &c.Sprint, &c.Status, &c.Tier, &c.WorkerID, &c.AttemptNo, &c.Hash, &c.BlockedReason, &crAt, &upAt, &c.PinTier, &c.PinModel); err != nil {
		return c, err
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339, crAt)
	c.UpdatedAt, _ = time.Parse(time.RFC3339, upAt)
	return c, nil
}

// ListChunks returns chunks matching the given status (or all if status is empty).
func (s *Store) ListChunks(status string) ([]ChunkRecord, error) {
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = s.DB.Query("SELECT "+chunkColumns+chunkFrom+" WHERE c.status = ? ORDER BY c.id ASC", status)
	} else {
		rows, err = s.DB.Query("SELECT " + chunkColumns + chunkFrom + " ORDER BY c.id ASC")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChunkRecord
	for rows.Next() {
		c, err := scanChunk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return NaturalLess(out[i].ID, out[j].ID) })
	return out, nil
}

// NaturalLess orders chunk IDs so that digit runs compare by value: "51B.2" < "51B.10", "9A" < "10A".
// Non-digit runs compare bytewise. Equal-valued numbers with different zero padding fall back to
// plain string order so the result is a strict, deterministic ordering.
func NaturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isDigit(a[i]) && isDigit(b[j]) {
			si := i
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			sj := j
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			na, nb := trimZeros(a[si:i]), trimZeros(b[sj:j])
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if a[i] != b[j] {
			return a[i] < b[j]
		}
		i++
		j++
	}
	if len(a)-i != len(b)-j {
		return len(a)-i < len(b)-j
	}
	return a < b
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func trimZeros(s string) string {
	for len(s) > 1 && s[0] == '0' {
		s = s[1:]
	}
	return s
}

// ReplaceStatus moves every chunk currently in status `from` to `to` and returns how many moved.
// Chunks in any other status (running, done, held, ...) are never touched.
func (s *Store) ReplaceStatus(from, to, reason string) (int, error) {
	res, err := s.DB.Exec("UPDATE chunks SET status = ?, blocked_reason = ?, updated_at = ? WHERE status = ?",
		to, reason, time.Now().UTC().Format(time.RFC3339), from)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetChunkPin assigns a tier (and optionally a specific model) to a chunk, overriding automatic routing.
func (s *Store) SetChunkPin(chunkID, tier, model string) error {
	_, err := s.DB.Exec(`INSERT INTO chunk_pins (chunk_id, tier, model, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(chunk_id) DO UPDATE SET tier = excluded.tier, model = excluded.model, updated_at = excluded.updated_at`,
		chunkID, tier, model, time.Now().UTC().Format(time.RFC3339))
	return err
}

// ClearChunkPin returns a chunk to automatic routing.
func (s *Store) ClearChunkPin(chunkID string) error {
	_, err := s.DB.Exec("DELETE FROM chunk_pins WHERE chunk_id = ?", chunkID)
	return err
}

// SetChunkStatus updates a chunk's lifecycle status, reason, and active tier/worker.
func (s *Store) SetChunkStatus(id string, status string, reason string, workerID string, tier string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	stmt := `
	UPDATE chunks
	SET status = ?, blocked_reason = ?, worker_id = ?, tier = ?, updated_at = ?
	WHERE id = ?
	`
	_, err := s.DB.Exec(stmt, status, reason, workerID, tier, now, id)
	return err
}

// SetConfigOverride sets an in-memory / state-level live override (ADR-0004).
func (s *Store) SetConfigOverride(key, value, actor string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	stmt := `
	INSERT INTO config_overrides (key, value, updated_by, updated_at)
	VALUES (?, ?, ?, ?)
	ON CONFLICT(key) DO UPDATE SET
		value = excluded.value,
		updated_by = excluded.updated_by,
		updated_at = excluded.updated_at
	`
	_, err := s.DB.Exec(stmt, key, value, actor, now)
	return err
}

// GetConfigOverrides returns all active live configuration overrides.
func (s *Store) GetConfigOverrides() (map[string]string, error) {
	rows, err := s.DB.Query("SELECT key, value FROM config_overrides")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// RecordSpend logs a measured chunk quota cost in the ledger.
func (s *Store) RecordSpend(chunkID, pool, window string, costPct int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	stmt := `INSERT INTO quota_ledger (timestamp, chunk_id, pool, window, cost_pct) VALUES (?, ?, ?, ?, ?)`
	_, err := s.DB.Exec(stmt, now, chunkID, pool, window, costPct)
	return err
}

// LearnedCost returns the median cost over the last 20 chunk executions for a pool/window.
func (s *Store) LearnedCost(pool, window string, defaultCost int) (int, error) {
	rows, err := s.DB.Query("SELECT cost_pct FROM quota_ledger WHERE pool = ? AND window = ? AND cost_pct > 0 ORDER BY id DESC LIMIT 20", pool, window)
	if err != nil {
		return defaultCost, err
	}
	defer rows.Close()

	var spends []int
	for rows.Next() {
		var cost int
		if err := rows.Scan(&cost); err != nil {
			return defaultCost, err
		}
		spends = append(spends, cost)
	}

	return quota.Median(spends, defaultCost), nil
}

// WorkerRecord represents a paired machine or client certificate identity (ADR-0003).
type WorkerRecord struct {
	ID         string    `json:"id"`
	PubkeyHash string    `json:"pubkey_hash"`
	Role       string    `json:"role"`
	Status     string    `json:"status"` // active, drained, revoked
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// LeaseRecord represents a temporary quota reservation held by a worker (ADR-0001).
type LeaseRecord struct {
	ID           string    `json:"id"`
	WorkerID     string    `json:"worker_id"`
	ChunkID      string    `json:"chunk_id"`
	Pool         string    `json:"pool"`
	EstimatedPct int       `json:"estimated_pct"`
	Status       string    `json:"status"` // active, released, expired
	ExpiresAt    time.Time `json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
}

// UpsertWorker adds or updates a registered worker.
func (s *Store) UpsertWorker(w WorkerRecord) error {
	stmt := `
	INSERT INTO workers (id, pubkey_hash, role, status, created_at, expires_at)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		pubkey_hash = excluded.pubkey_hash,
		role = excluded.role,
		status = excluded.status,
		expires_at = excluded.expires_at
	`
	_, err := s.DB.Exec(stmt, w.ID, w.PubkeyHash, w.Role, w.Status, w.CreatedAt.UTC().Format(time.RFC3339), w.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}

// GetWorker queries a worker by worker ID.
func (s *Store) GetWorker(id string) (*WorkerRecord, error) {
	row := s.DB.QueryRow("SELECT id, pubkey_hash, role, status, created_at, expires_at FROM workers WHERE id = ?", id)
	var w WorkerRecord
	var crAt, exAt string
	if err := row.Scan(&w.ID, &w.PubkeyHash, &w.Role, &w.Status, &crAt, &exAt); err != nil {
		return nil, err
	}
	w.CreatedAt, _ = time.Parse(time.RFC3339, crAt)
	w.ExpiresAt, _ = time.Parse(time.RFC3339, exAt)
	return &w, nil
}

// GetWorkerByPubkey queries an identity by its certificate public key hash (ADR-0003).
func (s *Store) GetWorkerByPubkey(pubkeyHash string) (*WorkerRecord, error) {
	row := s.DB.QueryRow("SELECT id, pubkey_hash, role, status, created_at, expires_at FROM workers WHERE pubkey_hash = ?", pubkeyHash)
	var w WorkerRecord
	var crAt, exAt string
	if err := row.Scan(&w.ID, &w.PubkeyHash, &w.Role, &w.Status, &crAt, &exAt); err != nil {
		return nil, err
	}
	w.CreatedAt, _ = time.Parse(time.RFC3339, crAt)
	w.ExpiresAt, _ = time.Parse(time.RFC3339, exAt)
	return &w, nil
}

// ListWorkers returns all registered workers.
func (s *Store) ListWorkers() ([]WorkerRecord, error) {
	rows, err := s.DB.Query("SELECT id, pubkey_hash, role, status, created_at, expires_at FROM workers ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WorkerRecord
	for rows.Next() {
		var w WorkerRecord
		var crAt, exAt string
		if err := rows.Scan(&w.ID, &w.PubkeyHash, &w.Role, &w.Status, &crAt, &exAt); err != nil {
			return nil, err
		}
		w.CreatedAt, _ = time.Parse(time.RFC3339, crAt)
		w.ExpiresAt, _ = time.Parse(time.RFC3339, exAt)
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWorkerStatus updates a worker's status (e.g. active, drained, revoked).
func (s *Store) SetWorkerStatus(id, status string) error {
	_, err := s.DB.Exec("UPDATE workers SET status = ? WHERE id = ?", status, id)
	return err
}

// CreateLease records an active quota lease.
func (s *Store) CreateLease(l LeaseRecord) error {
	stmt := `INSERT INTO leases (id, worker_id, chunk_id, pool, estimated_pct, status, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.DB.Exec(stmt, l.ID, l.WorkerID, l.ChunkID, l.Pool, l.EstimatedPct, l.Status, l.ExpiresAt.UTC().Format(time.RFC3339), l.CreatedAt.UTC().Format(time.RFC3339))
	return err
}

// GetActiveLeases returns all unexpired active leases for a pool.
func (s *Store) GetActiveLeases(pool string) ([]LeaseRecord, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.DB.Query("SELECT id, worker_id, chunk_id, pool, estimated_pct, status, expires_at, created_at FROM leases WHERE pool = ? AND status = 'active' AND expires_at > ?", pool, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LeaseRecord
	for rows.Next() {
		var l LeaseRecord
		var exAt, crAt string
		if err := rows.Scan(&l.ID, &l.WorkerID, &l.ChunkID, &l.Pool, &l.EstimatedPct, &l.Status, &exAt, &crAt); err != nil {
			return nil, err
		}
		l.ExpiresAt, _ = time.Parse(time.RFC3339, exAt)
		l.CreatedAt, _ = time.Parse(time.RFC3339, crAt)
		out = append(out, l)
	}
	return out, rows.Err()
}

// HasActiveLease reports whether any worker holds an unexpired active lease on the chunk.
func (s *Store) HasActiveLease(chunkID string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var n int
	err := s.DB.QueryRow("SELECT COUNT(*) FROM leases WHERE chunk_id = ? AND status = 'active' AND expires_at > ?", chunkID, now).Scan(&n)
	return n > 0, err
}

// ReleaseLease marks a lease released.
func (s *Store) ReleaseLease(id string) error {
	_, err := s.DB.Exec("UPDATE leases SET status = 'released' WHERE id = ?", id)
	return err
}

// ReclaimExpiredLeases marks expired leases as 'expired'.
func (s *Store) ReclaimExpiredLeases() (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec("UPDATE leases SET status = 'expired' WHERE status = 'active' AND expires_at <= ?", now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RecordChunkCost records token usage and USD cost for a chunk execution attempt.
func (s *Store) RecordChunkCost(chunkID, sprint, tier string, tokens int, costUSD float64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	stmt := `INSERT INTO cost_ledger (timestamp, chunk_id, sprint, tier, tokens_used, cost_usd) VALUES (?, ?, ?, ?, ?, ?)`
	_, err := s.DB.Exec(stmt, now, chunkID, sprint, tier, tokens, costUSD)
	return err
}

// GetChunkCostUSD returns the cumulative USD cost and tokens spent on a chunk across all attempts.
func (s *Store) GetChunkCostUSD(chunkID string) (float64, int, error) {
	row := s.DB.QueryRow("SELECT COALESCE(SUM(cost_usd), 0.0), COALESCE(SUM(tokens_used), 0) FROM cost_ledger WHERE chunk_id = ?", chunkID)
	var cost float64
	var tokens int
	if err := row.Scan(&cost, &tokens); err != nil {
		return 0, 0, err
	}
	return cost, tokens, nil
}

// GetSprintCostUSD returns the cumulative USD cost and tokens spent in a sprint (or across all sprints if sprint is "").
func (s *Store) GetSprintCostUSD(sprint string) (float64, int, error) {
	var row *sql.Row
	if sprint != "" {
		row = s.DB.QueryRow("SELECT COALESCE(SUM(cost_usd), 0.0), COALESCE(SUM(tokens_used), 0) FROM cost_ledger WHERE sprint = ?", sprint)
	} else {
		row = s.DB.QueryRow("SELECT COALESCE(SUM(cost_usd), 0.0), COALESCE(SUM(tokens_used), 0) FROM cost_ledger")
	}
	var cost float64
	var tokens int
	if err := row.Scan(&cost, &tokens); err != nil {
		return 0, 0, err
	}
	return cost, tokens, nil
}

// SaveHeldPatch stores the verified unified diff patch for a chunk held for human review.
func (s *Store) SaveHeldPatch(chunkID, patch string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	stmt := `
	INSERT INTO held_patches (chunk_id, patch, updated_at)
	VALUES (?, ?, ?)
	ON CONFLICT(chunk_id) DO UPDATE SET
		patch = excluded.patch,
		updated_at = excluded.updated_at
	`
	_, err := s.DB.Exec(stmt, chunkID, patch, now)
	return err
}

// GetHeldPatch retrieves a stored unified diff patch for a held chunk.
func (s *Store) GetHeldPatch(chunkID string) (string, error) {
	row := s.DB.QueryRow("SELECT patch FROM held_patches WHERE chunk_id = ?", chunkID)
	var patch string
	if err := row.Scan(&patch); err != nil {
		return "", err
	}
	return patch, nil
}

// DeleteHeldPatch removes a stored held patch after approval or rejection.
func (s *Store) DeleteHeldPatch(chunkID string) error {
	_, err := s.DB.Exec("DELETE FROM held_patches WHERE chunk_id = ?", chunkID)
	return err
}
