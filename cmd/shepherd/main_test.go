package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/eventlog"
)

func put(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const legacyChunk = "# %s -- t\n\n**Effort:** S | **Depends on:** %s | **Review:** low\n\n## Goal\ng\n\n## Files\n- EDIT `src/a.ts`\n\n## Done when\n- ok\n"

func legacyOrder(id, deps string) string {
	return strings.Replace(strings.Replace(legacyChunk, "%s", id, 1), "%s", deps, 1)
}

func TestPlanMigrateThenLint(t *testing.T) {
	root := t.TempDir()
	put(t, root, "src/a.ts", "x")
	put(t, root, "old/done/01A_first.md", legacyOrder("01A", "nothing"))
	put(t, root, "old/todo/area/02A_second.md", legacyOrder("02A", "01A"))
	put(t, root, "old/todo/README.md", "not a chunk")

	var out, errb bytes.Buffer
	code := run([]string{"plan", "migrate", "--from", "bold-lines", "--out", filepath.Join(root, "docs/plan"),
		"--done", filepath.Join(root, "old/done"), filepath.Join(root, "old/todo")}, &out, &errb)
	if code != 0 {
		t.Fatalf("migrate exit %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "2 converted") {
		t.Errorf("migrate output: %s", out.String())
	}
	b, err := os.ReadFile(filepath.Join(root, "docs/plan/S01/01A_first.md"))
	if err != nil || !strings.Contains(string(b), "status: done") {
		t.Fatalf("01A: %v\n%s", err, b)
	}

	out.Reset()
	errb.Reset()
	// Both chunks declare src/a.ts; 02A depends on 01A, so the overlap is ordered.
	if code := run([]string{"plan", "lint", "--root", root}, &out, &errb); code != 0 {
		t.Fatalf("lint exit %d\n%s%s", code, out.String(), errb.String())
	}

	// A second run must not overwrite converted (possibly hand-edited) files.
	out.Reset()
	errb.Reset()
	code = run([]string{"plan", "migrate", "--from", "bold-lines", "--out", filepath.Join(root, "docs/plan"),
		filepath.Join(root, "old/todo")}, &out, &errb)
	if code == 0 || !strings.Contains(errb.String(), "exists") {
		t.Errorf("rerun exit %d: %s", code, errb.String())
	}
}

func TestPlanMigrateRejectsUnknownFormat(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"plan", "migrate", "--from", "nope", "--out", t.TempDir(), t.TempDir()}, &out, &errb); code != 2 {
		t.Errorf("exit %d", code)
	}
}

func TestPlanLintAbsolutePlanRoot(t *testing.T) {
	root, plans := t.TempDir(), t.TempDir()
	put(t, root, "src/a.ts", "x")
	put(t, plans, "S1/1A_a.md", "---\nid: 1A\ntitle: a\nsprint: S1\nfiles: [src/a.ts]\nno_tests_reason: x\n---\n## Goal\ng\n## Out of scope\nx\n## Acceptance\ny\n")
	var out, errb bytes.Buffer
	if code := run([]string{"plan", "lint", "--root", root, "--plan", plans}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "1 chunks, 0 errors") {
		t.Errorf("output: %s", out.String())
	}
}

func TestPlanMigrateExpandsAbbreviatedPaths(t *testing.T) {
	root := t.TempDir()
	put(t, root, "src/app/[id]/page.tsx", "x")
	put(t, root, "node_modules/pkg/app/[id]/page.tsx", "x") // must not make the suffix ambiguous
	put(t, root, "old/01A_first.md", strings.Replace(legacyOrder("01A", "nothing"), "src/a.ts", ".../app/[id]/page.tsx", 1))
	var out, errb bytes.Buffer
	if code := run([]string{"plan", "migrate", "--from", "bold-lines", "--root", root, "--out", filepath.Join(root, "plan"),
		filepath.Join(root, "old")}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	b, _ := os.ReadFile(filepath.Join(root, "plan/S01/01A_first.md"))
	if !strings.Contains(string(b), "src/app/[id]/page.tsx") {
		t.Errorf("not expanded:\n%s", b)
	}
}

func TestPlanFormatCommand(t *testing.T) {
	root := t.TempDir()
	planDir := filepath.Join(root, "docs", "plan", "S01")
	_ = os.MkdirAll(planDir, 0755)

	// File with review: MED that needs normalization
	fileContent := "---\nid: 01A\ntitle: Format Test\nsprint: S01\nreview: MED\nest_effort: MED\nfiles:\n  - src/foo.go\nno_tests_reason: ok\n---\n## Goal\nTest\n"
	_ = os.WriteFile(filepath.Join(planDir, "01A_test.md"), []byte(fileContent), 0644)

	var out, errb bytes.Buffer
	code := run([]string{"format", "--root", root}, &out, &errb)
	if code != 0 {
		t.Fatalf("format exit %d: %s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "formatted 1 work order") {
		t.Errorf("unexpected output: %s", out.String())
	}

	updated, _ := os.ReadFile(filepath.Join(planDir, "01A_test.md"))
	if !strings.Contains(string(updated), "review: MEDIUM") {
		t.Errorf("expected review: MEDIUM, got:\n%s", string(updated))
	}
	if !strings.Contains(string(updated), "est_effort: M") {
		t.Errorf("expected est_effort: M, got:\n%s", string(updated))
	}
}

func TestCmdInitAndDoctor(t *testing.T) {
	root := t.TempDir()
	put(t, root, "go.mod", "module example.com/app\n\ngo 1.22\n")

	var out, errb bytes.Buffer
	// Test init
	code := run([]string{"init", "--root", root, "--name", "app"}, &out, &errb)
	if code != 0 {
		t.Fatalf("init exit %d: %s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "shepherd.toml created") {
		t.Errorf("init output: %s", out.String())
	}

	// Test doctor
	out.Reset()
	errb.Reset()
	docCode := run([]string{"doctor", "--root", root}, &out, &errb)
	// doctor should report shepherd.toml PASS
	if !strings.Contains(out.String(), "[PASS] shepherd.toml") {
		t.Errorf("doctor output did not report valid config: %s", out.String())
	}
	// exit code depends on whether agent CLIs / git are present in t.TempDir(), but command executes
	_ = docCode
}

type testBackend struct {
	status control.DaemonStatus
	cfg    map[string]string
	events []eventlog.Event
	chunks []db.ChunkRecord
}

func (m *testBackend) Status(_ context.Context) (control.DaemonStatus, error) { return m.status, nil }
func (m *testBackend) Start(_ context.Context) error                          { m.status.State = "running"; return nil }
func (m *testBackend) Pause(_ context.Context) error                          { m.status.State = "paused"; return nil }
func (m *testBackend) Resume(_ context.Context) error                         { m.status.State = "running"; return nil }
func (m *testBackend) Stop(_ context.Context) error                           { m.status.State = "stopping"; return nil }
func (m *testBackend) Halt(_ context.Context, reason string) error           { m.status.State = "idle"; return nil }
func (m *testBackend) Drain(_ context.Context, enabled bool, until *time.Time) error {
	m.status.DrainMode = enabled
	return nil
}
func (m *testBackend) Lockdown(_ context.Context) error {
	m.status.Lockdown = true
	m.status.State = "lockdown"
	return nil
}
func (m *testBackend) Unlock(_ context.Context) error {
	m.status.Lockdown = false
	m.status.State = "idle"
	return nil
}
func (m *testBackend) ConfigGet(_ context.Context) (map[string]string, error) { return m.cfg, nil }
func (m *testBackend) ConfigSet(_ context.Context, key, val string) error {
	if m.cfg == nil {
		m.cfg = make(map[string]string)
	}
	m.cfg[key] = val
	return nil
}
func (m *testBackend) ConfigReload(_ context.Context) error { return nil }
func (m *testBackend) QueueList(_ context.Context) ([]db.ChunkRecord, error) {
	return m.chunks, nil
}
func (m *testBackend) QueueSkip(_ context.Context, id string) error    { return nil }
func (m *testBackend) QueueRetry(_ context.Context, id string) error   { return nil }
func (m *testBackend) QueueHold(_ context.Context, id string) error    { return nil }
func (m *testBackend) QueueRelease(_ context.Context, id string) error { return nil }
func (m *testBackend) QueuePin(_ context.Context, id, tier, pool string) error {
	return nil
}
func (m *testBackend) QueueDiff(_ context.Context, id string) (string, error) {
	return "+// sample diff for " + id, nil
}
func (m *testBackend) QueueApprove(_ context.Context, id string) error {
	return nil
}
func (m *testBackend) QueueReject(_ context.Context, id, reason string) error {
	return nil
}
func (m *testBackend) QuotaGet(_ context.Context) (map[string]any, error) {
	return map[string]any{"pools": map[string]any{}}, nil
}
func (m *testBackend) QuotaSetFloor(_ context.Context, pool string, pct int) error   { return nil }
func (m *testBackend) QuotaSetReserve(_ context.Context, pool string, pct int) error { return nil }
func (m *testBackend) QuotaTierToggle(_ context.Context, tier string, enabled bool) error {
	return nil
}
func (m *testBackend) QuotaResetBucket(_ context.Context, pool string) error { return nil }
func (m *testBackend) Audit(_ context.Context, limit int) ([]eventlog.Event, error) {
	return m.events, nil
}
func (m *testBackend) ValidateToken(_ context.Context, token string) (string, error) {
	return control.RoleAdmin, nil
}

func TestCliCommands_EndToEnd(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "control.sock")
	t.Setenv("SHEPHERD_SOCKET", sockPath)

	tb := &testBackend{
		status: control.DaemonStatus{
			State:          "idle",
			QueuePending:   2,
			QueueCompleted: 1,
			NtfyTopic:      "https://ntfy.sh/proj-Ab3dEf6hIj9kLm2nOp5qRs8t",
		},
		cfg: map[string]string{
			"policy.max_consecutive_blocks": "3",
		},
		events: []eventlog.Event{
			{Seq: 1, Timestamp: time.Now(), Type: "daemon.started", Actor: "system", Payload: "init"},
		},
		chunks: []db.ChunkRecord{
			{ID: "01A", Title: "Sample Chunk", Status: "pending", Sprint: "01", WorkerID: "worker-1"},
		},
	}

	srv := control.NewServer(tb, sockPath)
	if err := srv.Start(); err != nil {
		t.Fatalf("start control server: %v", err)
	}
	defer srv.Close()

	exec := func(args ...string) (string, string, int) {
		var out, errb bytes.Buffer
		code := run(args, &out, &errb)
		return out.String(), errb.String(), code
	}

	// 1. status
	out, errb, code := exec("status")
	if code != 0 || !strings.Contains(out, "Daemon State:   IDLE") {
		t.Fatalf("status failed (exit %d): %s%s", code, out, errb)
	}
	if !strings.Contains(out, "ntfy Topic:     https://ntfy.sh/proj-Ab3dEf6hIj9kLm2nOp5qRs8t\n") {
		t.Fatalf("status should print the full ntfy topic URL:\n%s", out)
	}

	// 2. pause / resume
	_, _, code = exec("pause")
	if code != 0 || tb.status.State != "paused" {
		t.Fatalf("pause failed: exit %d, state: %s", code, tb.status.State)
	}

	_, _, code = exec("resume")
	if code != 0 || tb.status.State != "running" {
		t.Fatalf("resume failed: exit %d, state: %s", code, tb.status.State)
	}

	// 3. drain
	_, _, code = exec("drain", "on")
	if code != 0 || !tb.status.DrainMode {
		t.Fatalf("drain on failed: exit %d, drain: %v", code, tb.status.DrainMode)
	}

	// 4. lockdown / unlock
	_, _, code = exec("lockdown")
	if code != 0 || !tb.status.Lockdown {
		t.Fatalf("lockdown failed: exit %d", code)
	}

	_, _, code = exec("unlock")
	if code != 0 || tb.status.Lockdown {
		t.Fatalf("unlock failed: exit %d", code)
	}

	// 5. config
	_, _, code = exec("config", "set", "policy.max_consecutive_blocks", "5")
	if code != 0 || tb.cfg["policy.max_consecutive_blocks"] != "5" {
		t.Fatalf("config set failed: exit %d", code)
	}

	out, _, code = exec("config", "show")
	if code != 0 || !strings.Contains(out, "policy.max_consecutive_blocks") {
		t.Fatalf("config show failed: %s", out)
	}

	// 6. queue ls
	out, _, code = exec("queue", "ls")
	if code != 0 || !strings.Contains(out, "01A") {
		t.Fatalf("queue ls failed: %s", out)
	}

	// 7. audit
	out, _, code = exec("audit")
	if code != 0 || !strings.Contains(out, "daemon.started") {
		t.Fatalf("audit failed: %s", out)
	}

	// 8. dashboard --once
	out, _, code = exec("dashboard", "--once")
	if code != 0 || !strings.Contains(out, "SHEPHERD CONTROL STATION") {
		t.Fatalf("dashboard --once failed (exit %d): %s", code, out)
	}
}

func TestWorkerCommands(t *testing.T) {
	dir := t.TempDir()
	caDir := filepath.Join(dir, "ca")
	dbPath := filepath.Join(dir, "shepherd.db")

	var out, errb bytes.Buffer

	// 1. worker ls on empty db
	code := run([]string{"worker", "ls", "--db", dbPath}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "No workers registered") {
		t.Fatalf("worker ls empty failed (exit %d): %s %s", code, out.String(), errb.String())
	}

	// 2. worker add
	out.Reset()
	errb.Reset()
	code = run([]string{"worker", "add", "--addr", "127.0.0.1:8443", "--ca-dir", caDir, "--db", dbPath}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "PAIRING STRING:") {
		t.Fatalf("worker add failed (exit %d): %s %s", code, out.String(), errb.String())
	}

	// 3. Register a test worker directly in DB
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	err = store.UpsertWorker(db.WorkerRecord{
		ID:         "box-1",
		PubkeyHash: "11223344556677889900aabbcc",
		Role:       "worker",
		Status:     "active",
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(7 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("UpsertWorker: %v", err)
	}
	_ = store.Close()

	// 4. worker ls now lists box-1
	out.Reset()
	errb.Reset()
	code = run([]string{"worker", "ls", "--db", dbPath}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "box-1") || !strings.Contains(out.String(), "active") {
		t.Fatalf("worker ls with worker failed: %s", out.String())
	}

	// 5. worker drain
	out.Reset()
	errb.Reset()
	code = run([]string{"worker", "drain", "box-1", "--db", dbPath}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "drained") {
		t.Fatalf("worker drain failed: %s %s", out.String(), errb.String())
	}

	// 6. worker revoke
	out.Reset()
	errb.Reset()
	code = run([]string{"worker", "revoke", "box-1", "--db", dbPath}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "revoked (STRIDE S4)") {
		t.Fatalf("worker revoke failed: %s %s", out.String(), errb.String())
	}

	// 7. Verify status in DB
	store, _ = db.Open(dbPath)
	wk, _ := store.GetWorker("box-1")
	if wk.Status != "revoked" {
		t.Fatalf("Expected worker status 'revoked', got %q", wk.Status)
	}
	_ = store.Close()
}

func TestPlanSplitCommand(t *testing.T) {
	dir := t.TempDir()
	chunkPath := filepath.Join(dir, "01A.md")
	content := "---\nid: 01A\ntitle: Big Chunk\nfiles:\n  - file1.go\n  - file2.go\n  - file3.go\n  - file4.go\nno_tests_reason: ok\n---\n## Goal\nComplete big chunk\n"
	if err := os.WriteFile(chunkPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var out, errb bytes.Buffer
	code := run([]string{"plan", "split", "--parts", "2", chunkPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("plan split failed (exit %d): %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "01A.1") || !strings.Contains(out.String(), "01A.2") {
		t.Errorf("expected split subchunk IDs in output, got: %s", out.String())
	}
}

func TestRetroCommand(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = store.UpsertChunk(db.ChunkRecord{
		ID:     "01A",
		Title:  "First",
		Status: "done",
		Tier:   "high",
	})
	_ = store.Close()

	var out, errb bytes.Buffer
	code := run([]string{"retro", "--db", dbPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("retro failed (exit %d): %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Shepherd Sprint Retrospective") || !strings.Contains(out.String(), "01A") {
		t.Errorf("unexpected retro output: %s", out.String())
	}
}

func TestServiceCommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"service"}, &out, &errb)
	if code != 2 {
		t.Errorf("expected exit 2 for missing args, got %d", code)
	}

	out.Reset()
	errb.Reset()
	code = run([]string{"service", "status"}, &out, &errb)
	// Status should run (exit 0 on linux/darwin)
	if code != 0 {
		t.Logf("service status exit %d: %s %s", code, out.String(), errb.String())
	}
}

func TestPlanTestFirstCommand(t *testing.T) {
	root := t.TempDir()
	chunkPath := filepath.Join(root, "04A_auth.md")
	content := `---
id: 04A
title: Auth Token Provider
files:
  - internal/auth/token.go
  - tests/auth_test.go
tests:
  - tests/auth_test.go
---
## Goal
Implement auth token validation.
`
	if err := os.WriteFile(chunkPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	// Test missing args -> exit 2
	code := run([]string{"plan", "test-first"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit 2 on empty args, got %d", code)
	}

	out.Reset()
	errb.Reset()
	// Test file execution with --write
	code = run([]string{"plan", "test-first", "--write", chunkPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("plan test-first failed (exit %d): %s %s", code, out.String(), errb.String())
	}

	specFile := filepath.Join(root, "04A.spec.md")
	implFile := filepath.Join(root, "04A.impl.md")

	if _, err := os.Stat(specFile); err != nil {
		t.Fatalf("spec file not created: %v", err)
	}
	implBytes, err := os.ReadFile(implFile)
	if err != nil {
		t.Fatalf("impl file not created: %v", err)
	}
	implStr := string(implBytes)
	if !strings.Contains(implStr, "read_only_tests: true") {
		t.Errorf("impl file does not contain read_only_tests: true:\n%s", implStr)
	}
	if !strings.Contains(implStr, "04A.spec") {
		t.Errorf("impl file does not depend on 04A.spec:\n%s", implStr)
	}
}

func TestWebCommand(t *testing.T) {
	var out, errb bytes.Buffer
	// Bad flag returns 2
	code := run([]string{"web", "--invalid-flag"}, &out, &errb)
	if code != 2 {
		t.Errorf("expected exit 2 for invalid flag, got %d", code)
	}
}

func TestQueueReviewAndPRCommands(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "control.sock")
	t.Setenv("SHEPHERD_SOCKET", sockPath)

	tb := &testBackend{
		status: control.DaemonStatus{State: "idle"},
	}
	srv := control.NewServer(tb, sockPath)
	if err := srv.Start(); err != nil {
		t.Fatalf("start control server: %v", err)
	}
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"queue", "diff", "01A"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "sample diff for 01A") {
		t.Fatalf("queue diff failed (exit %d): %s %s", code, out.String(), errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := run([]string{"queue", "approve", "01A"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "approved and committed") {
		t.Fatalf("queue approve failed (exit %d): %s %s", code, out.String(), errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := run([]string{"queue", "reject", "--reason", "bad logic", "01A"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "rejected") {
		t.Fatalf("queue reject failed (exit %d): %s %s", code, out.String(), errb.String())
	}

	// Test shepherd pr command
	dbPath := filepath.Join(sockDir, "state.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	_ = store.UpsertChunk(db.ChunkRecord{
		ID:     "01A",
		Title:  "Auth Hardening",
		Sprint: "S1",
		Status: "done",
		Tier:   "strong",
	})
	_ = store.RecordChunkCost("01A", "S1", "strong", 12500, 0.425)
	_ = store.Close()

	out.Reset()
	errb.Reset()
	if code := run([]string{"pr", "--db", dbPath, "--sprint", "S1"}, &out, &errb); code != 0 {
		t.Fatalf("shepherd pr failed (exit %d): %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Auth Hardening") || !strings.Contains(out.String(), "$0.4250") {
		t.Errorf("expected PR summary with chunk title and cost telemetry, got:\n%s", out.String())
	}
}

func (m *testBackend) RotateNtfyTopic(context.Context) (string, error) { return "", nil }

func (m *testBackend) QueueSetActive(context.Context, string, bool) error  { return nil }
func (m *testBackend) QueueSetAllActive(context.Context, bool) (int, error) { return 0, nil }
func (m *testBackend) QueueAssign(context.Context, string, string, string) error {
	return nil
}
func (m *testBackend) FrontRunStart(context.Context, string, string) error { return nil }
func (m *testBackend) FrontRunStop(context.Context) error                   { return nil }
func (m *testBackend) QueueCheck(context.Context, string) (control.QueueCheckResult, error) {
	return control.QueueCheckResult{}, nil
}
func (m *testBackend) QueueRefresh(context.Context) (control.QueueRefreshResult, error) {
	return control.QueueRefreshResult{}, nil
}
