package leases_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/leases"
)

func setupTestStore(t *testing.T) *db.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "shepherd.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func TestLeases_FloorProtection_STRIDE_D3(t *testing.T) {
	store := setupTestStore(t)
	pools := map[string]config.PoolConfig{
		"claude": {
			Mode:           "polled",
			FloorWeeklyPct: 20, // max spend allowed = 80%
		},
	}
	// Currently 60% used. Headroom left before floor = 20% (since 80% - 60% = 20%).
	provider := &leases.FixedUsageProvider{
		Weekly: map[string]int{"claude": 60},
	}
	mgr := leases.NewManager(store, pools, provider)

	ctx := context.Background()

	// Worker 1 requests 15% - should succeed (60 + 15 = 75 <= 80)
	l1, err := mgr.Acquire(ctx, leases.Request{
		WorkerID:     "worker-1",
		ChunkID:      "c1",
		Pool:         "claude",
		EstimatedPct: 15,
	})
	if err != nil {
		t.Fatalf("Worker 1 acquire failed unexpectedly: %v", err)
	}
	if l1 == nil || l1.ID == "" {
		t.Fatalf("Worker 1 lease ID is empty")
	}

	// Worker 2 requests 10% - should fail because 60 + 15 (l1) + 10 = 85 > 80 (STRIDE D3)
	_, err = mgr.Acquire(ctx, leases.Request{
		WorkerID:     "worker-2",
		ChunkID:      "c2",
		Pool:         "claude",
		EstimatedPct: 10,
	})
	if err == nil {
		t.Fatalf("Worker 2 expected ErrFloorExceeded, got nil")
	}
	if !errors.Is(err, leases.ErrFloorExceeded) {
		t.Fatalf("Worker 2 expected ErrFloorExceeded, got: %v", err)
	}

	// Worker 3 requests with Force: true - should succeed despite floor
	l3, err := mgr.Acquire(ctx, leases.Request{
		WorkerID:     "worker-3",
		ChunkID:      "c3",
		Pool:         "claude",
		EstimatedPct: 10,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("Force acquire failed: %v", err)
	}
	if l3 == nil {
		t.Fatalf("Force lease is nil")
	}

	// Release Worker 1's lease with 12% actual spend
	if err := mgr.Release(ctx, l1.ID, 12); err != nil {
		t.Fatalf("Release l1 failed: %v", err)
	}

	// Worker 2 tries again with 5% - should now succeed because l1 is released (60 + 0 + 5 = 65 <= 80)
	l2, err := mgr.Acquire(ctx, leases.Request{
		WorkerID:     "worker-2",
		ChunkID:      "c2",
		Pool:         "claude",
		EstimatedPct: 5,
	})
	if err != nil {
		t.Fatalf("Worker 2 second acquire failed: %v", err)
	}
	if l2 == nil {
		t.Fatalf("Worker 2 lease is nil")
	}
}

func TestLeases_RevokedWorker_STRIDE_S4(t *testing.T) {
	store := setupTestStore(t)
	mgr := leases.NewManager(store, nil, nil)
	ctx := context.Background()

	// Register worker as active
	err := store.UpsertWorker(db.WorkerRecord{
		ID:         "worker-bad",
		PubkeyHash: "aabbcc",
		Status:     "active",
		Role:       "worker",
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(7 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("UpsertWorker: %v", err)
	}

	// Acquire lease works
	l, err := mgr.Acquire(ctx, leases.Request{
		WorkerID:     "worker-bad",
		ChunkID:      "c1",
		Pool:         "default",
		EstimatedPct: 5,
	})
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	// Revoke worker
	if err := store.SetWorkerStatus("worker-bad", "revoked"); err != nil {
		t.Fatalf("SetWorkerStatus: %v", err)
	}

	// Subsequent acquire should fail
	_, err = mgr.Acquire(ctx, leases.Request{
		WorkerID:     "worker-bad",
		ChunkID:      "c2",
		Pool:         "default",
		EstimatedPct: 5,
	})
	if err == nil {
		t.Fatalf("Expected error for revoked worker, got nil")
	}
	if !errors.Is(err, leases.ErrWorkerRevoked) {
		t.Fatalf("Expected ErrWorkerRevoked, got: %v", err)
	}

	// Clean up initial lease
	_ = mgr.Release(ctx, l.ID, 0)
}

func TestLeases_HeartbeatAndReclaim(t *testing.T) {
	store := setupTestStore(t)
	mgr := leases.NewManager(store, nil, nil)
	ctx := context.Background()

	l, err := mgr.Acquire(ctx, leases.Request{
		WorkerID:     "worker-hb",
		ChunkID:      "c-hb",
		Pool:         "test",
		EstimatedPct: 5,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Heartbeat should succeed
	if err := mgr.Heartbeat(ctx, l.ID); err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}

	// Heartbeat on nonexistent lease should fail
	if err := mgr.Heartbeat(ctx, "nonexistent-lease"); !errors.Is(err, leases.ErrLeaseNotFound) {
		t.Fatalf("Expected ErrLeaseNotFound, got: %v", err)
	}

	// Force expiration of l in DB to simulate timeout
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	_, err = store.DB.Exec("UPDATE leases SET expires_at = ? WHERE id = ?", past, l.ID)
	if err != nil {
		t.Fatalf("DB update: %v", err)
	}

	// Heartbeat on expired lease should fail
	if err := mgr.Heartbeat(ctx, l.ID); !errors.Is(err, leases.ErrLeaseNotFound) {
		t.Fatalf("Expected ErrLeaseNotFound on expired lease, got: %v", err)
	}

	// Acquire new lease should automatically reclaim expired lease
	_, err = mgr.Acquire(ctx, leases.Request{
		WorkerID:     "worker-hb2",
		ChunkID:      "c-hb2",
		Pool:         "test",
		EstimatedPct: 5,
	})
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}

	// Verify l is marked expired
	var status string
	_ = store.DB.QueryRow("SELECT status FROM leases WHERE id = ?", l.ID).Scan(&status)
	if status != "expired" {
		t.Fatalf("Expected lease status to be 'expired', got %q", status)
	}
}
