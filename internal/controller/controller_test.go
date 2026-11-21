package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/adapter"
	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/lan"
	"github.com/erinwnash/Shepherd/internal/notify"
	"github.com/erinwnash/Shepherd/internal/verify"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

type mockTestAdapter struct {
	onRun func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error)
}

func (m *mockTestAdapter) Name() string { return "mock-test" }
func (m *mockTestAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{HasUsage: true}
}
func (m *mockTestAdapter) Usage(_ context.Context, _ string) (string, error) {
	return "mock usage", nil
}
func (m *mockTestAdapter) Run(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
	if m.onRun != nil {
		return m.onRun(ctx, prompt, opts)
	}
	return adapter.RunResult{ExitCode: 0, Output: "ok"}, nil
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.local",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.local",
			"GIT_CONFIG_NOSYSTEM=1",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v (%s)", strings.Join(args, " "), err, string(out))
		}
	}

	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@test.local")
	run("config", "commit.gpgSign", "false")

	initialFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(initialFile, []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitignoreFile := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignoreFile, []byte(".shepherd/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md", ".gitignore")
	run("commit", "-m", "initial commit")
}

func setupTestController(t *testing.T, customAdapter adapter.Adapter) (*Controller, string) {
	t.Helper()
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	dbPath := filepath.Join(repoDir, ".shepherd", "state.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := &config.Config{
		SchemaVersion: 1,
		Project: config.ProjectConfig{
			Name: "test-proj",
		},
		Tiers: map[string]config.TierConfig{
			"primary": {Adapter: "mock-test"},
		},
		Policy: config.PolicyConfig{
			MaxConsecutiveBlocks: 2,
			CloudFirstThreshold:  0.8,
			AllowUnsandboxed:     true,
		},
	}

	adapters := map[string]adapter.Adapter{}
	if customAdapter != nil {
		adapters["mock-test"] = customAdapter
	} else {
		adapters["mock-test"] = &mockTestAdapter{}
	}

	ctrl, err := New(Options{
		ProjectRoot: repoDir,
		PlanRoot:    filepath.Join(repoDir, "docs", "plan"),
		Config:      cfg,
		Store:       store,
		Adapters:    adapters,
	})
	if err != nil {
		t.Fatalf("New controller: %v", err)
	}

	return ctrl, repoDir
}

func TestController_SuccessfulExecutionAndAuditLog(t *testing.T) {
	var targetFile string
	mockAd := &mockTestAdapter{
		onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
			targetFile = filepath.Join(opts.WorktreeRoot, "file.txt")
			_ = os.WriteFile(targetFile, []byte("hello world\n"), 0644)
			return adapter.RunResult{ExitCode: 0, Output: "created file.txt"}, nil
		},
	}

	ctrl, repoDir := setupTestController(t, mockAd)

	wo := &workorder.WorkOrder{
		ID:     "01A",
		Title:  "Create sample file",
		Sprint: "01",
		Status: workorder.StatusPending,
		Files: []workorder.File{
			{Path: "file.txt", New: true},
		},
		Body: "Create file.txt with hello world",
	}

	if err := ctrl.RegisterWorkOrder(wo); err != nil {
		t.Fatalf("RegisterWorkOrder: %v", err)
	}

	ctx := context.Background()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Poll until chunk is done
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, _ := ctrl.store.GetChunk("01A")
		if rec != nil && rec.Status == "done" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	rec, err := ctrl.store.GetChunk("01A")
	if err != nil || rec.Status != "done" {
		t.Fatalf("expected chunk status 'done', got %v (err: %v)", rec, err)
	}

	// Verify file was committed to git
	data, err := os.ReadFile(filepath.Join(repoDir, "file.txt"))
	if err != nil || string(data) != "hello world\n" {
		t.Fatalf("unexpected committed file content: %s (err: %v)", string(data), err)
	}

	// Verify hash-chained audit log
	events, err := ctrl.Audit(ctx, 50)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	hasStarted, hasPassed := false, false
	for _, ev := range events {
		if ev.Type == "chunk.started" {
			hasStarted = true
		}
		if ev.Type == "chunk.passed" {
			hasPassed = true
		}
	}
	if !hasStarted || !hasPassed {
		t.Errorf("expected chunk.started and chunk.passed in audit log, got: %+v", events)
	}

	// Assert hash chain integrity (STRIDE T6)
	if err := ctrl.store.EventLog.Verify(); err != nil {
		t.Fatalf("audit log integrity verification failed: %v", err)
	}
}

func TestController_HaltAndRollback(t *testing.T) {
	startedCh := make(chan struct{})
	unblockCh := make(chan struct{})
	var once sync.Once

	mockAd := &mockTestAdapter{
		onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
			// Write a dirty file before halting
			dirtyFile := filepath.Join(opts.WorktreeRoot, "dirty.txt")
			_ = os.WriteFile(dirtyFile, []byte("uncommitted change"), 0644)

			once.Do(func() {
				close(startedCh)
			})
			select {
			case <-ctx.Done():
				return adapter.RunResult{}, ctx.Err()
			case <-unblockCh:
				return adapter.RunResult{ExitCode: 0}, nil
			}
		},
	}

	ctrl, repoDir := setupTestController(t, mockAd)

	wo := &workorder.WorkOrder{
		ID:     "02A",
		Title:  "Long running chunk",
		Status: workorder.StatusPending,
		Files:  []workorder.File{{Path: "dirty.txt", New: true}},
	}
	_ = ctrl.RegisterWorkOrder(wo)

	ctx := context.Background()
	_ = ctrl.Start(ctx)

	// Wait until adapter is executing
	<-startedCh

	// Issue Halt
	if err := ctrl.Halt(ctx, "manual test abort"); err != nil {
		t.Fatalf("Halt failed: %v", err)
	}

	// Verify worktree rollback: dirty.txt must be removed
	dirtyFile := filepath.Join(repoDir, "dirty.txt")
	if _, err := os.Stat(dirtyFile); !os.IsNotExist(err) {
		t.Fatalf("expected dirty.txt to be removed by git rollback, but it still exists")
	}

	// Verify chunk was requeued as pending
	rec, err := ctrl.store.GetChunk("02A")
	if err != nil || rec.Status != "pending" {
		t.Fatalf("expected chunk status 'pending' (requeued), got %v", rec)
	}

	// Verify audit event
	events, _ := ctrl.Audit(ctx, 10)
	foundHalt := false
	for _, ev := range events {
		if ev.Type == "chunk.halted" {
			foundHalt = true
			break
		}
	}
	if !foundHalt {
		t.Errorf("chunk.halted event not found in audit log")
	}
}

func TestController_PauseResumeDrainLockdown(t *testing.T) {
	ctrl, _ := setupTestController(t, nil)
	ctx := context.Background()

	// Pause / Resume
	if err := ctrl.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	st, _ := ctrl.Status(ctx)
	if st.State != StatePaused {
		t.Fatalf("expected state 'paused', got %s", st.State)
	}

	if err := ctrl.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	st, _ = ctrl.Status(ctx)
	if st.State != StateRunning && st.State != StateIdle {
		t.Fatalf("expected running or idle, got %s", st.State)
	}

	// Drain
	if err := ctrl.Drain(ctx, true, nil); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	st, _ = ctrl.Status(ctx)
	if !st.DrainMode {
		t.Fatalf("expected drain = true")
	}

	// Lockdown
	if err := ctrl.Lockdown(ctx); err != nil {
		t.Fatalf("Lockdown: %v", err)
	}
	st, _ = ctrl.Status(ctx)
	if !st.Lockdown || st.State != StateLockdown {
		t.Fatalf("expected lockdown state, got %+v", st)
	}

	// Cannot start while in lockdown
	if err := ctrl.Start(ctx); err == nil {
		t.Fatal("expected Start() to fail in lockdown")
	}

	// Unlock
	if err := ctrl.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	st, _ = ctrl.Status(ctx)
	if st.Lockdown {
		t.Fatal("expected lockdown = false after unlock")
	}
}

func TestController_LiveConfigOverrides(t *testing.T) {
	ctrl, _ := setupTestController(t, nil)
	ctx := context.Background()

	// Setting invalid key fails
	if err := ctrl.ConfigSet(ctx, "invalid.key", "val"); err == nil {
		t.Fatal("expected error for invalid key")
	}

	// Setting valid override
	if err := ctrl.ConfigSet(ctx, "policy.max_consecutive_blocks", "5"); err != nil {
		t.Fatalf("ConfigSet failed: %v", err)
	}

	// Overrides are stored
	overrides, err := ctrl.ConfigGet(ctx)
	if err != nil || overrides["policy.max_consecutive_blocks"] != "5" {
		t.Fatalf("expected override 5, got %v", overrides)
	}

	// Reload config applies it to memory
	if err := ctrl.ConfigReload(ctx); err != nil {
		t.Fatalf("ConfigReload: %v", err)
	}
	if ctrl.cfg.Policy.MaxConsecutiveBlocks != 5 {
		t.Fatalf("expected memory config to update to 5, got %d", ctrl.cfg.Policy.MaxConsecutiveBlocks)
	}
}

func TestController_RemoteWorkerMergeAndOverlap(t *testing.T) {
	ctrl, repoDir := setupTestController(t, nil)
	ctx := context.Background()

	// Chunk A touches shared.go
	woA := &workorder.WorkOrder{
		ID:     "chunk-a",
		Title:  "Feature A",
		Status: workorder.StatusPending,
		Files:  []workorder.File{{Path: "shared.go", New: true}},
	}
	// Chunk B also touches shared.go (overlapping file set!)
	woB := &workorder.WorkOrder{
		ID:     "chunk-b",
		Title:  "Feature B",
		Status: workorder.StatusPending,
		Files:  []workorder.File{{Path: "shared.go", New: false}},
	}

	_ = ctrl.RegisterWorkOrder(woA)
	_ = ctrl.RegisterWorkOrder(woB)

	// Remote worker 1 fetches assignment - receives Chunk A
	assignA, err := ctrl.FetchAssignment(ctx, "worker-1", "chunk-a")
	if err != nil {
		t.Fatalf("FetchAssignment chunk-a: %v", err)
	}
	if assignA.ChunkID != "chunk-a" || assignA.WorkerID != "worker-1" {
		t.Fatalf("Unexpected assignment: %+v", assignA)
	}

	// While Chunk A is running, Chunk B cannot be fetched via next runnable because files overlap (Brief §4)
	ctrl.mu.Lock()
	next := ctrl.nextRunnableChunkLocked()
	ctrl.mu.Unlock()
	if next != nil && next.ID == "chunk-b" {
		t.Fatalf("Expected chunk-b to be serialized due to file overlap with running chunk-a")
	}

	// Create and write shared.go in repo
	sharedFile := filepath.Join(repoDir, "shared.go")
	_ = os.WriteFile(sharedFile, []byte("package test\nfunc Foo() {}\n"), 0644)

	// Remote worker submits result for Chunk A
	err = ctrl.SubmitResult(ctx, "worker-1", lan.ResultRequest{
		ChunkID:    "chunk-a",
		BaseCommit: assignA.BaseCommit,
		Status:     "done",
	})
	if err != nil {
		t.Fatalf("SubmitResult chunk-a failed: %v", err)
	}

	// Verify Chunk A is marked done
	recA, _ := ctrl.store.GetChunk("chunk-a")
	if recA.Status != "done" {
		t.Fatalf("Expected chunk-a status 'done', got %q", recA.Status)
	}

	// Now that Chunk A is done, Chunk B is no longer blocked by file overlap
	ctrl.mu.Lock()
	nextAfter := ctrl.nextRunnableChunkLocked()
	ctrl.mu.Unlock()
	if nextAfter == nil || nextAfter.ID != "chunk-b" {
		t.Fatalf("Expected chunk-b to now be runnable after chunk-a completed, got %v", nextAfter)
	}
}

// Threat Model T7: Merge of unreviewed HIGH-risk chunk -> held for human sign-off.
func TestController_ReviewTieringPolicy_STRIDE_T7(t *testing.T) {
	ctrl, repoDir := setupTestController(t, nil)
	ctrl.cfg.Policy.RequireHighReviewHuman = true

	ctx := context.Background()
	woHigh := &workorder.WorkOrder{
		ID:     "chunk-high",
		Title:  "Security Critical Auth Change",
		Review: workorder.ReviewHigh,
		Status: workorder.StatusPending,
		Files:  []workorder.File{{Path: "auth.go", New: true}},
	}
	_ = ctrl.RegisterWorkOrder(woHigh)

	authFile := filepath.Join(repoDir, "auth.go")
	_ = os.WriteFile(authFile, []byte("package test\nfunc Auth() bool { return true }\n"), 0644)

	// Fetch assignment
	assign, err := ctrl.FetchAssignment(ctx, "worker-high", "chunk-high")
	if err != nil {
		t.Fatalf("FetchAssignment failed: %v", err)
	}

	// Submit result as done
	err = ctrl.SubmitResult(ctx, "worker-high", lan.ResultRequest{
		ChunkID:    "chunk-high",
		BaseCommit: assign.BaseCommit,
		Status:     "done",
	})
	if err != nil {
		t.Fatalf("SubmitResult failed: %v", err)
	}

	// Must be held, not auto-merged
	rec, err := ctrl.store.GetChunk("chunk-high")
	if err != nil {
		t.Fatalf("GetChunk: %v", err)
	}
	if rec.Status != "held" {
		t.Fatalf("Expected HIGH review chunk to be held for human review (STRIDE T7), got %q", rec.Status)
	}
}

func TestController_NotificationsOnHeldAndBlocked(t *testing.T) {
	var (
		notifyMu       sync.Mutex
		notifiedEvents []string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		notifyMu.Lock()
		notifiedEvents = append(notifiedEvents, payload.Text)
		notifyMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	mockAd := &mockTestAdapter{
		onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
			return adapter.RunResult{ExitCode: 1, Output: "compilation failed"}, errors.New("exit 1")
		},
	}

	ctrl, _ := setupTestController(t, mockAd)
	ctrl.cfg.Notify = config.NotifyConfig{
		Events:          []string{"chunk_held", "queue_blocked"},
		SlackWebhookURL: ts.URL,
	}
	ctrl.notifier = notify.NewDispatcher(notify.Config{
		Events:          ctrl.cfg.Notify.Events,
		SlackWebhookURL: ctrl.cfg.Notify.SlackWebhookURL,
	})

	wo := &workorder.WorkOrder{
		ID:     "fail-chunk",
		Title:  "Broken chunk",
		Status: workorder.StatusPending,
		Files:  []workorder.File{{Path: "broken.go", New: true}},
	}
	_ = ctrl.RegisterWorkOrder(wo)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = ctrl.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	rec, _ := ctrl.store.GetChunk("fail-chunk")
	if rec.Status != "blocked" {
		t.Fatalf("expected blocked chunk, got %s", rec.Status)
	}

	notifyMu.Lock()
	defer notifyMu.Unlock()
	if len(notifiedEvents) == 0 {
		t.Errorf("expected notification to be sent to webhook on chunk blocked, got none")
	}
}

func TestController_DynamicTierEscalationAndCascadingRepair(t *testing.T) {
	runCount := 0
	var receivedPrompts []string

	mockAd := &mockTestAdapter{
		onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
			runCount++
			receivedPrompts = append(receivedPrompts, prompt)

			targetFile := filepath.Join(opts.WorktreeRoot, "math.go")
			if runCount == 1 {
				// Attempt 1: write buggy implementation that fails gate test
				_ = os.WriteFile(targetFile, []byte("package math\nfunc Add(a, b int) int { return a - b }\n"), 0644)
				return adapter.RunResult{ExitCode: 0}, nil
			}

			// Attempt 2 (Repair): write correct implementation that passes gate test
			_ = os.WriteFile(targetFile, []byte("package math\nfunc Add(a, b int) int { return a + b }\n"), 0644)
			return adapter.RunResult{ExitCode: 0}, nil
		},
	}

	ctrl, repoDir := setupTestController(t, mockAd)

	// Configure gate that checks Add output
	ctrl.verifier = verify.NewVerifier([]verify.GateConfig{
		{
			Name: "test-addition",
			Argv: []string{"grep", "return a + b", "math.go"},
			Hard: true,
		},
	})

	wo := &workorder.WorkOrder{
		ID:     "08A",
		Title:  "Implement Math Function",
		Status: workorder.StatusPending,
		Files:  []workorder.File{{Path: "math.go", New: true}},
		Body:   "## Goal\nImplement Add function.\n",
	}
	_ = ctrl.RegisterWorkOrder(wo)

	ctx := context.Background()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, _ := ctrl.store.GetChunk("08A")
		if rec != nil && rec.Status == "done" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	rec, err := ctrl.store.GetChunk("08A")
	if err != nil || rec.Status != "done" {
		t.Fatalf("expected chunk status 'done' after repair, got %+v (err: %v)", rec, err)
	}

	if runCount != 2 {
		t.Errorf("expected 2 runs (1 initial + 1 repair), got %d", runCount)
	}

	// Verify repair prompt included diagnostic history
	if len(receivedPrompts) >= 2 {
		if !strings.Contains(receivedPrompts[1], "Earlier attempts on this chunk") {
			t.Errorf("expected repair prompt to include diagnostic history, got:\n%s", receivedPrompts[1])
		}
	}

	// Verify git commit was created with repair message
	git := verify.NewGitRunner(repoDir)
	logOut, _ := git.Exec(ctx, "log", "-1", "--pretty=%s")
	if !strings.Contains(logOut, "repair verification failures on attempt 2") {
		t.Errorf("expected commit message to mention repair attempt 2, got: %s", logOut)
	}
}

func TestController_ValidateTokenSecurity(t *testing.T) {
	ctrl, _ := setupTestController(t, &mockTestAdapter{})
	ctx := context.Background()

	// 1. Empty token must NEVER be accepted even when env vars are unset
	t.Setenv("SHEPHERD_ADMIN_TOKEN", "")
	t.Setenv("SHEPHERD_OPERATOR_TOKEN", "")
	t.Setenv("SHEPHERD_VIEWER_TOKEN", "")

	if role, err := ctrl.ValidateToken(ctx, ""); err == nil {
		t.Fatalf("expected empty token to be rejected, got role %q", role)
	}
	if role, err := ctrl.ValidateToken(ctx, "   "); err == nil {
		t.Fatalf("expected whitespace token to be rejected, got role %q", role)
	}

	// 2. When custom env var is set, default "admin" fallback must be disabled
	t.Setenv("SHEPHERD_ADMIN_TOKEN", "s3cr3t-admin-tok")
	if role, err := ctrl.ValidateToken(ctx, "admin"); err == nil {
		t.Fatalf("expected fallback 'admin' token to be rejected when SHEPHERD_ADMIN_TOKEN is set, got %q", role)
	}
	if role, err := ctrl.ValidateToken(ctx, "s3cr3t-admin-tok"); err != nil || role != "admin" {
		t.Fatalf("expected custom admin token to succeed, got role=%q err=%v", role, err)
	}
}

func TestController_CostCircuitBreaker(t *testing.T) {
	mockAd := &mockTestAdapter{
		onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
			_ = os.WriteFile(filepath.Join(opts.WorktreeRoot, "expensive.go"), []byte("package main\n"), 0644)
			return adapter.RunResult{
				ExitCode:   0,
				TokensUsed: 50000,
				CostUSD:    3.50,
			}, nil
		},
	}

	ctrl, _ := setupTestController(t, mockAd)
	ctrl.cfg.Policy.MaxChunkCostUSD = 2.00 // Limit is $2.00, run costs $3.50

	wo := &workorder.WorkOrder{
		ID:     "cost-chunk",
		Title:  "Expensive Chunk",
		Sprint: "sprint-1",
		Status: workorder.StatusPending,
		Files:  []workorder.File{{Path: "expensive.go", New: true}},
	}
	_ = ctrl.RegisterWorkOrder(wo)

	ctx := context.Background()
	_ = ctrl.Start(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rec, _ := ctrl.store.GetChunk("cost-chunk")
		if rec != nil && rec.Status == "blocked" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	rec, err := ctrl.store.GetChunk("cost-chunk")
	if err != nil || rec.Status != "blocked" {
		t.Fatalf("expected chunk to be blocked by cost circuit breaker, got %+v", rec)
	}
	if !strings.Contains(rec.BlockedReason, "chunk cost circuit breaker tripped") {
		t.Errorf("expected circuit breaker reason, got %q", rec.BlockedReason)
	}

	st, err := ctrl.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.TotalCostUSD < 3.49 || st.TotalTokens != 50000 {
		t.Errorf("expected status telemetry cost=3.50 tokens=50000, got cost=%.2f tokens=%d", st.TotalCostUSD, st.TotalTokens)
	}
}

func TestController_QueueDiffApproveReject(t *testing.T) {
	ctrl, repoDir := setupTestController(t, &mockTestAdapter{})
	ctrl.cfg.Policy.RequireHighReviewHuman = true
	ctx := context.Background()

	wo := &workorder.WorkOrder{
		ID:     "review-1",
		Title:  "Auth Security Policy Update",
		Status: workorder.StatusPending,
		Review: workorder.ReviewHigh,
		Files:  []workorder.File{{Path: "policy.txt", New: true}},
	}
	_ = ctrl.RegisterWorkOrder(wo)

	assign, err := ctrl.FetchAssignment(ctx, "worker-1", "review-1")
	if err != nil {
		t.Fatalf("FetchAssignment: %v", err)
	}

	patch := `diff --git a/policy.txt b/policy.txt
new file mode 100644
index 0000000..e69de29
--- /dev/null
+++ b/policy.txt
@@ -0,0 +1 @@
+strict-mode=true
`
	if err := ctrl.SubmitResult(ctx, "worker-1", lan.ResultRequest{
		ChunkID:    "review-1",
		BaseCommit: assign.BaseCommit,
		Status:     "done",
		Patch:      patch,
	}); err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}

	// Verify chunk is held and diff is retrievable via QueueDiff
	diffOut, err := ctrl.QueueDiff(ctx, "review-1")
	if err != nil {
		t.Fatalf("QueueDiff: %v", err)
	}
	if !strings.Contains(diffOut, "strict-mode=true") {
		t.Fatalf("expected QueueDiff to return held patch, got: %s", diffOut)
	}

	// Approve the held chunk
	if err := ctrl.QueueApprove(ctx, "review-1"); err != nil {
		t.Fatalf("QueueApprove: %v", err)
	}

	rec, _ := ctrl.store.GetChunk("review-1")
	if rec.Status != "done" {
		t.Fatalf("expected chunk status 'done' after QueueApprove, got %q", rec.Status)
	}

	// Verify file was applied and committed
	data, err := os.ReadFile(filepath.Join(repoDir, "policy.txt"))
	if err != nil || !strings.Contains(string(data), "strict-mode=true") {
		t.Fatalf("expected policy.txt on disk after approval, got %q (err: %v)", string(data), err)
	}
}

func TestController_MarkSourceDone_DoneDir(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	planDir := filepath.Join(repoDir, "docs", "plan", "S01")
	if err := os.MkdirAll(planDir, 0755); err != nil {
		t.Fatal(err)
	}
	woPath := filepath.Join(planDir, "01A_init.md")
	woContent := `---
id: 01A
title: Init feature
epic: Core
sprint: S01
status: pending
---
## Goal
Initial goal
## Out of scope
Nothing
## Acceptance
Pass
`
	if err := os.WriteFile(woPath, []byte(woContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		SchemaVersion: 1,
		Project:       config.ProjectConfig{Name: "test-done", Repo: ".", BaseBranch: "main"},
		Queue: config.QueueConfig{
			Source:     "markdown-dir",
			Root:       "docs/plan",
			DoneMarker: "move-to-done",
			DoneDir:    "docs/plan/_done",
		},
	}
	store, _ := db.Open(":memory:")
	defer store.Close()

	ctrl, err := New(Options{
		ProjectRoot: repoDir,
		Config:      cfg,
		Store:       store,
	})
	if err != nil {
		t.Fatal(err)
	}

	wo := &workorder.WorkOrder{
		ID:     "01A",
		Path:   "docs/plan/S01/01A_init.md",
		Status: workorder.StatusPending,
	}
	ctrl.markSourceDone(wo)

	// Old file should have been moved
	if _, err := os.Stat(woPath); !os.IsNotExist(err) {
		t.Errorf("expected original file %s to be moved, but it still exists", woPath)
	}

	// New file should exist under DoneDir with status: done
	destPath := filepath.Join(repoDir, "docs", "plan", "_done", "01A_init.md")
	destData, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("expected dest file %s to exist: %v", destPath, err)
	}
	if !strings.Contains(string(destData), "status: done") {
		t.Errorf("expected status: done in moved file, got:\n%s", string(destData))
	}
}

func TestMarkSourceDoneDefaultDir(t *testing.T) {
	repoDir := t.TempDir()
	planDir := filepath.Join(repoDir, "docs", "plan")
	if err := os.MkdirAll(planDir, 0755); err != nil {
		t.Fatal(err)
	}

	woPath := filepath.Join(planDir, "02A_task.md")
	woContent := `---
id: 02A
title: Default Done Dir
status: in-progress
---
# 02A -- Default Done Dir
## Context
Test default
## Requirements
1. Move to _done
`
	if err := os.WriteFile(woPath, []byte(woContent), 0644); err != nil {
		t.Fatal(err)
	}

	// DoneDir is empty - should default to docs/planDONE
	cfg := &config.Config{
		SchemaVersion: 1,
		Project:       config.ProjectConfig{Name: "test-default-done", Repo: ".", BaseBranch: "main"},
		Queue: config.QueueConfig{
			Source:     "markdown-dir",
			Root:       "docs/plan",
			DoneMarker: "move-to-done",
		},
	}
	store, _ := db.Open(":memory:")
	defer store.Close()

	ctrl, err := New(Options{
		ProjectRoot: repoDir,
		PlanRoot:    "docs/plan",
		Config:      cfg,
		Store:       store,
	})
	if err != nil {
		t.Fatal(err)
	}

	wo := &workorder.WorkOrder{
		ID:     "02A",
		Path:   "docs/plan/02A_task.md",
		Status: workorder.StatusRunning,
	}
	ctrl.markSourceDone(wo)

	// Old file should be gone from docs/plan/
	if _, err := os.Stat(woPath); !os.IsNotExist(err) {
		t.Errorf("expected original file %s to be moved, but it still exists", woPath)
	}

	// New file should exist under docs/planDONE/ with status: done
	destPath := filepath.Join(repoDir, "docs", "planDONE", "02A_task.md")
	destData, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("expected dest file %s to exist: %v", destPath, err)
	}
	if !strings.Contains(string(destData), "status: done") {
		t.Errorf("expected status: done in moved file, got:\n%s", string(destData))
	}
}

func TestController_Notifications_BootChunkStartModelSwitchPass(t *testing.T) {
	type receivedNotification struct {
		Title    string
		Priority string
		Tags     string
		Body     string
	}
	var (
		mu       sync.Mutex
		received []receivedNotification
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		received = append(received, receivedNotification{
			Title:    r.Header.Get("Title"),
			Priority: r.Header.Get("Priority"),
			Tags:     r.Header.Get("Tags"),
			Body:     string(buf[:n]),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	runCount := 0
	mockAd := &mockTestAdapter{
		onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
			runCount++
			targetFile := filepath.Join(opts.WorktreeRoot, "notif_test.go")
			if runCount == 1 {
				// Attempt 1: fail gate test
				_ = os.WriteFile(targetFile, []byte("package ntest\nfunc Value() int { return 0 }\n"), 0644)
			} else {
				// Attempt 2 (Repair): pass gate test
				_ = os.WriteFile(targetFile, []byte("package ntest\nfunc Value() int { return 42 }\n"), 0644)
			}
			return adapter.RunResult{ExitCode: 0}, nil
		},
	}

	ctrl, _ := setupTestController(t, mockAd)
	ctrl.cfg.Notify = config.NotifyConfig{
		NtfyURL: ts.URL,
	}
	ctrl.notifier = notify.NewDispatcher(notify.Config{
		NtfyURL: ts.URL,
	})

	ctrl.cfg.Tiers = map[string]config.TierConfig{
		"primary": {Adapter: "mock-test", Model: "primary-model"},
		"strong":  {Adapter: "mock-test", Model: "strong-model"},
	}
	ctrl.cfg.Repair.Chain = []string{"primary:1", "strong:2"}
	ctrl.cfg.Repair.MechanicalFix = config.MechanicalFixConfig{
		Tier:  "strong",
		Codes: []string{"gate-failed"},
	}

	ctrl.verifier = verify.NewVerifier([]verify.GateConfig{
		{
			Name: "test-gate",
			Argv: []string{"sh", "-c", "grep 'return 42' notif_test.go || (echo 'FAIL: test failed: expected 42, got 0' >&2 && exit 1)"},
			Hard: true,
		},
	})

	wo := &workorder.WorkOrder{
		ID:     "10A",
		Title:  "Notification Flow Test",
		Status: workorder.StatusPending,
		Files:  []workorder.File{{Path: "notif_test.go", New: true}},
		Body:   "## Goal\nReturn 42.\n",
	}
	_ = ctrl.RegisterWorkOrder(wo)

	ctx := context.Background()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, _ := ctrl.store.GetChunk("10A")
		if rec != nil && rec.Status == "done" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Wait for trailing notifications: ntfy posts are spaced 1s apart to preserve order.
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		n := len(received)
		last := ""
		if n > 0 {
			last = received[n-1].Title
		}
		mu.Unlock()
		if strings.Contains(last, "Chunk Passed: 10A") {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()

	var (
		foundBoot        bool
		foundChunkStart  bool
		foundModelSwitch bool
		foundChunkPass   bool
	)

	for _, n := range received {
		if strings.Contains(n.Title, "Shepherd Started") {
			foundBoot = true
		}
		if strings.Contains(n.Title, "Chunk Started: 10A") && strings.Contains(n.Body, "model:") {
			foundChunkStart = true
		}
		if strings.Contains(n.Title, "Model Switch: 10A") && strings.Contains(n.Body, "strong-model") {
			foundModelSwitch = true
		}
		if strings.Contains(n.Title, "Chunk Passed: 10A") {
			foundChunkPass = true
		}
	}

	if !foundBoot {
		t.Errorf("expected Shepherd Started notification, got: %+v", received)
	}
	if !foundChunkStart {
		t.Errorf("expected Chunk Started: 10A notification with model info, got: %+v", received)
	}
	if !foundModelSwitch {
		t.Errorf("expected Model Switch: 10A notification with new model info, got: %+v", received)
	}
	if !foundChunkPass {
		t.Errorf("expected Chunk Passed: 10A notification, got: %+v", received)
	}
}

func TestController_NoCloudMode(t *testing.T) {
	var executedTier string
	var executedTimeout time.Duration

	mockAd := &mockTestAdapter{
		onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
			executedTimeout = opts.Timeout
			return adapter.RunResult{ExitCode: 0, Output: "done"}, nil
		},
	}

	ctrl, repoDir := setupTestController(t, mockAd)
	ctrl.noCloud = true
	ctrl.adapters["opencode"] = mockAd
	ctrl.adapters["agy"] = mockAd
	ctrl.cfg.Tiers["local"] = config.TierConfig{
		Adapter: "opencode",
		Pool:    "local",
	}
	ctrl.cfg.Tiers["strong"] = config.TierConfig{
		Adapter: "agy",
		Pool:    "claude",
	}
	ctrl.cfg.Repair.Chain = []string{"strong:2", "local:1"}

	// High complexity chunk that would normally route to strong cloud
	wo := &workorder.WorkOrder{
		ID:     "complex-1",
		Title:  "High complexity chunk",
		Status: workorder.StatusPending,
		Review: workorder.ReviewHigh,
		Effort: workorder.EffortL,
		Files:  []workorder.File{{Path: "complex.go", New: true}},
	}
	_ = ctrl.RegisterWorkOrder(wo)

	// Create test file so verification sees changes
	_ = os.WriteFile(filepath.Join(repoDir, "complex.go"), []byte("package main\n"), 0644)

	err := ctrl.executeChunk(wo)
	if err != nil {
		t.Fatalf("executeChunk failed: %v", err)
	}

	rec, _ := ctrl.store.GetChunk("complex-1")
	executedTier = rec.Tier
	if executedTier != "local" {
		t.Errorf("expected chunk to execute on local under --nocloud, got tier=%s", executedTier)
	}
	if executedTimeout != 0 {
		t.Errorf("expected timeout for local to be switched off (0), got: %v", executedTimeout)
	}
}

func TestController_StopgapLocalFallbackAndRecovery(t *testing.T) {
	var executedTimeout time.Duration

	mockAd := &mockTestAdapter{
		onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
			executedTimeout = opts.Timeout
			return adapter.RunResult{ExitCode: 0, Output: "done"}, nil
		},
	}

	ctrl, repoDir := setupTestController(t, mockAd)
	ctrl.adapters["opencode"] = mockAd
	ctrl.adapters["agy"] = mockAd
	ctrl.cfg.Tiers["local"] = config.TierConfig{
		Adapter: "opencode",
		Pool:    "local",
	}
	ctrl.cfg.Tiers["sonnet"] = config.TierConfig{
		Adapter: "agy",
		Model:   "claude-sonnet-5-5-medium",
		Pool:    "claude",
	}
	ctrl.cfg.Repair.Chain = []string{"sonnet:2", "local:1"}

	// Set cloud pool with 0% remaining (exhausted)
	if ctrl.cfg.Quota.Pools == nil {
		ctrl.cfg.Quota.Pools = make(map[string]config.PoolConfig)
	}
	ctrl.cfg.Quota.Pools["claude"] = config.PoolConfig{
		Mode:           "polled",
		FloorWeeklyPct: 25,
		UsageCmd:       []string{"echo", "test"},
		Parser:         "agy-usage",
	}

	// High complexity chunk
	wo := &workorder.WorkOrder{
		ID:     "complex-stopgap",
		Title:  "High complexity chunk in stopgap",
		Status: workorder.StatusPending,
		Review: workorder.ReviewHigh,
		Effort: workorder.EffortL,
		Files:  []workorder.File{{Path: "stopgap.go", New: true}},
	}
	_ = ctrl.RegisterWorkOrder(wo)

	_ = os.WriteFile(filepath.Join(repoDir, "stopgap.go"), []byte("package main\n"), 0644)

	err := ctrl.executeChunk(wo)
	if err != nil {
		t.Fatalf("executeChunk failed: %v", err)
	}

	rec, _ := ctrl.store.GetChunk("complex-stopgap")
	if rec.Tier != "local" {
		t.Errorf("expected chunk to fall back to local when cloud exhausted, got tier=%s", rec.Tier)
	}
	if executedTimeout != 0 {
		t.Errorf("expected local timeout to be switched off in stopgap mode, got: %v", executedTimeout)
	}

	// Now simulate cloud bucket recovery: pool outputs 80% remaining (> 25% floor)
	recoveredOutput := "Claude and GPT models\tWeekly window\t80%\t2026-10-24T00:00:00Z\n"
	ctrl.cfg.Quota.Pools["claude"] = config.PoolConfig{
		Mode:           "polled",
		FloorWeeklyPct: 25,
		UsageCmd:       []string{"printf", "%s", recoveredOutput},
		Parser:         "agy-usage",
	}

	// Trigger post-chunk pool polling / recovery check
	recovered := ctrl.checkAndRecoverCloudPools(context.Background())
	if !recovered {
		t.Errorf("expected cloud pool to recover when bucket remaining pct > floor")
	}

	// Verify next chunk restores cloud tier routing and reinstitutes timeouts
	wo2 := &workorder.WorkOrder{
		ID:     "complex-after-recovery",
		Title:  "Chunk after recovery",
		Status: workorder.StatusPending,
		Review: workorder.ReviewHigh,
		Effort: workorder.EffortL,
		Files:  []workorder.File{{Path: "recovery.go", New: true}},
	}
	_ = ctrl.RegisterWorkOrder(wo2)
	_ = os.WriteFile(filepath.Join(repoDir, "recovery.go"), []byte("package main\n"), 0644)

	err = ctrl.executeChunk(wo2)
	if err != nil {
		t.Fatalf("executeChunk after recovery failed: %v", err)
	}

	rec2, _ := ctrl.store.GetChunk("complex-after-recovery")
	if rec2.Tier != "sonnet" {
		t.Errorf("expected chunk after recovery to route back to cloud sonnet, got tier=%s", rec2.Tier)
	}
}

// Abuse case: a full stop (lockdown) with parallel slots must cancel every in-flight chunk,
// not just the first one in the batch.
func TestController_LockdownHaltsAllParallelChunks(t *testing.T) {
	var (
		mu       sync.Mutex
		started  int
		canceled int
	)
	allStarted := make(chan struct{})
	mockAd := &mockTestAdapter{
		onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
			mu.Lock()
			started++
			if started == 2 {
				close(allStarted)
			}
			mu.Unlock()
			select {
			case <-ctx.Done():
				mu.Lock()
				canceled++
				mu.Unlock()
				return adapter.RunResult{}, ctx.Err()
			case <-time.After(10 * time.Second):
				return adapter.RunResult{ExitCode: 0}, nil
			}
		},
	}
	ctrl, repoDir := setupTestController(t, mockAd)
	ctrl.cfg.Slots.MaxSlots = 2

	for _, id := range []string{"P1", "P2"} {
		_ = ctrl.RegisterWorkOrder(&workorder.WorkOrder{
			ID: id, Title: "parallel " + id, Status: workorder.StatusPending,
			Files: []workorder.File{{Path: id + ".txt", New: true}},
		})
	}
	ctx := context.Background()
	_ = ctrl.Start(ctx)

	select {
	case <-allStarted:
	case <-time.After(5 * time.Second):
		t.Skip("parallel slots unavailable on this host (hardware guard)")
	}
	_ = os.WriteFile(filepath.Join(repoDir, "P1.txt"), []byte("in flight"), 0o644)

	if err := ctrl.Lockdown(ctx); err != nil {
		t.Fatalf("Lockdown: %v", err)
	}

	mu.Lock()
	if canceled != 2 {
		mu.Unlock()
		t.Fatalf("expected both parallel runs canceled, got %d", canceled)
	}
	mu.Unlock()
	for _, id := range []string{"P1", "P2"} {
		rec, err := ctrl.store.GetChunk(id)
		if err != nil || rec.Status != "pending" {
			t.Fatalf("%s: expected requeued pending, got %+v", id, rec)
		}
	}
	if _, err := os.Stat(filepath.Join(repoDir, "P1.txt")); !os.IsNotExist(err) {
		t.Fatal("in-flight file not rolled back")
	}
}

// A daemon that died mid-chunk leaves rows in "running"; nothing will ever finish them, and
// every dependent chunk is unschedulable. Startup must requeue them unless a live worker holds a lease.
func TestController_ReconcileOrphanedRuns(t *testing.T) {
	ctrl, _ := setupTestController(t, nil)
	for _, id := range []string{"OR1", "OR2", "OR3"} {
		_ = ctrl.RegisterWorkOrder(&workorder.WorkOrder{ID: id, Title: id, Status: workorder.StatusPending})
	}
	_ = ctrl.store.SetChunkStatus("OR1", "running", "", "local-worker-1", "codex")
	_ = ctrl.store.SetChunkStatus("OR2", "running", "", "lan-live", "codex")
	_ = ctrl.store.SetChunkStatus("OR3", "running", "", "lan-gone", "codex")
	if err := ctrl.store.CreateLease(db.LeaseRecord{
		ID: "L1", WorkerID: "lan-live", ChunkID: "OR2", Pool: "codex", EstimatedPct: 5,
		Status: "active", ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	reset, err := ctrl.ReconcileOrphanedRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(reset, ",") != "OR1,OR3" {
		t.Fatalf("reset = %v, want OR1,OR3", reset)
	}
	for id, want := range map[string]string{"OR1": "pending", "OR2": "running", "OR3": "pending"} {
		rec, _ := ctrl.store.GetChunk(id)
		if rec.Status != want {
			t.Errorf("%s: status %q, want %q", id, rec.Status, want)
		}
	}
	events, _ := ctrl.Audit(context.Background(), 20)
	n := 0
	for _, ev := range events {
		if ev.Type == "chunk.orphan_requeued" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("want 2 chunk.orphan_requeued events, got %d", n)
	}
}

// With pending work but nothing schedulable the loop used to go idle silently. It must say
// why (root blockers) once per distinct stall, not on every pass.
func TestController_QueueStalledIsReported(t *testing.T) {
	var (
		mu    sync.Mutex
		posts []string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		posts = append(posts, p.Text)
		mu.Unlock()
	}))
	defer ts.Close()

	ctrl, _ := setupTestController(t, nil)
	ctrl.notifier = notify.NewDispatcher(notify.Config{SlackWebhookURL: ts.URL, Events: []string{notify.EventQueueStalled}})
	_ = ctrl.RegisterWorkOrder(&workorder.WorkOrder{ID: "ROOT", Title: "root", Status: workorder.StatusPending})
	_ = ctrl.RegisterWorkOrder(&workorder.WorkOrder{ID: "KID", Title: "kid", Status: workorder.StatusPending, DependsOn: []string{"ROOT"}})
	_ = ctrl.RegisterWorkOrder(&workorder.WorkOrder{ID: "GRANDKID", Title: "gk", Status: workorder.StatusPending, DependsOn: []string{"KID"}})
	_ = ctrl.store.SetChunkStatus("ROOT", "blocked", "spent", "", "")

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_ = ctrl.Start(ctx)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if st, _ := ctrl.Status(ctx); st.State == StateIdle {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	events, _ := ctrl.Audit(ctx, 50)
	var stalled []string
	for _, ev := range events {
		if ev.Type == "queue.stalled" {
			stalled = append(stalled, ev.Payload)
		}
	}
	if len(stalled) != 1 {
		t.Fatalf("want exactly one queue.stalled event, got %d: %v", len(stalled), stalled)
	}
	if !strings.Contains(stalled[0], "ROOT (blocked)") || !strings.Contains(stalled[0], "2 pending") {
		t.Fatalf("stall payload should name root blocker and count: %q", stalled[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 1 || !strings.Contains(posts[0], "ROOT") {
		t.Fatalf("want one stall notification naming ROOT, got %v", posts)
	}
}

// Rotating the ntfy topic live: new random topic on the same server, persisted so a restart keeps
// it, announced on the new topic, and the retired topic is told it is retired WITHOUT learning the
// new name (rotation exists to cut off whoever knows the old one).
func TestController_RotateNtfyTopic(t *testing.T) {
	var (
		mu    sync.Mutex
		posts = map[string][]string{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts[r.URL.Path] = append(posts[r.URL.Path], r.Header.Get("Title")+"|"+string(b))
		mu.Unlock()
	}))
	defer srv.Close()

	ctrl, repoDir := setupTestController(t, nil)
	ctrl.cfg.Project.Name = "Demo App!"
	ctrl.notifier = notify.NewDispatcher(notify.Config{NtfyURL: srv.URL + "/old-topic"})
	var hooked string
	ctrl.SetNtfyTopicHook(func(u string) { hooked = u })

	newURL, err := ctrl.RotateNtfyTopic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newTopic := strings.TrimPrefix(newURL, srv.URL+"/")
	if !regexp.MustCompile(`^demo-app-[A-Za-z0-9]{24}$`).MatchString(newTopic) {
		t.Fatalf("new topic %q", newTopic)
	}
	if st, _ := ctrl.Status(context.Background()); st.NtfyTopic != newURL {
		t.Fatalf("status topic %q, want %q", st.NtfyTopic, newURL)
	}
	if hooked != newURL {
		t.Fatalf("hook got %q", hooked)
	}
	data, err := os.ReadFile(filepath.Join(repoDir, ".shepherd", NtfyTopicOverrideFile))
	if err != nil || strings.TrimSpace(string(data)) != newURL {
		t.Fatalf("override file: %q %v", data, err)
	}

	mu.Lock()
	old, fresh := posts["/old-topic"], posts["/"+newTopic]
	mu.Unlock()
	if len(old) != 1 || strings.Contains(old[0], newTopic) {
		t.Fatalf("old topic must get one retirement notice without the new name: %v", old)
	}
	if len(fresh) != 1 {
		t.Fatalf("new topic should get a hello: %v", fresh)
	}

	// A restarted controller picks up the rotated topic over config/env.
	store2, _ := db.Open(filepath.Join(repoDir, ".shepherd", "state2.db"))
	defer store2.Close()
	c2, err := New(Options{ProjectRoot: repoDir, Config: &config.Config{SchemaVersion: 1, Project: config.ProjectConfig{Name: "x"},
		Notify: config.NotifyConfig{NtfyURL: srv.URL + "/old-topic"}}, Store: store2, Adapters: map[string]adapter.Adapter{}})
	if err != nil {
		t.Fatal(err)
	}
	if c2.NtfyURL() != newURL {
		t.Fatalf("restart uses %q, want rotated %q", c2.NtfyURL(), newURL)
	}

	// Nothing configured: actionable error.
	ctrl.notifier = notify.NewDispatcher(notify.Config{})
	if _, err := ctrl.RotateNtfyTopic(context.Background()); err == nil {
		t.Fatal("expected error with no ntfy topic")
	}
}

func statusOf(t *testing.T, ctrl *Controller, id string) string {
	t.Helper()
	rec, err := ctrl.store.GetChunk(id)
	if err != nil {
		t.Fatalf("GetChunk(%s): %v", id, err)
	}
	return rec.Status
}

func registerSimple(t *testing.T, ctrl *Controller, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := ctrl.RegisterWorkOrder(&workorder.WorkOrder{
			ID: id, Title: "chunk " + id, Sprint: "01", Status: workorder.StatusPending,
			Files: []workorder.File{{Path: id + ".txt", New: true}},
		}); err != nil {
			t.Fatalf("RegisterWorkOrder(%s): %v", id, err)
		}
	}
}

func TestController_QueueSetActive(t *testing.T) {
	ctrl, _ := setupTestController(t, nil)
	registerSimple(t, ctrl, "A1", "A2", "A3")
	ctx := context.Background()

	if err := ctrl.QueueSetActive(ctx, "A1", false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if got := statusOf(t, ctrl, "A1"); got != "inactive" {
		t.Fatalf("A1 status = %s, want inactive", got)
	}
	if next := ctrl.nextRunnableChunkLocked(); next == nil || next.ID == "A1" {
		t.Fatalf("an inactive chunk must never be scheduled, got %+v", next)
	}
	if err := ctrl.QueueSetActive(ctx, "A1", true); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if got := statusOf(t, ctrl, "A1"); got != "pending" {
		t.Fatalf("A1 status = %s, want pending", got)
	}

	// Only pending <-> inactive; other lifecycle states are left alone and the caller is told why.
	_ = ctrl.store.SetChunkStatus("A2", "running", "", "local-worker-1", "primary")
	_ = ctrl.store.SetChunkStatus("A3", "done", "", "", "")
	for _, id := range []string{"A2", "A3"} {
		if err := ctrl.QueueSetActive(ctx, id, false); err == nil {
			t.Errorf("deactivating %s (%s) should be refused", id, statusOf(t, ctrl, id))
		}
	}
	if err := ctrl.QueueSetActive(ctx, "A1", true); err != nil {
		t.Errorf("activating an already-active chunk should be a no-op, got %v", err)
	}
	if err := ctrl.QueueSetActive(ctx, "nope", false); err == nil {
		t.Error("unknown chunk should error")
	}
	if got := statusOf(t, ctrl, "A2"); got != "running" {
		t.Errorf("running chunk was disturbed: %s", got)
	}
}

func TestController_QueueSetAllActive(t *testing.T) {
	ctrl, _ := setupTestController(t, nil)
	registerSimple(t, ctrl, "B1", "B2", "B3", "B4", "B5")
	ctx := context.Background()
	_ = ctrl.store.SetChunkStatus("B3", "running", "", "local-worker-1", "primary")
	_ = ctrl.store.SetChunkStatus("B4", "done", "", "", "")
	_ = ctrl.store.SetChunkStatus("B5", "held", "needs review", "", "")

	n, err := ctrl.QueueSetAllActive(ctx, false)
	if err != nil || n != 2 {
		t.Fatalf("uncheck all = %d, %v; want 2", n, err)
	}
	want := map[string]string{"B1": "inactive", "B2": "inactive", "B3": "running", "B4": "done", "B5": "held"}
	for id, st := range want {
		if got := statusOf(t, ctrl, id); got != st {
			t.Errorf("after uncheck all: %s = %s, want %s", id, got, st)
		}
	}
	if next := ctrl.nextRunnableChunkLocked(); next != nil {
		t.Errorf("nothing should be runnable after uncheck all, got %s", next.ID)
	}

	n, err = ctrl.QueueSetAllActive(ctx, true)
	if err != nil || n != 2 {
		t.Fatalf("check all = %d, %v; want 2", n, err)
	}
	if got := statusOf(t, ctrl, "B5"); got != "held" {
		t.Errorf("check all must not release review-held chunks, got %s", got)
	}
	if statusOf(t, ctrl, "B1") != "pending" || statusOf(t, ctrl, "B2") != "pending" {
		t.Error("check all should requeue inactive chunks")
	}
}

func TestController_InactiveChunkDoesNotRunEvenIfAlreadySelected(t *testing.T) {
	ran := false
	mockAd := &mockTestAdapter{onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
		ran = true
		return adapter.RunResult{ExitCode: 0, Output: "ok"}, nil
	}}
	ctrl, _ := setupTestController(t, mockAd)
	wo := &workorder.WorkOrder{ID: "C1", Title: "c", Sprint: "01", Status: workorder.StatusPending, Files: []workorder.File{{Path: "c.txt", New: true}}}
	_ = ctrl.RegisterWorkOrder(wo)

	// The scheduler picked C1, then the operator unchecked it before it started.
	if err := ctrl.QueueSetActive(context.Background(), "C1", false); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.executeChunk(wo); !errors.Is(err, context.Canceled) {
		t.Fatalf("executeChunk on an inactive chunk = %v, want context.Canceled (not a block)", err)
	}
	if ran {
		t.Fatal("adapter ran for a chunk the operator deactivated")
	}
	if got := statusOf(t, ctrl, "C1"); got != "inactive" {
		t.Fatalf("status = %s, want inactive", got)
	}
}

func TestController_QueueActionsRefuseRunningChunks(t *testing.T) {
	ctrl, _ := setupTestController(t, nil)
	registerSimple(t, ctrl, "D1")
	_ = ctrl.store.SetChunkStatus("D1", "running", "", "local-worker-1", "primary")
	ctx := context.Background()

	for name, fn := range map[string]func() error{
		"skip":    func() error { return ctrl.QueueSkip(ctx, "D1") },
		"retry":   func() error { return ctrl.QueueRetry(ctx, "D1") },
		"hold":    func() error { return ctrl.QueueHold(ctx, "D1") },
		"release": func() error { return ctrl.QueueRelease(ctx, "D1") },
		"assign":  func() error { return ctrl.QueueAssign(ctx, "D1", "primary", "") },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s on a running chunk must be refused (it would double-run)", name)
		}
	}
	if got := statusOf(t, ctrl, "D1"); got != "running" {
		t.Errorf("running chunk status changed to %s", got)
	}
}

func TestController_QueueRetryClearsBlockedAndKeepsAssignment(t *testing.T) {
	ctrl, _ := setupTestController(t, nil)
	registerSimple(t, ctrl, "E1")
	ctx := context.Background()
	if err := ctrl.QueueAssign(ctx, "E1", "primary", "sonnet"); err != nil {
		t.Fatal(err)
	}
	_ = ctrl.store.SetChunkStatus("E1", "blocked", "verification failed", "local-worker-1", "primary")

	if err := ctrl.QueueRetry(ctx, "E1"); err != nil {
		t.Fatal(err)
	}
	rec, _ := ctrl.store.GetChunk("E1")
	if rec.Status != "pending" || rec.BlockedReason != "" {
		t.Errorf("retry should requeue and clear the reason: %+v", rec)
	}
	if rec.PinTier != "primary" || rec.PinModel != "sonnet" {
		t.Errorf("retry must keep the operator's model assignment: %+v", rec)
	}
}

func TestController_QueueAssignValidation(t *testing.T) {
	ctrl, _ := setupTestController(t, nil)
	ctrl.cfg.Tiers["strong"] = config.TierConfig{Adapter: "mock-test"}
	ctrl.cfg.Tiers["off"] = config.TierConfig{Adapter: "mock-test", Enabled: config.BoolPtr(false)}
	registerSimple(t, ctrl, "F1")
	ctx := context.Background()

	if err := ctrl.QueueAssign(ctx, "F1", "strong", "claude-opus-5-5"); err != nil {
		t.Fatalf("valid assignment: %v", err)
	}
	rec, _ := ctrl.store.GetChunk("F1")
	if rec.PinTier != "strong" || rec.PinModel != "claude-opus-5-5" {
		t.Fatalf("pin not stored: %+v", rec)
	}
	if rec.Status != "pending" {
		t.Errorf("assigning must not change the lifecycle status, got %s", rec.Status)
	}

	bad := []struct{ tier, model, why string }{
		{"missing", "", "unknown tier"},
		{"off", "", "deactivated tier"},
		{"strong", "--dangerously-skip-permissions", "model that would be parsed as a CLI flag"},
		{"strong", "-m", "leading dash"},
		{"strong", "opus; rm -rf /", "shell metacharacters"},
		{"strong", "a b", "whitespace"},
		{"strong", "m\nodel", "newline"},
		{"strong", strings.Repeat("a", 200), "overlong"},
		{"../etc", "", "path-like tier"},
	}
	for _, b := range bad {
		if err := ctrl.QueueAssign(ctx, "F1", b.tier, b.model); err == nil {
			t.Errorf("assign(%q, %q) accepted: %s", b.tier, b.model, b.why)
		}
	}
	if err := ctrl.QueueAssign(ctx, "nope", "strong", ""); err == nil {
		t.Error("assigning an unknown chunk should error")
	}
	rec, _ = ctrl.store.GetChunk("F1")
	if rec.PinTier != "strong" || rec.PinModel != "claude-opus-5-5" {
		t.Errorf("rejected assignments changed the stored pin: %+v", rec)
	}

	if err := ctrl.QueueAssign(ctx, "F1", "", ""); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	rec, _ = ctrl.store.GetChunk("F1")
	if rec.PinTier != "" || rec.PinModel != "" {
		t.Errorf("pin not cleared: %+v", rec)
	}
	events, _ := ctrl.store.EventLog.Since(0)
	found := false
	for _, ev := range events {
		if ev.Type == "queue.assigned" && strings.Contains(ev.Payload, "F1") && strings.Contains(ev.Payload, "claude-opus-5-5") {
			found = true
		}
	}
	if !found {
		t.Error("assignment should be written to the audit log")
	}
}

func TestController_AssignedModelOverridesComplexityRouting(t *testing.T) {
	var gotModel string
	mockAd := &mockTestAdapter{onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
		gotModel = opts.Model
		return adapter.RunResult{ExitCode: 0, Output: "done"}, nil
	}}
	ctrl, repoDir := setupTestController(t, mockAd)
	ctrl.adapters["agy"] = mockAd
	ctrl.cfg.Tiers["strong"] = config.TierConfig{Adapter: "agy", Pool: "claude", Model: "strong-default"}
	ctrl.cfg.Tiers["cheap"] = config.TierConfig{Adapter: "mock-test", Model: "cheap-default"}

	// High review + large effort would normally be routed to the strong tier.
	wo := &workorder.WorkOrder{
		ID: "G1", Title: "complex", Sprint: "01", Status: workorder.StatusPending,
		Review: workorder.ReviewHigh, Effort: workorder.EffortL,
		Files: []workorder.File{{Path: "g.go", New: true}},
	}
	_ = ctrl.RegisterWorkOrder(wo)
	_ = os.WriteFile(filepath.Join(repoDir, "g.go"), []byte("package main\n"), 0644)

	if err := ctrl.QueueAssign(context.Background(), "G1", "cheap", "my-chosen-model"); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.executeChunk(wo); err != nil {
		t.Fatalf("executeChunk: %v", err)
	}
	if gotModel != "my-chosen-model" {
		t.Errorf("adapter got model %q, want the assigned %q", gotModel, "my-chosen-model")
	}
	rec, _ := ctrl.store.GetChunk("G1")
	if rec.Tier != "cheap" {
		t.Errorf("executed on tier %q, want the assigned tier cheap", rec.Tier)
	}
}

func TestController_AssignedTierWithoutModelUsesTierDefault(t *testing.T) {
	var gotModel string
	mockAd := &mockTestAdapter{onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
		gotModel = opts.Model
		return adapter.RunResult{ExitCode: 0, Output: "done"}, nil
	}}
	ctrl, repoDir := setupTestController(t, mockAd)
	ctrl.cfg.Tiers["cheap"] = config.TierConfig{Adapter: "mock-test", Model: "cheap-default"}
	wo := &workorder.WorkOrder{ID: "G2", Title: "t", Sprint: "01", Status: workorder.StatusPending, Files: []workorder.File{{Path: "g2.go", New: true}}}
	_ = ctrl.RegisterWorkOrder(wo)
	_ = os.WriteFile(filepath.Join(repoDir, "g2.go"), []byte("package main\n"), 0644)
	_ = ctrl.QueueAssign(context.Background(), "G2", "cheap", "")
	if err := ctrl.executeChunk(wo); err != nil {
		t.Fatal(err)
	}
	if gotModel != "cheap-default" {
		t.Errorf("model = %q, want cheap-default", gotModel)
	}
}

func TestController_AssignmentNeverBypassesNoCloudOrDisabledTier(t *testing.T) {
	var gotModel string
	mockAd := &mockTestAdapter{onRun: func(ctx context.Context, prompt string, opts adapter.RunOptions) (adapter.RunResult, error) {
		gotModel = opts.Model
		return adapter.RunResult{ExitCode: 0, Output: "done"}, nil
	}}

	t.Run("nocloud", func(t *testing.T) {
		gotModel = ""
		ctrl, repoDir := setupTestController(t, mockAd)
		ctrl.noCloud = true
		ctrl.adapters["opencode"] = mockAd
		ctrl.cfg.Tiers["local"] = config.TierConfig{Adapter: "opencode", Pool: "local", Model: "local-model"}
		ctrl.cfg.Tiers["cloudy"] = config.TierConfig{Adapter: "mock-test", Pool: "claude", Model: "cloud-model"}
		wo := &workorder.WorkOrder{ID: "H1", Title: "t", Sprint: "01", Status: workorder.StatusPending, Files: []workorder.File{{Path: "h1.go", New: true}}}
		_ = ctrl.RegisterWorkOrder(wo)
		_ = os.WriteFile(filepath.Join(repoDir, "h1.go"), []byte("package main\n"), 0644)
		_ = ctrl.QueueAssign(context.Background(), "H1", "cloudy", "cloud-model")
		if err := ctrl.executeChunk(wo); err != nil {
			t.Fatal(err)
		}
		if gotModel == "cloud-model" {
			t.Fatal("--nocloud must win over a cloud assignment")
		}
		rec, _ := ctrl.store.GetChunk("H1")
		if rec.Tier != "local" {
			t.Errorf("tier = %s, want local", rec.Tier)
		}
	})

	t.Run("tier deactivated after assignment", func(t *testing.T) {
		gotModel = ""
		ctrl, repoDir := setupTestController(t, mockAd)
		ctrl.cfg.Tiers["cheap"] = config.TierConfig{Adapter: "mock-test", Model: "cheap-default"}
		wo := &workorder.WorkOrder{ID: "H2", Title: "t", Sprint: "01", Status: workorder.StatusPending, Files: []workorder.File{{Path: "h2.go", New: true}}}
		_ = ctrl.RegisterWorkOrder(wo)
		_ = os.WriteFile(filepath.Join(repoDir, "h2.go"), []byte("package main\n"), 0644)
		_ = ctrl.QueueAssign(context.Background(), "H2", "cheap", "pinned-model")
		tc := ctrl.cfg.Tiers["cheap"]
		tc.Enabled = config.BoolPtr(false)
		ctrl.cfg.Tiers["cheap"] = tc
		if err := ctrl.executeChunk(wo); err != nil {
			t.Fatal(err)
		}
		if gotModel == "pinned-model" {
			t.Fatal("an assignment must not resurrect a tier the operator deactivated")
		}
		events, _ := ctrl.store.EventLog.Since(0)
		ignored := false
		for _, ev := range events {
			if ev.Type == "chunk.assignment_ignored" && strings.Contains(ev.Payload, "H2") {
				ignored = true
			}
		}
		if !ignored {
			t.Error("ignoring an assignment should be audited")
		}
	})
}

func TestController_QueueRefreshReadsPlanFromDisk(t *testing.T) {
	ctrl, repoDir := setupTestController(t, nil)
	planDir := filepath.Join(repoDir, "docs", "plan")
	if err := os.MkdirAll(planDir, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(name, id, title string) {
		src := "---\nid: " + id + "\ntitle: " + title + "\nsprint: S1\nreview: LOW\nest_effort: S\nstatus: pending\nfiles:\n    - " + id + ".go\ntests:\n    - " + id + "_test.go\n---\n## Goal\nDo it.\n"
		if err := os.WriteFile(filepath.Join(planDir, name), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("one.md", "R1", "First")
	write("two.md", "R2", "Second")
	if err := os.WriteFile(filepath.Join(planDir, "broken.md"), []byte("not a work order"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := ctrl.QueueRefresh(context.Background())
	if err != nil {
		t.Fatalf("QueueRefresh: %v", err)
	}
	if res.Added != 2 || res.Updated != 0 || res.Invalid != 1 {
		t.Fatalf("first refresh = %+v, want added=2 updated=0 invalid=1", res)
	}

	// Operator state survives a refresh; edited and new files are picked up.
	_ = ctrl.QueueSetActive(context.Background(), "R1", false)
	write("one.md", "R1", "First (edited)")
	write("three.md", "R3", "Third")
	res, err = ctrl.QueueRefresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.Updated != 1 {
		t.Fatalf("second refresh = %+v, want added=1 updated=1", res)
	}
	r1, _ := ctrl.store.GetChunk("R1")
	if r1.Title != "First (edited)" {
		t.Errorf("edited title not reloaded: %q", r1.Title)
	}
	if r1.Status != "inactive" {
		t.Errorf("refresh must not reactivate a deactivated chunk, got %s", r1.Status)
	}
}

func TestController_QueueRefreshWithoutPlanTreeIsAnError(t *testing.T) {
	ctrl, _ := setupTestController(t, nil)
	if _, err := ctrl.QueueRefresh(context.Background()); err == nil {
		t.Fatal("a missing plan tree should be reported, not silently ignored")
	}
}

func TestController_IsSubChunk_GroupBoundary(t *testing.T) {
	ctrl, _ := setupTestController(t, nil)

	for _, id := range []string{"45A.1", "45A.2", "45A.3", "24L"} {
		_ = ctrl.RegisterWorkOrder(&workorder.WorkOrder{
			ID:     id,
			Title:  "Chunk " + id,
			Status: workorder.StatusPending,
		})
	}

	// Unsplit chunk is never a sub-chunk
	if ctrl.isSubChunk("24L") {
		t.Errorf("expected unsplit chunk 24L to have isSubChunk=false")
	}

	// While 45A.2 and 45A.3 are pending, 45A.1 is an intermediate sub-chunk
	if !ctrl.isSubChunk("45A.1") {
		t.Errorf("expected 45A.1 to have isSubChunk=true while 45A.2 and 45A.3 are pending")
	}

	// Mark 45A.1 done -> 45A.2 is still an intermediate sub-chunk because 45A.3 is pending
	_ = ctrl.store.SetChunkStatus("45A.1", "done", "", "local-worker-1", "flash")
	if !ctrl.isSubChunk("45A.2") {
		t.Errorf("expected 45A.2 to have isSubChunk=true while 45A.3 is pending")
	}

	// Mark 45A.2 done -> 45A.3 is now the final remaining chunk of 45A, so isSubChunk=false (runs full suite)
	_ = ctrl.store.SetChunkStatus("45A.2", "done", "", "local-worker-1", "flash")
	if ctrl.isSubChunk("45A.3") {
		t.Errorf("expected final group chunk 45A.3 to have isSubChunk=false once 45A.1 and 45A.2 are done")
	}
}
