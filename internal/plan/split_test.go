package plan_test

import (
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/plan"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

func TestSplitChunk_PartitioningAndChaining(t *testing.T) {
	orig := &workorder.WorkOrder{
		ID:        "03C",
		Title:     "Refactor Massive Subsystem",
		Sprint:    "S03",
		DependsOn: []string{"03B"},
		Review:    workorder.ReviewMedium,
		Effort:    workorder.EffortL,
		Files: []workorder.File{
			{Path: "src/a.go"},
			{Path: "src/b.go"},
			{Path: "src/c.go"},
			{Path: "src/d.go"},
		},
		Body: "## Goal\nRefactor everything.\n",
	}

	parts, err := plan.SplitChunk(orig, plan.SplitOptions{Parts: 2})
	if err != nil {
		t.Fatalf("SplitChunk failed: %v", err)
	}

	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}

	// Part 1
	p1 := parts[0]
	if p1.ID != "03C.1" {
		t.Errorf("part 1 ID = %s, want 03C.1", p1.ID)
	}
	if len(p1.DependsOn) != 1 || p1.DependsOn[0] != "03B" {
		t.Errorf("part 1 deps = %v, want ['03B']", p1.DependsOn)
	}
	if len(p1.Files) != 2 {
		t.Errorf("part 1 files count = %d, want 2", len(p1.Files))
	}

	// Part 2
	p2 := parts[1]
	if p2.ID != "03C.2" {
		t.Errorf("part 2 ID = %s, want 03C.2", p2.ID)
	}
	if len(p2.DependsOn) != 1 || p2.DependsOn[0] != "03C.1" {
		t.Errorf("part 2 deps = %v, want ['03C.1']", p2.DependsOn)
	}
	if len(p2.Files) != 2 {
		t.Errorf("part 2 files count = %d, want 2", len(p2.Files))
	}

	// Test Serialization
	md, err := plan.FormatWorkOrderMarkdown(p1)
	if err != nil {
		t.Fatalf("FormatWorkOrderMarkdown: %v", err)
	}
	if !strings.Contains(md, "id: 03C.1") || !strings.Contains(md, "depends_on:\n    - 03B") {
		t.Fatalf("Unexpected serialized markdown: %s", md)
	}
}
