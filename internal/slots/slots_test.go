package slots_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/slots"
	"github.com/erinwnash/Shepherd/internal/verify"
)

func TestGateExecutionMutex_ConcurrencyOne(t *testing.T) {
	ctx := context.Background()
	m := slots.NewGateExecutionMutex()

	if err := m.Lock(ctx); err != nil {
		t.Fatalf("Lock failed: %v", err)
	}

	// Try acquiring again with a timeout context; must time out because concurrency = 1
	timeoutCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()

	err := m.Lock(timeoutCtx)
	if err == nil {
		t.Fatalf("expected lock acquisition to block/timeout, but succeeded")
	}

	m.Unlock()

	// Now acquisition must succeed immediately
	if err := m.Lock(ctx); err != nil {
		t.Fatalf("Lock after unlock failed: %v", err)
	}
	m.Unlock()
}

func TestPool_LifecycleAndPatchApplication(t *testing.T) {
	ctx := context.Background()
	repoDir := t.TempDir()

	git := verify.NewGitRunner(repoDir)
	if _, err := git.Exec(ctx, "init"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	_, _ = git.Exec(ctx, "config", "user.email", "tester@test.local")
	_, _ = git.Exec(ctx, "config", "user.name", "Tester")

	// Commit initial file to main worktree
	baseFile := filepath.Join(repoDir, "app.txt")
	if err := os.WriteFile(baseFile, []byte("version 1.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Exec(ctx, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Exec(ctx, "commit", "-m", "init"); err != nil {
		t.Fatal(err)
	}

	guard := slots.NewHardwareGuard(1.0, 1)
	guard.RAMChecker = func() (float64, error) { return 16.0, nil }

	pool, err := slots.NewPool(repoDir, 2, guard, true)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}

	if err := pool.Provision(ctx); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		_ = pool.Close(ctx)
	}()

	// Acquire slot 1
	slot1, err := pool.Acquire(ctx, false)
	if err != nil {
		t.Fatalf("Acquire slot 1 failed: %v", err)
	}
	if slot1.ID != "slot-1" {
		t.Errorf("expected slot-1, got %s", slot1.ID)
	}

	// In slot 1, modify app.txt
	slotFile := filepath.Join(slot1.Path, "app.txt")
	if err := os.WriteFile(slotFile, []byte("version 2.0 (from slot 1)\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Extract patch from slot 1
	patch, err := slot1.ExtractPatch(ctx)
	if err != nil {
		t.Fatalf("ExtractPatch failed: %v", err)
	}
	if patch == "" {
		t.Fatalf("expected non-empty patch from slot 1")
	}

	// Release slot 1
	if err := pool.Release(ctx, slot1); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	// Apply patch onto main repository
	if err := pool.ApplyPatch(ctx, patch); err != nil {
		t.Fatalf("ApplyPatch failed: %v", err)
	}

	// Verify main repo has updated content
	updated, err := os.ReadFile(baseFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) != "version 2.0 (from slot 1)\n" {
		t.Errorf("main repo not updated! got: %q", string(updated))
	}
}
