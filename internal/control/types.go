package control

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/eventlog"
)

// Roles per ADR-0003.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// DaemonStatus captures the live state of the controller daemon.
type DaemonStatus struct {
	State          string            `json:"state"`
	RunningChunkID string            `json:"running_chunk_id,omitempty"`
	WorkerID       string            `json:"worker_id,omitempty"`
	ActiveTier     string            `json:"active_tier,omitempty"`
	Stack          []string          `json:"stack,omitempty"`
	DrainMode      bool              `json:"drain_mode"`
	DrainUntil     *time.Time        `json:"drain_until,omitempty"`
	Lockdown       bool              `json:"lockdown"`
	QueuePending   int               `json:"queue_pending"`
	QueueRunning   int               `json:"queue_running"`
	QueueBlocked   int               `json:"queue_blocked"`
	QueueCompleted int               `json:"queue_completed"`
	TotalCostUSD   float64           `json:"total_cost_usd,omitempty"`
	TotalTokens    int               `json:"total_tokens,omitempty"`
	Overrides      map[string]string `json:"overrides,omitempty"`
	NtfyTopic      string            `json:"ntfy_topic,omitempty"` // ntfy URL notifications go to
	FrontRun       *FrontRunStatus   `json:"front_run,omitempty"`  // nil when the front-run lane is off
	Progress       []ChunkProgress   `json:"progress,omitempty"`   // chunks being worked on right now
}

// ChunkProgress is the live state of a chunk being worked on: where it is in the pipeline, how much
// of the worktree it has touched, and the tail of the agent's output.
type ChunkProgress struct {
	ID       string   `json:"id"`
	Lane     string   `json:"lane"` // "main" or "frontrun"
	Phase    string   `json:"phase"`
	Detail   string   `json:"detail,omitempty"` // tier / model
	Attempt  int      `json:"attempt,omitempty"`
	PhaseSec int      `json:"phase_sec"` // time in this phase
	TotalSec int      `json:"total_sec"` // time since the chunk started
	Files    int      `json:"files"`
	Added    int      `json:"added"`
	Deleted  int      `json:"deleted"`
	IdleSec  int      `json:"idle_sec"` // time since the worktree last changed
	Stalled  bool     `json:"stalled,omitempty"`
	Tail     []string `json:"tail,omitempty"` // last lines of agent output, redacted
}

// FrontRunStatus describes the front-run burn lane: a second queue that works ahead of the main one.
type FrontRunStatus struct {
	Tier    string `json:"tier"`
	Model   string `json:"model,omitempty"`
	Current string `json:"current,omitempty"` // chunk being worked on
	Landed  int    `json:"landed"`            // chunks finished and merged into the main tree
	Failed  int    `json:"failed"`            // attempts handed back to the main queue
	Note    string `json:"note,omitempty"`    // what the lane is waiting for, e.g. low RAM
}

// FrontRunStartRequest picks the tier and model the front-run lane burns. The tier may be switched off.
type FrontRunStartRequest struct {
	Tier  string `json:"tier"`
	Model string `json:"model,omitempty"`
}

// Request payloads for API endpoints.
type HaltRequest struct {
	Reason string `json:"reason"`
}

type DrainRequest struct {
	Enabled bool       `json:"enabled"`
	Until   *time.Time `json:"until,omitempty"`
}

type ConfigSetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type QueueActionRequest struct {
	ChunkID string `json:"chunk_id"`
}

type QueueRejectRequest struct {
	ChunkID string `json:"chunk_id"`
	Reason  string `json:"reason"`
}

type QueuePinRequest struct {
	ChunkID string `json:"chunk_id"`
	Tier    string `json:"tier"`
	Pool    string `json:"pool"`
}

type QueueActiveRequest struct {
	ChunkID string `json:"chunk_id"`
	Active  bool   `json:"active"`
}

type QueueActiveAllRequest struct {
	Active bool `json:"active"`
}

type QueueActiveAllResult struct {
	Changed int `json:"changed"`
}

type QueueAssignRequest struct {
	ChunkID string `json:"chunk_id"`
	Tier    string `json:"tier"`  // empty = back to automatic routing
	Model   string `json:"model"` // empty = the tier's configured model
}

// QueueRefreshResult reports what re-reading the plan tree from disk changed.
type QueueRefreshResult struct {
	Added      int    `json:"added"`
	Updated    int    `json:"updated"`
	Invalid    int    `json:"invalid"`
	Reconciled int    `json:"reconciled"` // rows marked done because their work order is in a done directory
	FirstError string `json:"first_error,omitempty"`
}

// QueueCheckResult is the spot-test of one chunk against the plan tree and the done directories.
type QueueCheckResult struct {
	ChunkID    string   `json:"chunk_id"`
	Before     string   `json:"before"` // database status before the check ("" = no row)
	After      string   `json:"after"`
	InPlanTree bool     `json:"in_plan_tree"`
	PlanPath   string   `json:"plan_path,omitempty"`
	DonePath   string   `json:"done_path,omitempty"`
	Reconciled bool     `json:"reconciled"`
	DependsOn  []string `json:"depends_on,omitempty"` // "ID=status"
	Note       string   `json:"note"`
}

type QuotaSetPctRequest struct {
	Pool string `json:"pool"`
	Pct  int    `json:"pct"`
}

type QuotaTierRequest struct {
	Tier    string `json:"tier"`
	Enabled bool   `json:"enabled"`
}

type QuotaResetBucketRequest struct {
	Pool string `json:"pool"`
}

// ReviewBackend is an optional extension of Backend for inspecting and approving/rejecting held chunk diffs.
type ReviewBackend interface {
	QueueDiff(ctx context.Context, chunkID string) (string, error)
	QueueApprove(ctx context.Context, chunkID string) error
	QueueReject(ctx context.Context, chunkID, reason string) error
}

// Backend coordinates the daemon state and execution engine behind the control API.
type Backend interface {
	Status(ctx context.Context) (DaemonStatus, error)
	Start(ctx context.Context) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Stop(ctx context.Context) error
	Halt(ctx context.Context, reason string) error
	Drain(ctx context.Context, enabled bool, until *time.Time) error
	Lockdown(ctx context.Context) error
	Unlock(ctx context.Context) error
	RotateNtfyTopic(ctx context.Context) (string, error)

	ConfigGet(ctx context.Context) (map[string]string, error)
	ConfigSet(ctx context.Context, key, val string) error
	ConfigReload(ctx context.Context) error

	QueueList(ctx context.Context) ([]db.ChunkRecord, error)
	QueueSkip(ctx context.Context, chunkID string) error
	QueueRetry(ctx context.Context, chunkID string) error
	QueueHold(ctx context.Context, chunkID string) error
	QueueRelease(ctx context.Context, chunkID string) error
	QueuePin(ctx context.Context, chunkID, tier, pool string) error
	QueueSetActive(ctx context.Context, chunkID string, active bool) error
	QueueSetAllActive(ctx context.Context, active bool) (int, error)
	QueueAssign(ctx context.Context, chunkID, tier, model string) error
	QueueRefresh(ctx context.Context) (QueueRefreshResult, error)
	QueueCheck(ctx context.Context, chunkID string) (QueueCheckResult, error)
	FrontRunStart(ctx context.Context, tier, model string) error
	FrontRunStop(ctx context.Context) error

	QuotaGet(ctx context.Context) (map[string]any, error)
	QuotaSetFloor(ctx context.Context, pool string, pct int) error
	QuotaSetReserve(ctx context.Context, pool string, pct int) error
	QuotaTierToggle(ctx context.Context, tier string, enabled bool) error
	QuotaResetBucket(ctx context.Context, pool string) error

	Audit(ctx context.Context, limit int) ([]eventlog.Event, error)
	ValidateToken(ctx context.Context, token string) (string, error)
}

// DefaultSocketPath returns the standard control socket path per ADR-0003.
func DefaultSocketPath() string {
	if s := os.Getenv("SHEPHERD_SOCKET"); s != "" {
		return s
	}
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "shepherd", "run", "control.sock")
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "shepherd", "control.sock")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "shepherd", "control.sock")
}
