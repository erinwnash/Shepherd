package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/adapter"
	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/quota"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

func frWO(id string, review workorder.Review, deps []string, files ...string) *workorder.WorkOrder {
	wo := &workorder.WorkOrder{ID: id, Title: "T " + id, Sprint: "S1", Status: workorder.StatusPending, Review: review, DependsOn: deps}
	for _, f := range files {
		wo.Files = append(wo.Files, workorder.File{Path: f, New: true})
	}
	return wo
}

func frPending(ids ...string) []db.ChunkRecord {
	var out []db.ChunkRecord
	for _, id := range ids {
		out = append(out, db.ChunkRecord{ID: id, Status: "pending"})
	}
	return out
}

func isHighReview(wo *workorder.WorkOrder) bool { return wo.Review == workorder.ReviewHigh }

// The lane must leave the next 10 items to the main queue, then take high-complexity chunks whose
// dependencies are all done and whose files do not collide with running or imminent work.
func TestFrontRun_PickRules(t *testing.T) {
	var ids []string
	orders := map[string]*workorder.WorkOrder{}
	for i := 1; i <= 16; i++ {
		id := fmt.Sprintf("C%02d", i)
		ids = append(ids, id)
		review := workorder.ReviewLow
		if i <= 3 || i == 13 || i == 14 || i == 15 || i == 16 {
			review = workorder.ReviewHigh // C01-C03 sit inside the protected head
		}
		orders[id] = frWO(id, review, nil, id+".go")
	}
	orders["C13"].DependsOn = []string{"C12"}                // precursor not done
	orders["C14"].Files = []workorder.File{{Path: "C02.go"}} // collides with the head
	status := map[string]string{"C12": "pending"}
	pending := frPending(ids...)

	got := pickFrontRun(pending, orders, status, nil, map[string]bool{}, frontRunSkipAhead, isHighReview)
	if got == nil || got.ID != "C15" {
		t.Fatalf("picked %v, want C15 (C01-C03 are in the head, C13 has an open precursor, C14 collides)", got)
	}
	if got := pickFrontRun(pending, orders, status, nil, map[string]bool{"C15": true}, frontRunSkipAhead, isHighReview); got == nil || got.ID != "C16" {
		t.Fatalf("a chunk already tried must be skipped, got %v", got)
	}
	running := [][]workorder.File{{{Path: "C15.go"}}}
	if got := pickFrontRun(pending, orders, status, running, map[string]bool{}, frontRunSkipAhead, isHighReview); got == nil || got.ID != "C16" {
		t.Fatalf("files in use by a running chunk must be avoided, got %v", got)
	}
	pending[14].PinTier = "strong" // operator assigned this one a model
	if got := pickFrontRun(pending, orders, status, nil, map[string]bool{}, frontRunSkipAhead, isHighReview); got == nil || got.ID != "C16" {
		t.Fatalf("operator-assigned chunks must be left alone, got %v", got)
	}
	if got := pickFrontRun(frPending(ids[:10]...), orders, status, nil, nil, frontRunSkipAhead, isHighReview); got != nil {
		t.Fatalf("10 or fewer pending: nothing to front-run, got %v", got)
	}
	if got := pickFrontRun(frPending(ids[:12]...), orders, status, nil, nil, frontRunSkipAhead, isHighReview); got != nil {
		t.Fatalf("C11/C12 are low complexity: nothing to take, got %v", got)
	}
}

func frontRunFixture(t *testing.T, onRun func(ctx context.Context, opts adapter.RunOptions) (adapter.RunResult, error)) (*Controller, string) {
	t.Helper()
	old := frontRunPollInterval
	frontRunPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { frontRunPollInterval = old })

	ctrl, repo := setupTestController(t, &mockTestAdapter{onRun: func(ctx context.Context, _ string, opts adapter.RunOptions) (adapter.RunResult, error) {
		return onRun(ctx, opts)
	}})
	ctrl.slotsPool.Guard.RAMChecker = func() (float64, error) { return 64, nil }
	off := false
	ctrl.cfg.Tiers["burn"] = config.TierConfig{Adapter: "mock-test", Model: "big", Enabled: &off} // switched off on purpose

	planDir := filepath.Join(repo, "docs", "plan")
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("F%02d", i)
		review := "LOW"
		if i == 12 {
			review = "HIGH"
		}
		src := fmt.Sprintf("---\nid: %s\ntitle: T %s\nsprint: S1\nreview: %s\nest_effort: S\nstatus: pending\nfiles:\n    - new:%s.txt\ntests:\n    - %s_test.go\n---\n## Goal\nDo it.\n", id, id, review, id, id)
		if err := os.MkdirAll(planDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(planDir, id+".md"), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}
	gitDo(t, repo, "add", "-A")
	gitDo(t, repo, "commit", "-m", "plan")
	if _, err := ctrl.QueueRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ctrl, repo
}

func gitDo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().After(deadline) == false {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFrontRun_EndToEndLandsInIsolationOnAnOffTier(t *testing.T) {
	var runRoot atomic.Value
	var gotModel atomic.Value
	ctrl, repo := frontRunFixture(t, func(ctx context.Context, opts adapter.RunOptions) (adapter.RunResult, error) {
		runRoot.Store(opts.WorktreeRoot)
		gotModel.Store(opts.Model)
		_ = os.WriteFile(filepath.Join(opts.WorktreeRoot, "F12.txt"), []byte("front run\n"), 0644)
		return adapter.RunResult{ExitCode: 0, Output: "ok"}, nil
	})

	if err := ctrl.FrontRunStart(context.Background(), "nope", ""); err == nil {
		t.Fatal("unknown tier must be rejected")
	}
	if err := ctrl.FrontRunStart(context.Background(), "burn", "huge-model"); err != nil {
		t.Fatalf("a switched-off tier is exactly what front-run is for: %v", err)
	}
	if err := ctrl.FrontRunStart(context.Background(), "burn", ""); err == nil {
		t.Fatal("a second start must be rejected")
	}
	waitFor(t, "F12 to be done", func() bool {
		rec, _ := ctrl.store.GetChunk("F12")
		return rec != nil && rec.Status == "done"
	})

	if root, _ := runRoot.Load().(string); root == "" || root == repo {
		t.Errorf("the lane must work in its own worktree, ran in %q", root)
	}
	if m, _ := gotModel.Load().(string); m != "huge-model" {
		t.Errorf("model = %q, want the one chosen for the lane", m)
	}
	if _, err := os.Stat(filepath.Join(repo, "F12.txt")); err != nil {
		t.Errorf("landed file missing from the main tree: %v", err)
	}
	if log := gitDo(t, repo, "log", "--oneline", "-3"); !strings.Contains(log, "feat(F12)") {
		t.Errorf("expected a feat(F12) commit on the main branch:\n%s", log)
	}
	if rec, _ := ctrl.store.GetChunk("F01"); rec.Status != "pending" {
		t.Errorf("head of the queue must stay with the main lane, F01 = %s", rec.Status)
	}
	st, _ := ctrl.Status(context.Background())
	if st.FrontRun == nil || st.FrontRun.Landed != 1 || st.FrontRun.Tier != "burn" {
		t.Errorf("status = %+v", st.FrontRun)
	}

	if err := ctrl.FrontRunStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _ := ctrl.Status(context.Background()); st.FrontRun != nil {
		t.Errorf("lane should be gone after stop, got %+v", st.FrontRun)
	}
}

func TestFrontRun_StopHandsTheChunkBack(t *testing.T) {
	started := make(chan struct{}, 1)
	ctrl, repo := frontRunFixture(t, func(ctx context.Context, opts adapter.RunOptions) (adapter.RunResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return adapter.RunResult{}, ctx.Err()
	})
	if err := ctrl.FrontRunStart(context.Background(), "burn", ""); err != nil {
		t.Fatal(err)
	}
	<-started
	if rec, _ := ctrl.store.GetChunk("F12"); rec.Status != "running" {
		t.Fatalf("claimed chunk should read running so the main lane skips it, got %s", rec.Status)
	}
	if err := ctrl.FrontRunStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec, _ := ctrl.store.GetChunk("F12"); rec.Status != "pending" {
		t.Errorf("after stop the chunk must return to pending, got %s", rec.Status)
	}
	if out := gitDo(t, repo, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("main tree must be untouched:\n%s", out)
	}
}

func TestFrontRun_WaitsWhileRAMIsLow(t *testing.T) {
	var runs atomic.Int32
	ctrl, _ := frontRunFixture(t, func(ctx context.Context, opts adapter.RunOptions) (adapter.RunResult, error) {
		runs.Add(1)
		return adapter.RunResult{ExitCode: 0}, nil
	})
	ctrl.slotsPool.Guard.RAMChecker = func() (float64, error) { return 0.5, nil }
	if err := ctrl.FrontRunStart(context.Background(), "burn", ""); err != nil {
		t.Fatal(err)
	}
	defer ctrl.FrontRunStop(context.Background())
	waitFor(t, "a low-RAM note", func() bool {
		st, _ := ctrl.Status(context.Background())
		return st.FrontRun != nil && strings.Contains(st.FrontRun.Note, "RAM")
	})
	time.Sleep(150 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatal("no chunk may start while RAM is below the floor plus margin")
	}
	if rec, _ := ctrl.store.GetChunk("F12"); rec.Status != "pending" {
		t.Errorf("F12 = %s, want pending", rec.Status)
	}
}

func TestFrontRun_FailureReturnsChunkAndLaneKeepsGoing(t *testing.T) {
	ctrl, _ := frontRunFixture(t, func(ctx context.Context, opts adapter.RunOptions) (adapter.RunResult, error) {
		return adapter.RunResult{ExitCode: 0, Output: "did nothing"}, nil // zero changes: verification fails
	})
	if err := ctrl.FrontRunStart(context.Background(), "burn", ""); err != nil {
		t.Fatal(err)
	}
	defer ctrl.FrontRunStop(context.Background())
	waitFor(t, "a failed attempt", func() bool {
		st, _ := ctrl.Status(context.Background())
		return st.FrontRun != nil && st.FrontRun.Failed >= 1
	})
	rec, _ := ctrl.store.GetChunk("F12")
	if rec.Status != "pending" {
		t.Errorf("a failed front-run must not block the chunk, got %s", rec.Status)
	}
}

func TestFrontRun_LockdownStopsTheLane(t *testing.T) {
	ctrl, _ := frontRunFixture(t, func(ctx context.Context, opts adapter.RunOptions) (adapter.RunResult, error) {
		<-ctx.Done()
		return adapter.RunResult{}, ctx.Err()
	})
	if err := ctrl.FrontRunStart(context.Background(), "burn", ""); err != nil {
		t.Fatal(err)
	}
	_ = ctrl.Lockdown(context.Background())
	if st, _ := ctrl.Status(context.Background()); st.FrontRun != nil {
		t.Errorf("lockdown must stop the lane, got %+v", st.FrontRun)
	}
}

// Merging touches HEAD, so it must wait while a main-queue chunk is in flight.
func TestFrontRun_LandingWaitsForTheMainQueueToBeBetweenChunks(t *testing.T) {
	ctrl, repo := frontRunFixture(t, func(ctx context.Context, opts adapter.RunOptions) (adapter.RunResult, error) {
		return adapter.RunResult{}, nil
	})
	wo := ctrl.workOrders["F12"]
	patch := "diff --git a/F12.txt b/F12.txt\nnew file mode 100644\nindex 0000000..e69de29\n--- /dev/null\n+++ b/F12.txt\n@@ -0,0 +1 @@\n+hi\n"
	ctrl.mu.Lock()
	ctrl.activeChunk = ctrl.workOrders["F01"] // the main queue is mid-chunk
	ctrl.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- ctrl.landFrontRun(context.Background(), &frontRunReady{wo: wo, patch: patch, tier: "burn", result: make(chan error, 1)})
	}()
	select {
	case err := <-done:
		t.Fatalf("landed while a main chunk was active (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(repo, "F12.txt")); err == nil {
		t.Fatal("patch applied to the main tree during a main chunk")
	}

	ctrl.mu.Lock()
	ctrl.activeChunk = nil
	ctrl.landFrontRunLocked() // what the main loop does at the top of each iteration
	ctrl.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("landing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "F12.txt")); err != nil {
		t.Fatalf("patch not applied once the main queue was between chunks: %v", err)
	}
}

// The lane polls the pool's 5-hour session bucket and starts nothing with under 15% left.
func TestFrontRun_HoldsBelow15PercentOfTheSessionBucket(t *testing.T) {
	var runs atomic.Int32
	ctrl, _ := frontRunFixture(t, func(ctx context.Context, opts adapter.RunOptions) (adapter.RunResult, error) {
		runs.Add(1)
		_ = os.WriteFile(filepath.Join(opts.WorktreeRoot, "F12.txt"), []byte("x\n"), 0644)
		return adapter.RunResult{ExitCode: 0}, nil
	})
	off := false
	ctrl.cfg.Tiers["burn"] = config.TierConfig{Adapter: "mock-test", Model: "big", Pool: "cc", Enabled: &off}
	ctrl.cfg.Quota.Pools = map[string]config.PoolConfig{"cc": {Mode: "polled", UsageCmd: []string{"claude", "-p", "/usage"}}}
	var pct atomic.Int32
	pct.Store(14)
	var polls atomic.Int32
	ctrl.frontRunPoll = func(_ context.Context, pool string, _ config.PoolConfig) ([]quota.Bucket, error) {
		polls.Add(1)
		return []quota.Bucket{
			{Pool: pool, Window: "weekly", RemainingPct: 90},
			{Pool: pool, Window: "5h", RemainingPct: int(pct.Load())},
		}, nil
	}
	if err := ctrl.FrontRunStart(context.Background(), "burn", ""); err != nil {
		t.Fatal(err)
	}
	defer ctrl.FrontRunStop(context.Background())

	waitFor(t, "a session-quota note", func() bool {
		st, _ := ctrl.Status(context.Background())
		return st.FrontRun != nil && strings.Contains(st.FrontRun.Note, "5h session quota") && strings.Contains(st.FrontRun.Note, "14%")
	})
	n := polls.Load()
	time.Sleep(150 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatal("no chunk may start with 14% of the session bucket left")
	}
	if polls.Load() <= n {
		t.Error("the lane should keep polling the bucket while it waits")
	}

	pct.Store(15) // exactly 15% is enough: the rule is "less than 15%"
	waitFor(t, "F12 once the bucket recovers", func() bool {
		rec, _ := ctrl.store.GetChunk("F12")
		return rec != nil && rec.Status == "done"
	})
}
