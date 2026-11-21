// Package leases implements distributed quota leasing and floor protection per ADR-0001 and STRIDE D3.
// Prevents concurrent workers from jointly overshooting weekly or 5-hour quota floors.
package leases

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/db"
)

// DefaultLeaseDuration is 15 minutes, renewable via heartbeat.
const DefaultLeaseDuration = 15 * time.Minute

// Common lease errors.
var (
	ErrFloorExceeded  = errors.New("quota lease denied: spending would exceed pool floor (STRIDE D3)")
	ErrWorkerRevoked  = errors.New("worker identity is revoked or not authorized (STRIDE S4)")
	ErrLeaseNotFound  = errors.New("lease not found or already released")
	ErrLeaseExpired   = errors.New("lease has expired")
)

// UsageProvider allows injecting live pool usage readings into the lease manager.
type UsageProvider interface {
	CurrentUsagePct(pool string) (weeklyPct int, fiveHourPct int, err error)
}

// FixedUsageProvider provides static readings for testing or cached values.
type FixedUsageProvider struct {
	Weekly   map[string]int
	FiveHour map[string]int
}

func (f *FixedUsageProvider) CurrentUsagePct(pool string) (int, int, error) {
	w := 0
	if f.Weekly != nil {
		w = f.Weekly[pool]
	}
	fh := 0
	if f.FiveHour != nil {
		fh = f.FiveHour[pool]
	}
	return w, fh, nil
}

// Request specifies a quota lease reservation.
type Request struct {
	WorkerID     string
	ChunkID      string
	Pool         string
	EstimatedPct int
	Force        bool // allows emergency over-floor spend, audited
}

// Lease holds details of an active quota lease.
type Lease struct {
	ID           string
	WorkerID     string
	ChunkID      string
	Pool         string
	EstimatedPct int
	ExpiresAt    time.Time
}

// Manager coordinates distributed quota reservations and prevents concurrent overshoots.
type Manager struct {
	mu       sync.Mutex
	store    *db.Store
	pools    map[string]config.PoolConfig
	provider UsageProvider
}

// NewManager creates a LeaseManager.
func NewManager(store *db.Store, pools map[string]config.PoolConfig, provider UsageProvider) *Manager {
	if provider == nil {
		provider = &FixedUsageProvider{}
	}
	return &Manager{
		store:    store,
		pools:    pools,
		provider: provider,
	}
}

// Acquire requests a quota reservation before a chunk is executed.
func (m *Manager) Acquire(ctx context.Context, req Request) (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Check worker identity & status
	if m.store != nil && req.WorkerID != "" {
		w, err := m.store.GetWorker(req.WorkerID)
		if err == nil {
			if w.Status == "revoked" {
				return nil, ErrWorkerRevoked
			}
		}
	}

	// 2. Reclaim any expired leases first
	if m.store != nil {
		_, _ = m.store.ReclaimExpiredLeases()
	}

	// 3. Check pool floor limits
	poolCfg, hasPool := m.pools[req.Pool]
	if hasPool && poolCfg.Mode == "polled" && !req.Force {
		curWeekly, _, err := m.provider.CurrentUsagePct(req.Pool)
		if err == nil {
			floorWeekly := poolCfg.FloorWeeklyPct
			if floorWeekly <= 0 {
				floorWeekly = 20 // default 20% floor
			}

			// Sum active leases on this pool
			activeLeaseSum := 0
			if m.store != nil {
				leases, _ := m.store.GetActiveLeases(req.Pool)
				for _, l := range leases {
					activeLeaseSum += l.EstimatedPct
				}
			}

			// Headroom check: remaining budget must be strictly >= floor
			maxSpendAllowed := 100 - floorWeekly
			if curWeekly+activeLeaseSum+req.EstimatedPct > maxSpendAllowed {
				return nil, fmt.Errorf("%w: pool %s used=%d%%, activeLeases=%d%%, requested=%d%%, floor=%d%% (max allowed spend=%d%%)",
					ErrFloorExceeded, req.Pool, curWeekly, activeLeaseSum, req.EstimatedPct, floorWeekly, maxSpendAllowed)
			}
		}
	}

	// 4. Generate lease ID
	idBytes := make([]byte, 16)
	_, _ = rand.Read(idBytes)
	leaseID := hex.EncodeToString(idBytes)

	expiresAt := time.Now().Add(DefaultLeaseDuration)
	record := db.LeaseRecord{
		ID:           leaseID,
		WorkerID:     req.WorkerID,
		ChunkID:      req.ChunkID,
		Pool:         req.Pool,
		EstimatedPct: req.EstimatedPct,
		Status:       "active",
		ExpiresAt:    expiresAt,
		CreatedAt:    time.Now(),
	}

	if m.store != nil {
		if err := m.store.CreateLease(record); err != nil {
			return nil, fmt.Errorf("create lease record: %w", err)
		}
	}

	return &Lease{
		ID:           leaseID,
		WorkerID:     req.WorkerID,
		ChunkID:      req.ChunkID,
		Pool:         req.Pool,
		EstimatedPct: req.EstimatedPct,
		ExpiresAt:    expiresAt,
	}, nil
}

// Heartbeat renews an active lease.
func (m *Manager) Heartbeat(ctx context.Context, leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.store == nil {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	newExpiry := time.Now().Add(DefaultLeaseDuration).UTC().Format(time.RFC3339)
	res, err := m.store.DB.Exec("UPDATE leases SET expires_at = ? WHERE id = ? AND status = 'active' AND expires_at > ?", newExpiry, leaseID, now)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrLeaseNotFound
	}
	return nil
}

// Release completes a lease and logs measured spend in the ledger.
func (m *Manager) Release(ctx context.Context, leaseID string, actualSpendPct int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.store == nil {
		return nil
	}

	// Lookup lease record
	row := m.store.DB.QueryRow("SELECT chunk_id, pool FROM leases WHERE id = ?", leaseID)
	var chunkID, pool string
	if err := row.Scan(&chunkID, &pool); err != nil {
		return ErrLeaseNotFound
	}

	if err := m.store.ReleaseLease(leaseID); err != nil {
		return err
	}

	if actualSpendPct > 0 {
		_ = m.store.RecordSpend(chunkID, pool, "weekly", actualSpendPct)
	}

	return nil
}
