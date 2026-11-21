package slots

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/erinwnash/Shepherd/internal/verify"
)

var (
	// ErrNoSlotAvailable indicates all slots in the pool are actively occupied.
	ErrNoSlotAvailable = errors.New("no worker slot currently available")
)

// GateExecutionMutex serializes verification gate (compilers, test suites)
// executions so only ONE test suite runs on the host at a time, preventing CPU cache
// thrashing and false timeouts.
type GateExecutionMutex struct {
	sem chan struct{}
}

// NewGateExecutionMutex creates a gate mutex with Concurrency = 1.
func NewGateExecutionMutex() *GateExecutionMutex {
	return &GateExecutionMutex{
		sem: make(chan struct{}, 1),
	}
}

// Lock acquires the gate execution mutex, blocking until acquired or context cancelled.
func (m *GateExecutionMutex) Lock(ctx context.Context) error {
	select {
	case m.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock releases the gate execution mutex.
func (m *GateExecutionMutex) Unlock() {
	select {
	case <-m.sem:
	default:
	}
}

// Slot represents one bounded local worker slot provisioned on a detached Git worktree.
type Slot struct {
	ID        string // e.g. "slot-1"
	Index     int    // 0, 1, ...
	Path      string // absolute path to .shepherd/slots/slot-N
	Git       *verify.GitRunner
	GPUIndex  int    // Pinned GPU device index (for CUDA_VISIBLE_DEVICES)
	InUse     bool
	UsingGPU  bool
}

// ExtractPatch returns a unified diff of all uncommitted modifications in the slot.
func (s *Slot) ExtractPatch(ctx context.Context) (string, error) {
	return s.Git.Exec(ctx, "diff", "HEAD")
}

// Reset cleans and resets the slot worktree back to HEAD.
func (s *Slot) Reset(ctx context.Context) error {
	if _, err := s.Git.Exec(ctx, "reset", "--hard", "HEAD"); err != nil {
		return fmt.Errorf("reset slot %s: %w", s.ID, err)
	}
	if _, err := s.Git.Exec(ctx, "clean", "-fd"); err != nil {
		return fmt.Errorf("clean slot %s: %w", s.ID, err)
	}
	return nil
}

// Pool coordinates a bounded collection of local worker slots with hardware protection.
type Pool struct {
	RepoRoot        string
	SlotsDir        string
	Slots           []*Slot
	Guard           *HardwareGuard
	GateMutex       *GateExecutionMutex
	DesktopPriority bool
	activeGPUCount  int
	mu              sync.Mutex
}

// NewPool initializes a local worker slot pool bounded by maxSlots.
func NewPool(repoRoot string, maxSlots int, guard *HardwareGuard, desktopPriority bool) (*Pool, error) {
	if maxSlots <= 0 {
		maxSlots = 2
	}
	if guard == nil {
		guard = NewHardwareGuard(4.0, 1)
	}

	absRepo, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}

	slotsDir := filepath.Join(absRepo, ".shepherd", "slots")
	p := &Pool{
		RepoRoot:        absRepo,
		SlotsDir:        slotsDir,
		Guard:           guard,
		GateMutex:       NewGateExecutionMutex(),
		DesktopPriority: desktopPriority,
	}

	for i := 0; i < maxSlots; i++ {
		slotID := fmt.Sprintf("slot-%d", i+1)
		slotPath := filepath.Join(slotsDir, slotID)
		gpuIdx := i % guard.MaxGPUWorkers
		p.Slots = append(p.Slots, &Slot{
			ID:       slotID,
			Index:    i,
			Path:     slotPath,
			Git:      verify.NewGitRunner(slotPath),
			GPUIndex: gpuIdx,
		})
	}

	return p, nil
}

// Provision sets up the slot directories and worktrees on detached HEAD.
func (p *Pool) Provision(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := os.MkdirAll(p.SlotsDir, 0755); err != nil {
		return fmt.Errorf("create slots dir %s: %w", p.SlotsDir, err)
	}

	rootGit := verify.NewGitRunner(p.RepoRoot)

	for _, slot := range p.Slots {
		if _, err := os.Stat(slot.Path); os.IsNotExist(err) {
			// Create worktree on detached HEAD (Zero permanent branches)
			_, err = rootGit.Exec(ctx, "worktree", "add", "--detach", slot.Path, "HEAD")
			if err != nil {
				return fmt.Errorf("git worktree add for %s: %w", slot.ID, err)
			}
		} else {
			// Worktree already exists: reset cleanly
			_ = slot.Reset(ctx)
		}
	}

	return nil
}

// Acquire reserves an available worker slot after verifying hardware headroom (RAM floor & GPU concurrency).
func (p *Pool) Acquire(ctx context.Context, requiresGPU bool) (*Slot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.Guard.CheckHeadroom(requiresGPU, p.activeGPUCount); err != nil {
		return nil, err
	}

	for _, slot := range p.Slots {
		if !slot.InUse {
			slot.InUse = true
			slot.UsingGPU = requiresGPU
			if requiresGPU {
				p.activeGPUCount++
			}
			return slot, nil
		}
	}

	return nil, ErrNoSlotAvailable
}

// Release returns a slot to the pool and resets its worktree back to HEAD.
func (p *Pool) Release(ctx context.Context, slot *Slot) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if slot == nil {
		return nil
	}

	if slot.UsingGPU {
		p.activeGPUCount--
		if p.activeGPUCount < 0 {
			p.activeGPUCount = 0
		}
	}

	slot.InUse = false
	slot.UsingGPU = false

	return slot.Reset(ctx)
}

// ApplyPatch applies an extracted unified diff directly to the primary repository.
func (p *Pool) ApplyPatch(ctx context.Context, patch string) error {
	if patch == "" {
		return nil
	}

	rootGit := verify.NewGitRunner(p.RepoRoot)

	tmpPatch := filepath.Join(os.TempDir(), fmt.Sprintf("shepherd-slot-apply-%d.patch", os.Getpid()))
	if err := os.WriteFile(tmpPatch, []byte(patch), 0600); err != nil {
		return fmt.Errorf("write temp patch: %w", err)
	}
	defer os.Remove(tmpPatch)

	_, err := rootGit.Exec(ctx, "apply", "--ignore-whitespace", "--recount", tmpPatch)
	if err != nil {
		return fmt.Errorf("git apply patch from slot: %w", err)
	}

	return nil
}

// Close removes the detached slot worktrees on shutdown.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	rootGit := verify.NewGitRunner(p.RepoRoot)
	for _, slot := range p.Slots {
		_, _ = rootGit.Exec(ctx, "worktree", "remove", "--force", slot.Path)
		_ = os.RemoveAll(slot.Path)
	}
	_ = os.RemoveAll(p.SlotsDir)
	return nil
}
