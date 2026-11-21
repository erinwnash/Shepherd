package controller

import (
	"testing"

	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

func TestAnnotateWaiting(t *testing.T) {
	f := func(p string) []workorder.File { return []workorder.File{{Path: p}} }
	orders := map[string]*workorder.WorkOrder{
		"1": {ID: "1", Files: f("a")},
		"2": {ID: "2", DependsOn: []string{"3", "9"}, Files: f("b")},
		"3": {ID: "3", Files: f("c")},
		"4": {ID: "4", DependsOn: []string{"1"}, Files: f("a")},
		"5": {ID: "5", Files: f("a")},
		"6": {ID: "6", Files: f("z")},
	}
	chunks := []db.ChunkRecord{
		{ID: "1", Status: "running"}, {ID: "2", Status: "pending"}, {ID: "3", Status: "inactive"},
		{ID: "4", Status: "pending"}, {ID: "5", Status: "pending"}, {ID: "6", Status: "pending"},
	}
	annotateWaiting(chunks, orders)
	want := map[string]string{
		"2": "waiting on 3 (inactive), 9 (not in plan)",
		"4": "waiting on 1 (running)",
		"5": "waiting for 1 to finish (shared files)",
		"6": "",
		"1": "", "3": "",
	}
	for _, c := range chunks {
		if c.WaitingOn != want[c.ID] {
			t.Errorf("chunk %s WaitingOn = %q, want %q", c.ID, c.WaitingOn, want[c.ID])
		}
	}
}
