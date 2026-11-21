// Package eventlog implements an append-only, hash-chained audit log for all control actions
// and lifecycle transitions (STRIDE T6 tamper-detection, R1 audit trail).
package eventlog

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/redact"
)

// GenesisHash is the root sentinel hash for sequence 1.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Event represents one immutable entry in the audit hash chain.
type Event struct {
	Seq       int64     `json:"seq"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Actor     string    `json:"actor"`
	Payload   string    `json:"payload"`
	PrevHash  string    `json:"prev_hash"`
	Hash      string    `json:"hash"`
}

// ComputeHash computes the SHA-256 digest over the canonical string representation of an event.
func ComputeHash(seq int64, ts time.Time, eventType, actor, payload, prevHash string) string {
	canonical := fmt.Sprintf("%d|%s|%s|%s|%s|%s",
		seq,
		ts.UTC().Format(time.RFC3339Nano),
		eventType,
		actor,
		payload,
		prevHash,
	)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// Log manages the append-only events table in SQLite.
type Log struct {
	db       *sql.DB
	mu       sync.Mutex
	lastSeq  int64
	lastHash string
}

// New initializes the events table and sets up the hash chain state.
func New(db *sql.DB) (*Log, error) {
	createStmt := `
	CREATE TABLE IF NOT EXISTS events (
		seq INTEGER PRIMARY KEY,
		timestamp TEXT NOT NULL,
		event_type TEXT NOT NULL,
		actor TEXT NOT NULL,
		payload TEXT NOT NULL,
		prev_hash TEXT NOT NULL,
		hash TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_events_actor ON events (actor);
	CREATE INDEX IF NOT EXISTS idx_events_type ON events (event_type);
	`
	if _, err := db.Exec(createStmt); err != nil {
		return nil, fmt.Errorf("create events schema: %w", err)
	}

	l := &Log{db: db}

	// Query last event to restore chain state
	row := db.QueryRow("SELECT seq, hash FROM events ORDER BY seq DESC LIMIT 1")
	var seq int64
	var hash string
	if err := row.Scan(&seq, &hash); err == nil {
		l.lastSeq = seq
		l.lastHash = hash
	} else if err != sql.ErrNoRows {
		return nil, fmt.Errorf("read last event: %w", err)
	} else {
		l.lastSeq = 0
		l.lastHash = GenesisHash
	}

	return l, nil
}

// Append writes a new event to the chain, locking during computation and write.
func (l *Log) Append(eventType, actor, payload string) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Redact secrets at sink per STRIDE I1
	payload = redact.Sanitize(payload)

	seq := l.lastSeq + 1
	ts := time.Now().UTC()
	prevHash := l.lastHash
	if seq == 1 || prevHash == "" {
		prevHash = GenesisHash
	}

	h := ComputeHash(seq, ts, eventType, actor, payload, prevHash)

	insertStmt := `INSERT INTO events (seq, timestamp, event_type, actor, payload, prev_hash, hash)
	              VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := l.db.Exec(insertStmt, seq, ts.Format(time.RFC3339Nano), eventType, actor, payload, prevHash, h)
	if err != nil {
		return Event{}, fmt.Errorf("insert event seq %d: %w", seq, err)
	}

	l.lastSeq = seq
	l.lastHash = h

	return Event{
		Seq:       seq,
		Timestamp: ts,
		Type:      eventType,
		Actor:     actor,
		Payload:   payload,
		PrevHash:  prevHash,
		Hash:      h,
	}, nil
}

// Verify walks the entire event chain from Seq 1 to N, asserting sequence continuity,
// previous hash links, and recalculating each SHA-256 digest (STRIDE T6).
func (l *Log) Verify() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	rows, err := l.db.Query("SELECT seq, timestamp, event_type, actor, payload, prev_hash, hash FROM events ORDER BY seq ASC")
	if err != nil {
		return fmt.Errorf("query events for verification: %w", err)
	}
	defer rows.Close()

	var expectedSeq int64 = 1
	expectedPrevHash := GenesisHash

	for rows.Next() {
		var ev Event
		var tsStr string
		if err := rows.Scan(&ev.Seq, &tsStr, &ev.Type, &ev.Actor, &ev.Payload, &ev.PrevHash, &ev.Hash); err != nil {
			return fmt.Errorf("scan event: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, tsStr)
		if err != nil {
			return fmt.Errorf("parse event seq %d timestamp %q: %w", ev.Seq, tsStr, err)
		}
		ev.Timestamp = t

		if ev.Seq != expectedSeq {
			return fmt.Errorf("audit chain broken: sequence gap detected at seq %d (expected %d)", ev.Seq, expectedSeq)
		}
		if ev.PrevHash != expectedPrevHash {
			return fmt.Errorf("audit chain broken: prev_hash mismatch at seq %d (got %s, want %s)", ev.Seq, ev.PrevHash, expectedPrevHash)
		}

		recomputed := ComputeHash(ev.Seq, ev.Timestamp, ev.Type, ev.Actor, ev.Payload, ev.PrevHash)
		if recomputed != ev.Hash {
			return fmt.Errorf("audit chain broken: hash mismatch at seq %d (data tampered; computed %s, stored %s)", ev.Seq, recomputed, ev.Hash)
		}

		expectedPrevHash = ev.Hash
		expectedSeq++
	}

	return rows.Err()
}

// QueryByActor returns events initiated by a specific actor.
func (l *Log) QueryByActor(actor string) ([]Event, error) {
	rows, err := l.db.Query("SELECT seq, timestamp, event_type, actor, payload, prev_hash, hash FROM events WHERE actor = ? ORDER BY seq ASC", actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var ev Event
		var tsStr string
		if err := rows.Scan(&ev.Seq, &tsStr, &ev.Type, &ev.Actor, &ev.Payload, &ev.PrevHash, &ev.Hash); err != nil {
			return nil, err
		}
		ev.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Since returns all events strictly after the specified sequence number.
func (l *Log) Since(seq int64) ([]Event, error) {
	rows, err := l.db.Query("SELECT seq, timestamp, event_type, actor, payload, prev_hash, hash FROM events WHERE seq > ? ORDER BY seq ASC", seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var ev Event
		var tsStr string
		if err := rows.Scan(&ev.Seq, &tsStr, &ev.Type, &ev.Actor, &ev.Payload, &ev.PrevHash, &ev.Hash); err != nil {
			return nil, err
		}
		ev.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)
		out = append(out, ev)
	}
	return out, rows.Err()
}
