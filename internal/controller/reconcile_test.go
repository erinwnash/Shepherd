package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeOrder(t *testing.T, dir, name, id, deps, status string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	src := "---\nid: " + id + "\ntitle: T " + id + "\nsprint: S1\ndepends_on: [" + deps + "]\nreview: LOW\nest_effort: S\nstatus: " + status + "\nfiles:\n    - " + id + ".go\ntests:\n    - " + id + "_test.go\n---\n## Goal\nDo it.\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
}

func chunkStatus(t *testing.T, c *Controller, id string) string {
	t.Helper()
	rec, err := c.store.GetChunk(id)
	if err != nil || rec == nil {
		t.Fatalf("chunk %s: %v", id, err)
	}
	return rec.Status
}

// Work finished outside Shepherd (file moved to planDONE) must be seen: stale pending/inactive
// rows become done and the downstream chunk is no longer waiting.
func TestController_RefreshReconcilesPlanDone(t *testing.T) {
	ctrl, repo := setupTestController(t, nil)
	plan := filepath.Join(repo, "docs", "plan")
	done := filepath.Join(repo, "docs", "planDONE")
	writeOrder(t, plan, "a.md", "A1", "", "pending")
	writeOrder(t, plan, "b.md", "B1", "'A1'", "pending")
	writeOrder(t, plan, "c.md", "C1", "", "pending")
	if _, err := ctrl.QueueRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = ctrl.QueueSetActive(context.Background(), "A1", false) // operator unchecked it
	_ = ctrl.store.SetChunkStatus("C1", "running", "", "w1", "")

	// A1 and C1 get finished by hand: files move to planDONE. A9 was never in the db.
	writeOrder(t, done, "a.md", "A1", "", "done")
	writeOrder(t, done, "c.md", "C1", "", "done")
	writeOrder(t, done, "z.md", "A9", "", "done")
	_ = os.Remove(filepath.Join(plan, "a.md"))
	_ = os.Remove(filepath.Join(plan, "c.md"))

	res, err := ctrl.QueueRefresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Reconciled != 2 { // A1 and A9; C1 is running and left alone
		t.Fatalf("reconciled = %d, want 2", res.Reconciled)
	}
	if s := chunkStatus(t, ctrl, "A1"); s != "done" {
		t.Errorf("A1 = %s, want done", s)
	}
	if s := chunkStatus(t, ctrl, "A9"); s != "done" {
		t.Errorf("A9 = %s, want done (unknown rows are recorded)", s)
	}
	if s := chunkStatus(t, ctrl, "C1"); s != "running" {
		t.Errorf("running chunk must not be touched, got %s", s)
	}
	if again, _ := ctrl.QueueRefresh(context.Background()); again.Reconciled != 0 {
		t.Errorf("second refresh reconciled %d, want 0 (idempotent)", again.Reconciled)
	}
	evs, _ := ctrl.store.EventLog.Since(0)
	found := false
	for _, e := range evs {
		if e.Type == "queue.reconciled" {
			found = true
		}
	}
	if !found {
		t.Error("reconcile must be audited as queue.reconciled")
	}
}

func TestController_ReconcileDoneIgnoresSkippedAndPlanCopies(t *testing.T) {
	ctrl, repo := setupTestController(t, nil)
	plan := filepath.Join(repo, "docs", "plan")
	done := filepath.Join(repo, "docs", "planDONE")
	writeOrder(t, plan, "a.md", "A1", "", "pending")
	writeOrder(t, done, "a.md", "A1", "", "done") // stale duplicate: the plan copy is still live
	writeOrder(t, plan, "s.md", "S1", "", "pending")
	writeOrder(t, done, "s.md", "S1x", "", "skipped")
	if _, err := ctrl.QueueRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := chunkStatus(t, ctrl, "A1"); s != "pending" {
		t.Errorf("A1 still lives in the plan tree, must stay pending, got %s", s)
	}
	if rec, _ := ctrl.store.GetChunk("S1x"); rec != nil && rec.Status == "done" {
		t.Error("a skipped order must not be recorded as done")
	}
}

func TestController_QueueCheckSpotTestsOneRow(t *testing.T) {
	ctrl, repo := setupTestController(t, nil)
	plan := filepath.Join(repo, "docs", "plan")
	done := filepath.Join(repo, "docs", "planDONE")
	writeOrder(t, plan, "a.md", "A1", "", "pending")
	writeOrder(t, plan, "b.md", "B1", "'A1'", "pending")
	_, _ = ctrl.QueueRefresh(context.Background())
	_ = os.Remove(filepath.Join(plan, "a.md"))
	writeOrder(t, done, "a.md", "A1", "", "done")

	res, err := ctrl.QueueCheck(context.Background(), "A1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reconciled || res.Before != "pending" || res.After != "done" || res.DonePath == "" || res.InPlanTree {
		t.Fatalf("check A1 = %+v", res)
	}
	if chunkStatus(t, ctrl, "A1") != "done" {
		t.Error("check should repair the row")
	}
	res, err = ctrl.QueueCheck(context.Background(), "B1")
	if err != nil || res.Reconciled || !res.InPlanTree || len(res.DependsOn) != 1 || res.DependsOn[0] != "A1=done" {
		t.Fatalf("check B1 = %+v, %v", res, err)
	}
	if _, err := ctrl.QueueCheck(context.Background(), "NOPE"); err == nil {
		t.Error("unknown chunk should be an error")
	}
}
