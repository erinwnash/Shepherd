package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/eventlog"
)

type mockBackend struct {
	status DaemonStatus
	tokens map[string]string // token -> role
	cfg    map[string]string
	events []eventlog.Event
	halted bool
	reason string
	drain  bool
	locked bool

	calls []string // queue-manager calls, in order
}

func (m *mockBackend) Status(_ context.Context) (DaemonStatus, error) {
	st := m.status
	st.DrainMode = m.drain
	st.Lockdown = m.locked
	return st, nil
}
func (m *mockBackend) Start(_ context.Context) error  { m.status.State = "running"; return nil }
func (m *mockBackend) Pause(_ context.Context) error  { m.status.State = "paused"; return nil }
func (m *mockBackend) Resume(_ context.Context) error { m.status.State = "running"; return nil }
func (m *mockBackend) Stop(_ context.Context) error   { m.status.State = "stopping"; return nil }
func (m *mockBackend) Halt(_ context.Context, reason string) error {
	m.halted = true
	m.reason = reason
	m.status.State = "idle"
	return nil
}
func (m *mockBackend) Drain(_ context.Context, enabled bool, until *time.Time) error {
	m.drain = enabled
	return nil
}
func (m *mockBackend) Lockdown(_ context.Context) error {
	m.locked = true
	m.status.State = "lockdown"
	return nil
}
func (m *mockBackend) Unlock(_ context.Context) error {
	m.locked = false
	m.status.State = "idle"
	return nil
}
func (m *mockBackend) ConfigGet(_ context.Context) (map[string]string, error) {
	return m.cfg, nil
}
func (m *mockBackend) ConfigSet(_ context.Context, key, val string) error {
	if m.cfg == nil {
		m.cfg = map[string]string{}
	}
	m.cfg[key] = val
	return nil
}
func (m *mockBackend) ConfigReload(_ context.Context) error { return nil }
func (m *mockBackend) QueueList(_ context.Context) ([]db.ChunkRecord, error) {
	return []db.ChunkRecord{
		{ID: "01A", Status: "pending"},
	}, nil
}
func (m *mockBackend) QueueSkip(_ context.Context, chunkID string) error    { return nil }
func (m *mockBackend) QueueRetry(_ context.Context, chunkID string) error   { return nil }
func (m *mockBackend) QueueHold(_ context.Context, chunkID string) error    { return nil }
func (m *mockBackend) QueueRelease(_ context.Context, chunkID string) error { return nil }
func (m *mockBackend) QueuePin(_ context.Context, chunkID, tier, pool string) error {
	return nil
}
func (m *mockBackend) QuotaGet(_ context.Context) (map[string]any, error) {
	return map[string]any{"pools": map[string]any{}}, nil
}
func (m *mockBackend) QuotaSetFloor(_ context.Context, pool string, pct int) error   { return nil }
func (m *mockBackend) QuotaSetReserve(_ context.Context, pool string, pct int) error { return nil }
func (m *mockBackend) QuotaTierToggle(_ context.Context, tier string, enabled bool) error {
	return nil
}
func (m *mockBackend) QuotaResetBucket(_ context.Context, pool string) error { return nil }
func (m *mockBackend) Audit(_ context.Context, limit int) ([]eventlog.Event, error) {
	return m.events, nil
}
func (m *mockBackend) ValidateToken(_ context.Context, token string) (string, error) {
	if role, ok := m.tokens[token]; ok {
		return role, nil
	}
	return "", errors.New("invalid token")
}

func setupTestServer(t *testing.T) (*Server, *Client, *mockBackend, string) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "run", "control.sock")

	mb := &mockBackend{
		status: DaemonStatus{State: "idle"},
		tokens: map[string]string{
			"viewer-token":   RoleViewer,
			"operator-token": RoleOperator,
			"admin-token":    RoleAdmin,
		},
		cfg: map[string]string{"policy.max_consecutive_blocks": "3"},
		events: []eventlog.Event{
			{Seq: 1, Type: "daemon.started"},
		},
	}

	srv := NewServer(mb, sockPath)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start control server: %v", err)
	}

	client := NewClient(sockPath, "")
	return srv, client, mb, sockPath
}

func TestControlSocket_PermissionsAndBasicLifecycle(t *testing.T) {
	srv, client, mb, sockPath := setupTestServer(t)
	defer srv.Close()

	// Check socket file permissions (0600)
	fi, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("socket permissions = %o, expected 0600", fi.Mode().Perm())
	}

	ctx := context.Background()

	// Test Status
	st, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if st.State != "idle" {
		t.Fatalf("expected state 'idle', got %q", st.State)
	}

	// Test Start
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if mb.status.State != "running" {
		t.Fatalf("expected backend state 'running', got %q", mb.status.State)
	}

	// Test Halt with reason
	if err := client.Halt(ctx, "manual operator abort"); err != nil {
		t.Fatalf("Halt failed: %v", err)
	}
	if !mb.halted || mb.reason != "manual operator abort" {
		t.Fatalf("expected halt with reason, got halted=%v, reason=%q", mb.halted, mb.reason)
	}

	// Test Drain mode
	if err := client.Drain(ctx, true, nil); err != nil {
		t.Fatalf("Drain failed: %v", err)
	}
	if !mb.drain {
		t.Fatalf("expected drain = true")
	}

	// Test Lockdown
	if err := client.Lockdown(ctx); err != nil {
		t.Fatalf("Lockdown failed: %v", err)
	}
	if !mb.locked {
		t.Fatalf("expected locked = true")
	}

	// Test Unlock
	if err := client.Unlock(ctx); err != nil {
		t.Fatalf("Unlock failed: %v", err)
	}
	if mb.locked {
		t.Fatalf("expected locked = false")
	}
}

func TestControlSocket_RoleAuthorization(t *testing.T) {
	srv, _, _, sockPath := setupTestServer(t)
	defer srv.Close()

	ctx := context.Background()

	// 1. Viewer token
	viewerClient := NewClient(sockPath, "viewer-token")
	st, err := viewerClient.Status(ctx)
	if err != nil {
		t.Fatalf("viewer Status failed: %v", err)
	}
	if st.State != "idle" {
		t.Fatalf("unexpected state: %s", st.State)
	}

	// Viewer cannot start (requires operator)
	err = viewerClient.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "does not satisfy required role") {
		t.Fatalf("expected role forbidden error for viewer Start, got: %v", err)
	}

	// 2. Operator token
	opClient := NewClient(sockPath, "operator-token")
	if err := opClient.Start(ctx); err != nil {
		t.Fatalf("operator Start failed: %v", err)
	}

	// Operator cannot set config (requires admin)
	err = opClient.ConfigSet(ctx, "test_key", "test_val")
	if err == nil || !strings.Contains(err.Error(), "does not satisfy required role") {
		t.Fatalf("expected role forbidden error for operator ConfigSet, got: %v", err)
	}

	// 3. Admin token
	adminClient := NewClient(sockPath, "admin-token")
	if err := adminClient.ConfigSet(ctx, "test_key", "test_val"); err != nil {
		t.Fatalf("admin ConfigSet failed: %v", err)
	}

	cfg, err := adminClient.ConfigGet(ctx)
	if err != nil {
		t.Fatalf("ConfigGet failed: %v", err)
	}
	if cfg["test_key"] != "test_val" {
		t.Fatalf("expected config test_key = test_val, got: %v", cfg)
	}

	// 4. Invalid token
	badClient := NewClient(sockPath, "invalid-token")
	err = badClient.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "invalid bearer token") {
		t.Fatalf("expected invalid bearer token error, got: %v", err)
	}
}

func TestControlSocket_RequestSizeLimit(t *testing.T) {
	srv, _, _, sockPath := setupTestServer(t)
	defer srv.Close()

	ctx := context.Background()
	adminClient := NewClient(sockPath, "admin-token")

	// Payload larger than 1 MiB
	hugeVal := strings.Repeat("A", 1024*1024+100)
	err := adminClient.ConfigSet(ctx, "big_key", hugeVal)
	if err == nil {
		t.Fatal("expected request size limit rejection for payload > 1 MiB")
	}
}

func (m *mockBackend) RotateNtfyTopic(context.Context) (string, error) { return "", nil }

func (m *mockBackend) QueueSetActive(_ context.Context, id string, active bool) error {
	m.calls = append(m.calls, "active:"+id+":"+map[bool]string{true: "on", false: "off"}[active])
	return nil
}
func (m *mockBackend) QueueSetAllActive(_ context.Context, active bool) (int, error) {
	m.calls = append(m.calls, "all:"+map[bool]string{true: "on", false: "off"}[active])
	return 7, nil
}
func (m *mockBackend) QueueAssign(_ context.Context, id, tier, model string) error {
	if tier == "bad" {
		return errors.New("unknown tier \"bad\"")
	}
	m.calls = append(m.calls, "assign:"+id+":"+tier+":"+model)
	return nil
}
func (m *mockBackend) QueueRefresh(context.Context) (QueueRefreshResult, error) {
	m.calls = append(m.calls, "refresh")
	return QueueRefreshResult{Added: 2, Updated: 1, Invalid: 1, FirstError: "x.md: bad"}, nil
}

func (m *mockBackend) FrontRunStart(_ context.Context, tier, model string) error {
	m.calls = append(m.calls, "frontrun:"+tier+":"+model)
	return nil
}
func (m *mockBackend) FrontRunStop(context.Context) error {
	m.calls = append(m.calls, "frontrun-stop")
	return nil
}
func (m *mockBackend) QueueCheck(_ context.Context, id string) (QueueCheckResult, error) {
	m.calls = append(m.calls, "check:"+id)
	return QueueCheckResult{ChunkID: id, Before: "pending", After: "done", Reconciled: true}, nil
}

func TestControlSocket_QueueManagerRoundTripAndRoles(t *testing.T) {
	srv, _, mb, sockPath := setupTestServer(t)
	defer srv.Close()
	ctx := context.Background()
	op := NewClient(sockPath, "operator-token")

	if err := op.QueueSetActive(ctx, "01A", false); err != nil {
		t.Fatal(err)
	}
	if err := op.QueueSetActive(ctx, "01A", true); err != nil {
		t.Fatal(err)
	}
	n, err := op.QueueSetAllActive(ctx, false)
	if err != nil || n != 7 {
		t.Fatalf("QueueSetAllActive = %d, %v", n, err)
	}
	if err := op.QueueAssign(ctx, "01A", "strong", "claude-opus-5-5"); err != nil {
		t.Fatal(err)
	}
	res, err := op.QueueRefresh(ctx)
	if err != nil || res.Added != 2 || res.Updated != 1 || res.Invalid != 1 || res.FirstError != "x.md: bad" {
		t.Fatalf("QueueRefresh = %+v, %v", res, err)
	}
	want := []string{"active:01A:off", "active:01A:on", "all:off", "assign:01A:strong:claude-opus-5-5", "refresh"}
	if strings.Join(mb.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("backend calls = %v, want %v", mb.calls, want)
	}

	// Backend validation errors reach the caller.
	if err := op.QueueAssign(ctx, "01A", "bad", ""); err == nil || !strings.Contains(err.Error(), "unknown tier") {
		t.Fatalf("expected the backend's validation error, got %v", err)
	}
	// chunk_id is mandatory for per-chunk calls.
	if err := op.QueueSetActive(ctx, "", false); err == nil {
		t.Fatal("empty chunk_id should be rejected")
	}

	// Viewers may look but not change anything.
	viewer := NewClient(sockPath, "viewer-token")
	mb.calls = nil
	for name, err := range map[string]error{
		"active":    viewer.QueueSetActive(ctx, "01A", false),
		"assign":    viewer.QueueAssign(ctx, "01A", "strong", ""),
		"refresh":   func() error { _, e := viewer.QueueRefresh(ctx); return e }(),
		"all":       func() error { _, e := viewer.QueueSetAllActive(ctx, false); return e }(),
		"frontrun":  viewer.FrontRunStart(ctx, "strong", "m"),
		"frontstop": viewer.FrontRunStop(ctx),
	} {
		if err == nil || !strings.Contains(err.Error(), "does not satisfy required role") {
			t.Errorf("viewer %s should be forbidden, got %v", name, err)
		}
	}
	if len(mb.calls) != 0 {
		t.Errorf("forbidden calls reached the backend: %v", mb.calls)
	}
}
