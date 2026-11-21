package eventlog

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestEventLog_AppendAndVerify(t *testing.T) {
	db := newTestDB(t)
	log, err := New(db)
	if err != nil {
		t.Fatalf("New event log: %v", err)
	}

	ev1, err := log.Append("chunk_queued", "operator", `{"id":"01A","title":"test"}`)
	if err != nil {
		t.Fatalf("Append ev1: %v", err)
	}
	if ev1.Seq != 1 {
		t.Errorf("ev1.Seq = %d, want 1", ev1.Seq)
	}
	if ev1.PrevHash != GenesisHash {
		t.Errorf("ev1.PrevHash = %q, want GenesisHash", ev1.PrevHash)
	}

	ev2, err := log.Append("attempt_started", "worker-1", `{"chunk":"01A","attempt":1}`)
	if err != nil {
		t.Fatalf("Append ev2: %v", err)
	}
	if ev2.Seq != 2 {
		t.Errorf("ev2.Seq = %d, want 2", ev2.Seq)
	}
	if ev2.PrevHash != ev1.Hash {
		t.Errorf("ev2.PrevHash = %q, want ev1.Hash %q", ev2.PrevHash, ev1.Hash)
	}

	// Verify intact chain passes
	if err := log.Verify(); err != nil {
		t.Fatalf("Verify intact chain failed: %v", err)
	}
}

// Threat Model T6: Event log tampered -> broken chain detected.
func TestAbuse_T6_TamperPayload(t *testing.T) {
	db := newTestDB(t)
	log, err := New(db)
	if err != nil {
		t.Fatalf("New event log: %v", err)
	}

	_, _ = log.Append("chunk_queued", "operator", `{"id":"01A"}`)
	_, _ = log.Append("config_set", "operator", `{"key":"policy.max_consecutive_blocks","val":3}`)
	_, _ = log.Append("attempt_started", "worker-1", `{"chunk":"01A"}`)

	// Tamper directly in the database: alter payload of event seq 2
	_, err = db.Exec("UPDATE events SET payload = ? WHERE seq = 2", `{"key":"policy.max_consecutive_blocks","val":99}`)
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}

	verifyErr := log.Verify()
	if verifyErr == nil {
		t.Fatalf("expected Verify() to fail after payload tampering, but it passed")
	}
	if !strings.Contains(verifyErr.Error(), "hash mismatch") && !strings.Contains(verifyErr.Error(), "seq 2") {
		t.Errorf("expected error to identify hash mismatch at seq 2, got: %v", verifyErr)
	}
}

// Threat Model T6: Event deletion detected.
func TestAbuse_T6_DeleteEvent(t *testing.T) {
	db := newTestDB(t)
	log, err := New(db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _ = log.Append("chunk_queued", "operator", `{"id":"01A"}`)
	_, _ = log.Append("chunk_queued", "operator", `{"id":"02A"}`)
	_, _ = log.Append("chunk_queued", "operator", `{"id":"03A"}`)

	// Delete middle event
	_, err = db.Exec("DELETE FROM events WHERE seq = 2")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	verifyErr := log.Verify()
	if verifyErr == nil {
		t.Fatalf("expected Verify() to fail after deleting an event, but it passed")
	}
	if !strings.Contains(verifyErr.Error(), "sequence gap") && !strings.Contains(verifyErr.Error(), "broken") {
		t.Logf("got error: %v", verifyErr)
	}
}

// Threat Model R1: Operator action without trace -> every control action records actor, time, payload.
func TestR1_OperatorAuditTrace(t *testing.T) {
	db := newTestDB(t)
	log, err := New(db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	before := time.Now().UTC()
	ev, err := log.Append("drain_mode_set", "operator:alice", `{"drain":true,"reason":"weekend"}`)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	after := time.Now().UTC()

	if ev.Actor != "operator:alice" {
		t.Errorf("Actor = %q, want 'operator:alice'", ev.Actor)
	}
	if ev.Timestamp.Before(before.Add(-time.Second)) || ev.Timestamp.After(after.Add(time.Second)) {
		t.Errorf("Timestamp %v not within expected range", ev.Timestamp)
	}

	events, err := log.QueryByActor("operator:alice")
	if err != nil {
		t.Fatalf("QueryByActor: %v", err)
	}
	if len(events) != 1 || events[0].Type != "drain_mode_set" {
		t.Errorf("unexpected query results: %v", events)
	}
}

// Threat Model I1: Secret appears in transcript/log/notification -> pattern + exact value redaction at every sink.
func TestI1_SecretRedactionInAuditLog(t *testing.T) {
	db := newTestDB(t)
	log, err := New(db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	leakedKey := "sk-ant-api03-abcdef12345678901234567890"
	ev, err := log.Append("chunk.started", "worker", "chunk started with key="+leakedKey)
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	if strings.Contains(ev.Payload, leakedKey) {
		t.Fatalf("Secret leaked into eventlog payload: %s", ev.Payload)
	}
	if !strings.Contains(ev.Payload, "[REDACTED]") {
		t.Fatalf("Expected [REDACTED] in eventlog payload: %s", ev.Payload)
	}
}

