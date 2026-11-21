package db

import (
	"path/filepath"
	"sort"
	"testing"
)

func TestDB_ChunksLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "state.db")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// 1. Insert chunk
	c1 := ChunkRecord{
		ID:        "01A",
		Title:     "First Chunk",
		Sprint:    "S01",
		Status:    "pending",
		Hash:      "abcdef",
		AttemptNo: 0,
	}
	if err := store.UpsertChunk(c1); err != nil {
		t.Fatalf("UpsertChunk: %v", err)
	}

	// 2. Query chunk
	got, err := store.GetChunk("01A")
	if err != nil {
		t.Fatalf("GetChunk: %v", err)
	}
	if got.ID != "01A" || got.Status != "pending" || got.Title != "First Chunk" {
		t.Errorf("GetChunk returned unexpected: %+v", got)
	}

	// 3. Update status to running
	if err := store.SetChunkStatus("01A", "running", "", "worker-1", "sonnet"); err != nil {
		t.Fatalf("SetChunkStatus running: %v", err)
	}
	gotRunning, _ := store.GetChunk("01A")
	if gotRunning.Status != "running" || gotRunning.WorkerID != "worker-1" || gotRunning.Tier != "sonnet" {
		t.Errorf("chunk running state unexpected: %+v", gotRunning)
	}

	// 4. Update status to blocked with reason
	if err := store.SetChunkStatus("01A", "blocked", "repair chain spent", "", ""); err != nil {
		t.Fatalf("SetChunkStatus blocked: %v", err)
	}
	gotBlocked, _ := store.GetChunk("01A")
	if gotBlocked.Status != "blocked" || gotBlocked.BlockedReason != "repair chain spent" {
		t.Errorf("chunk blocked state unexpected: %+v", gotBlocked)
	}

	// 5. List chunks
	list, err := store.ListChunks("blocked")
	if err != nil {
		t.Fatalf("ListChunks: %v", err)
	}
	if len(list) != 1 || list[0].ID != "01A" {
		t.Errorf("ListChunks(blocked) unexpected: %+v", list)
	}
}

func TestDB_ConfigOverrides(t *testing.T) {
	tempDir := t.TempDir()
	store, err := Open(filepath.Join(tempDir, "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// Set override
	if err := store.SetConfigOverride("policy.max_consecutive_blocks", "5", "operator:bob"); err != nil {
		t.Fatalf("SetConfigOverride: %v", err)
	}

	overrides, err := store.GetConfigOverrides()
	if err != nil {
		t.Fatalf("GetConfigOverrides: %v", err)
	}
	if overrides["policy.max_consecutive_blocks"] != "5" {
		t.Errorf("got %q, want '5'", overrides["policy.max_consecutive_blocks"])
	}
}

func TestDB_QuotaSpendAndLearnedCost(t *testing.T) {
	tempDir := t.TempDir()
	store, err := Open(filepath.Join(tempDir, "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// Record spends for claude weekly
	spends := []int{4, 6, 5, 8, 5}
	for _, s := range spends {
		if err := store.RecordSpend("01A", "claude", "weekly", s); err != nil {
			t.Fatalf("RecordSpend: %v", err)
		}
	}

	learned, err := store.LearnedCost("claude", "weekly", 10)
	if err != nil {
		t.Fatalf("LearnedCost: %v", err)
	}
	// Median of [4, 5, 5, 6, 8] is 5
	if learned != 5 {
		t.Errorf("LearnedCost = %d, want 5", learned)
	}

	// Unknown pool should return default
	unknownCost, err := store.LearnedCost("unknown", "weekly", 15)
	if err != nil {
		t.Fatalf("LearnedCost unknown: %v", err)
	}
	if unknownCost != 15 {
		t.Errorf("LearnedCost unknown = %d, want default 15", unknownCost)
	}
}

func TestDB_ChunkPinsSurviveUpsertAndShowInList(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	for _, id := range []string{"01A", "01B"} {
		if err := store.UpsertChunk(ChunkRecord{ID: id, Title: id, Sprint: "S", Status: "pending", Hash: "h"}); err != nil {
			t.Fatalf("UpsertChunk: %v", err)
		}
	}
	if err := store.SetChunkPin("01A", "strong", "claude-opus-5-5"); err != nil {
		t.Fatalf("SetChunkPin: %v", err)
	}

	// A plan reload re-upserts the row; the assignment must not be lost.
	if err := store.UpsertChunk(ChunkRecord{ID: "01A", Title: "renamed", Sprint: "S", Status: "pending", Hash: "h2"}); err != nil {
		t.Fatalf("UpsertChunk: %v", err)
	}
	if err := store.SetChunkStatus("01A", "pending", "", "", ""); err != nil {
		t.Fatalf("SetChunkStatus: %v", err)
	}

	list, err := store.ListChunks("")
	if err != nil || len(list) != 2 {
		t.Fatalf("ListChunks: %v %+v", err, list)
	}
	if list[0].PinTier != "strong" || list[0].PinModel != "claude-opus-5-5" {
		t.Errorf("pin missing from list: %+v", list[0])
	}
	if list[1].PinTier != "" || list[1].PinModel != "" {
		t.Errorf("unpinned chunk shows a pin: %+v", list[1])
	}
	got, _ := store.GetChunk("01A")
	if got.PinTier != "strong" {
		t.Errorf("GetChunk lost pin: %+v", got)
	}
	byStatus, _ := store.ListChunks("pending")
	if len(byStatus) != 2 || byStatus[0].PinTier != "strong" {
		t.Errorf("ListChunks(status) lost pin: %+v", byStatus)
	}

	if err := store.ClearChunkPin("01A"); err != nil {
		t.Fatalf("ClearChunkPin: %v", err)
	}
	got, _ = store.GetChunk("01A")
	if got.PinTier != "" || got.PinModel != "" {
		t.Errorf("pin not cleared: %+v", got)
	}
}

func TestDB_ReplaceStatusOnlyTouchesTheSourceStatus(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	for id, st := range map[string]string{"a": "pending", "b": "pending", "c": "running", "d": "done", "e": "held", "f": "blocked"} {
		if err := store.UpsertChunk(ChunkRecord{ID: id, Title: id, Sprint: "S", Status: st, Hash: "h"}); err != nil {
			t.Fatalf("UpsertChunk: %v", err)
		}
	}
	n, err := store.ReplaceStatus("pending", "inactive", "deactivated by operator")
	if err != nil || n != 2 {
		t.Fatalf("ReplaceStatus = %d, %v; want 2", n, err)
	}
	for id, want := range map[string]string{"a": "inactive", "b": "inactive", "c": "running", "d": "done", "e": "held", "f": "blocked"} {
		got, _ := store.GetChunk(id)
		if got.Status != want {
			t.Errorf("chunk %s status = %s, want %s", id, got.Status, want)
		}
	}
	if a, _ := store.GetChunk("a"); a.BlockedReason != "deactivated by operator" {
		t.Errorf("reason not recorded: %q", a.BlockedReason)
	}
	n, err = store.ReplaceStatus("inactive", "pending", "")
	if err != nil || n != 2 {
		t.Fatalf("ReplaceStatus back = %d, %v", n, err)
	}
}

func TestNaturalLess(t *testing.T) {
	ids := []string{"51B.10", "51B.2", "9A", "10A", "46D.1", "46D.10", "46D.2", "46C", "51B.1"}
	want := []string{"9A", "10A", "46C", "46D.1", "46D.2", "46D.10", "51B.1", "51B.2", "51B.10"}
	sort.Slice(ids, func(i, j int) bool { return NaturalLess(ids[i], ids[j]) })
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("natural order = %v, want %v", ids, want)
		}
	}
	if NaturalLess("A1", "A1") {
		t.Error("equal ids must not be less")
	}
}
