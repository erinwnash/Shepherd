package retro_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/retro"
)

func TestRetro_ReportGeneration(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// Seed completed chunks and spend ledger
	_ = store.UpsertChunk(db.ChunkRecord{
		ID:        "01A",
		Title:     "Feature One",
		Status:    "done",
		CreatedAt: time.Now(),
	})
	_ = store.RecordSpend("01A", "claude", "weekly", 12)

	_ = store.UpsertChunk(db.ChunkRecord{
		ID:        "01B",
		Title:     "Feature Two",
		Status:    "blocked",
		CreatedAt: time.Now(),
	})

	rep, err := retro.Generate(store)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if rep.TotalChunks != 2 {
		t.Errorf("TotalChunks = %d, want 2", rep.TotalChunks)
	}
	if rep.DoneCount != 1 || rep.BlockedCount != 1 {
		t.Errorf("Counts mismatch: done=%d, blocked=%d", rep.DoneCount, rep.BlockedCount)
	}
	if rep.TotalSpendByPool["claude"] != 12 {
		t.Errorf("TotalSpendByPool claude = %d, want 12", rep.TotalSpendByPool["claude"])
	}

	md := retro.FormatMarkdown(rep)
	if !strings.Contains(md, "Shepherd Sprint Retrospective") || !strings.Contains(md, "Feature One") {
		t.Fatalf("Unexpected markdown output: %s", md)
	}
}
