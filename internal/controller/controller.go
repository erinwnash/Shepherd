// Package controller implements the single-writer daemon coordinator and worker execution loop per ADR-0001.
// Manages the state machine (idle, running, paused, draining, stopping, lockdown), worktree commits,
// safe halt-and-rollback, quota leases, live config overrides, and the local control plane backend.
package controller

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/adapter"
	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/eventlog"
	"github.com/erinwnash/Shepherd/internal/lan"
	"github.com/erinwnash/Shepherd/internal/notify"
	"github.com/erinwnash/Shepherd/internal/onboard"
	"github.com/erinwnash/Shepherd/internal/plan"
	"github.com/erinwnash/Shepherd/internal/predict"
	"github.com/erinwnash/Shepherd/internal/quota"
	"github.com/erinwnash/Shepherd/internal/redact"
	"github.com/erinwnash/Shepherd/internal/remote"
	"github.com/erinwnash/Shepherd/internal/repair"
	"github.com/erinwnash/Shepherd/internal/repomap"
	"github.com/erinwnash/Shepherd/internal/sandbox"
	"github.com/erinwnash/Shepherd/internal/scope"
	"github.com/erinwnash/Shepherd/internal/slots"
	"github.com/erinwnash/Shepherd/internal/source"
	"github.com/erinwnash/Shepherd/internal/verify"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

// Daemon states.
const (
	StateIdle     = "idle"
	StateRunning  = "running"
	StatePaused   = "paused"
	StateDraining = "draining"
	StateStopping = "stopping"
	StateLockdown = "lockdown"
)

// Options configures the controller daemon.
type Options struct {
	ProjectRoot string
	PlanRoot    string
	Config      *config.Config
	Store       *db.Store
	Adapters    map[string]adapter.Adapter
	NoCloud     bool
}

// activeRun is one in-flight executeChunk call.
type activeRun struct {
	cancel    context.CancelFunc
	preCommit string
	done      chan struct{}
}

// NtfyTopicOverrideFile (in .shepherd/) holds the topic URL chosen by RotateNtfyTopic. It wins over
// shepherd.toml and NTFY_*/NOTIFY_CMD so a rotation survives restarts.
const NtfyTopicOverrideFile = "ntfy_topic_override"

// localWorkerID is the worker id of chunks executed in-process by this controller.
const localWorkerID = "local-worker-1"

// haltWait bounds how long Halt waits for cancelled runs to exit before rolling back.
const haltWait = 15 * time.Second

// Controller coordinates the queue, state database, sandbox execution, and control API.
type Controller struct {
	mu                sync.Mutex
	commitMu          sync.Mutex
	state             string
	drain             bool
	drainUntil        *time.Time
	lockdown          bool
	consecutiveBlocks int
	noCloud           bool
	inStopgap         bool
	limitedUntil      map[string]time.Time // pool -> when a provider limit resets

	projectRoot  string
	planRoot     string
	cfg          *config.Config
	store        *db.Store
	adapters     map[string]adapter.Adapter
	git          *verify.GitRunner
	verifier     *verify.Verifier
	predictModel predict.Model

	activeChunk    *workorder.WorkOrder
	activeCancel   context.CancelFunc
	preChunkCommit string
	runs           map[string]*activeRun // every in-flight chunk, including parallel slots
	lastStall      string                // last reported stall, so each distinct stall is reported once
	onNtfyTopic    func(topicURL string) // called after a live topic rotation (ntfy listener)

	workOrders map[string]*workorder.WorkOrder // in-memory parsed work orders
	notifier   *notify.Dispatcher
	slotsPool  *slots.Pool

	prog         progressTracker                                                                      // live per-chunk phase, diff and output tail (own leaf lock)
	frMu         sync.Mutex                                                                           // guards frontRun and its counters; a leaf lock, never held while taking mu
	frontRun     *frontRunLane                                                                        // the front-run burn lane, nil when off
	frontRunPoll func(ctx context.Context, pool string, pc config.PoolConfig) ([]quota.Bucket, error) // test seam; nil polls the pool's usage command
	frReady      *frontRunReady                                                                       // verified front-run patch waiting for the main queue to be between chunks (guarded by mu)

	wakeCh chan struct{}
	stopCh chan struct{}
}

// New creates a new Controller daemon instance.
func New(opts Options) (*Controller, error) {
	if opts.Store == nil {
		return nil, errors.New("db store is required")
	}
	if opts.Config == nil {
		return nil, errors.New("config is required")
	}
	if opts.ProjectRoot == "" {
		opts.ProjectRoot = "."
	}
	if opts.PlanRoot == "" {
		if opts.Config.Queue.Root != "" {
			opts.PlanRoot = opts.Config.Queue.Root
		} else {
			opts.PlanRoot = "docs/plan"
		}
	}
	if opts.Config.Queue.DoneMarker == "" {
		opts.Config.Queue.DoneMarker = "move-to-done"
	}
	if opts.Config.Queue.DoneDir == "" && (opts.Config.Queue.DoneMarker == "" || opts.Config.Queue.DoneMarker == "move-to-done") {
		opts.Config.Queue.DoneDir = filepath.ToSlash(opts.PlanRoot + "DONE")
	}
	if opts.Adapters == nil {
		opts.Adapters = adapter.AllAdapters()
	}

	// Ensure local OpenCode instance is registered in the runtime tier & pool stack if available on host
	if _, hasOCAdapter := opts.Adapters["opencode"]; hasOCAdapter {
		hasLocalOC := false
		for name, t := range opts.Config.Tiers {
			adLower := strings.ToLower(t.Adapter)
			if name == "local" || name == "opencode" || adLower == "opencode" || adLower == "openai-compat" {
				hasLocalOC = true
				break
			}
		}
		if !hasLocalOC {
			ocInfo := onboard.DetectOpenCodeInfo(opts.ProjectRoot)
			if ocInfo.Installed || ocInfo.ConfigPath != "" {
				if opts.Config.Tiers == nil {
					opts.Config.Tiers = make(map[string]config.TierConfig)
				}
				localURL := ocInfo.BaseURL
				if localURL == "" {
					localURL = "http://127.0.0.1:8080/v1"
				}
				opts.Config.Tiers["local"] = config.TierConfig{
					Adapter: "opencode",
					Model:   ocInfo.DefaultModel,
					URL:     localURL,
					Pool:    "local",
				}
				if opts.Config.Quota.Pools == nil {
					opts.Config.Quota.Pools = make(map[string]config.PoolConfig)
				}
				if _, ok := opts.Config.Quota.Pools["local"]; !ok {
					opts.Config.Quota.Pools["local"] = config.PoolConfig{
						Mode:       "until-rejected",
						BackoffMin: []int{10, 30},
					}
				}
				if len(opts.Config.Repair.Chain) > 0 {
					opts.Config.Repair.Chain = append([]string{"local:3"}, opts.Config.Repair.Chain...)
				} else {
					opts.Config.Repair.Chain = []string{"local:3"}
				}
			}
		}
	}

	guard := slots.NewHardwareGuard(opts.Config.Slots.MinFreeRAMGB, opts.Config.Slots.MaxGPUWorkers)
	pool, _ := slots.NewPool(opts.ProjectRoot, opts.Config.Slots.MaxSlots, guard, opts.Config.Slots.DesktopPriority)

	v := verify.NewVerifier(opts.Config.VerifierGates())
	v.FlakeRetries = opts.Config.Verify.FlakeRetries
	v.BaseCommitDelta = opts.Config.Verify.BaseCommitDelta
	if pool != nil {
		v.GateMutex = pool.GateMutex
	}
	if opts.Config.Slots.DesktopPriority {
		v.PriorityWrapper = slots.WrapPriority
	}
	if opts.Config.Policy.SandboxBackend != "" {
		v.SandboxWrapper = func(argv []string) ([]string, error) {
			return sandbox.Wrap(argv, sandbox.Profile{
				WorktreeDir:      opts.ProjectRoot,
				AllowUnsandboxed: opts.Config.Policy.AllowUnsandboxed,
				AllowNetwork:     false,
				Backend:          opts.Config.Policy.SandboxBackend,
				ContainerImage:   opts.Config.Policy.ContainerImage,
			})
		}
	}

	c := &Controller{
		state:       StateIdle,
		projectRoot: opts.ProjectRoot,
		planRoot:    opts.PlanRoot,
		cfg:         opts.Config,
		store:       opts.Store,
		adapters:    opts.Adapters,
		git:         verify.NewGitRunner(opts.ProjectRoot),
		verifier:    v,
		slotsPool:   pool,
		workOrders:  make(map[string]*workorder.WorkOrder),
		runs:        make(map[string]*activeRun),
		wakeCh:      make(chan struct{}, 1),
		stopCh:      make(chan struct{}),
		noCloud:     opts.NoCloud,
		notifier: notify.NewDispatcher(notify.Config{
			Events:            opts.Config.Notify.Events,
			SlackWebhookURL:   opts.Config.Notify.SlackWebhookURL,
			DiscordWebhookURL: opts.Config.Notify.DiscordWebhookURL,
			Desktop:           opts.Config.Notify.Desktop,
			NtfyURL:           opts.Config.Notify.NtfyURL,
			NtfyTopic:         opts.Config.Notify.NtfyTopic,
			NtfyTokenEnv:      opts.Config.Notify.NtfyTokenEnv,
			NotifyCmd:         opts.Config.Notify.NotifyCmd,
		}),
	}

	if u := readNtfyOverride(opts.ProjectRoot); u != "" {
		c.notifier.SetNtfyURL(u)
	}

	return c, nil
}

func readNtfyOverride(projectRoot string) string {
	data, err := os.ReadFile(filepath.Join(projectRoot, ".shepherd", NtfyTopicOverrideFile))
	if err != nil {
		return ""
	}
	u := strings.TrimSpace(string(data))
	if remote.ValidateURL(u) != nil {
		return ""
	}
	return u
}

// NtfyURL returns the ntfy topic URL notifications currently go to ("" if none).
func (c *Controller) NtfyURL() string {
	if c.notifier == nil {
		return ""
	}
	return c.notifier.NtfyURL()
}

// SetNtfyTopicHook registers a callback run after RotateNtfyTopic (the daemon moves the ntfy
// command listener with it).
func (c *Controller) SetNtfyTopicHook(fn func(topicURL string)) {
	c.mu.Lock()
	c.onNtfyTopic = fn
	c.mu.Unlock()
}

// RotateNtfyTopic switches notifications to a fresh random topic on the same ntfy server, live.
// The retired topic gets a notice that does NOT name the new topic: rotation exists to cut off
// anyone who learned the old one. The new topic is shown on the dashboard and persisted.
func (c *Controller) RotateNtfyTopic(ctx context.Context) (string, error) {
	c.mu.Lock()
	oldURL := c.NtfyURL()
	if oldURL == "" {
		c.mu.Unlock()
		return "", errors.New("no ntfy topic to rotate: set [notify] ntfy_topic in shepherd.toml (or NTFY_TOPIC / NOTIFY_CMD) first")
	}
	u, err := url.Parse(oldURL)
	if err != nil {
		c.mu.Unlock()
		return "", fmt.Errorf("parse ntfy url: %w", err)
	}
	token, err := randomTopicToken(24)
	if err != nil {
		c.mu.Unlock()
		return "", err
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	segs[len(segs)-1] = ntfyTopicPrefix(c.cfg.Project.Name) + "-" + token
	u.Path = "/" + strings.Join(segs, "/")
	u.RawQuery, u.Fragment = "", ""
	newURL := u.String()

	stateDir := filepath.Join(c.projectRoot, ".shepherd")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		c.mu.Unlock()
		return "", err
	}
	if err := os.WriteFile(filepath.Join(stateDir, NtfyTopicOverrideFile), []byte(newURL+"\n"), 0o600); err != nil {
		c.mu.Unlock()
		return "", fmt.Errorf("persist rotated topic: %w", err)
	}
	c.notifier.SetNtfyURL(newURL)
	hook := c.onNtfyTopic
	_, _ = c.store.EventLog.Append("notify.topic_rotated", "operator", fmt.Sprintf("ntfy topic rotated: %s -> %s", shortTopic(oldURL), shortTopic(newURL)))
	c.mu.Unlock()

	nCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	_ = c.notifier.PostNtfy(nCtx, oldURL, "Shepherd topic retired",
		"Shepherd no longer posts here. The new topic is on the Shepherd dashboard (Overview, bottom line).",
		[]string{"wastebasket"}, "high")
	_ = c.notifier.PostNtfy(nCtx, newURL, "Shepherd topic active",
		fmt.Sprintf("Notifications for %s now arrive on this topic.", c.cfg.Project.Name),
		[]string{"key", "shepherd"}, "default")

	if hook != nil {
		hook(newURL)
	}
	return newURL, nil
}

const topicAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomTopicToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random topic: %w", err)
	}
	for i, b := range buf {
		buf[i] = topicAlphabet[int(b)%len(topicAlphabet)]
	}
	return string(buf), nil
}

// ntfyTopicPrefix turns a project name into a readable topic prefix ("Demo App!" -> "demo-app").
func ntfyTopicPrefix(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	p := strings.Trim(b.String(), "-")
	if len(p) > 24 {
		p = strings.Trim(p[:24], "-")
	}
	if p == "" {
		p = "shepherd"
	}
	return p
}

// shortTopic keeps the event log from storing full topic names (they are the shared secret).
func shortTopic(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	t := strings.Trim(u.Path, "/")
	if i := strings.LastIndex(t, "/"); i >= 0 {
		t = t[i+1:]
	}
	if len(t) > 8 {
		t = t[:8] + "…"
	}
	return u.Host + "/" + t
}

// RegisterWorkOrder adds or updates an in-memory parsed work order and syncs it to the database.
func (c *Controller) RegisterWorkOrder(wo *workorder.WorkOrder) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.workOrders[wo.ID] = wo
	existing, _ := c.store.GetChunk(wo.ID)
	status := string(wo.Status)
	if existing != nil && existing.Status != "" {
		status = existing.Status
	}

	return c.store.UpsertChunk(db.ChunkRecord{
		ID:        wo.ID,
		Title:     wo.Title,
		Sprint:    wo.Sprint,
		Status:    status,
		Hash:      wo.Hash,
		CreatedAt: time.Now().UTC(),
	})
}

// Start begins processing the queue.
func (c *Controller) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.lockdown {
		return errors.New("cannot start: controller is in lockdown mode")
	}
	if c.state == StateRunning {
		return nil
	}

	c.state = StateRunning
	_, _ = c.store.EventLog.Append("daemon.started", "operator", "execution loop started")
	c.notifyEvent(ctx, notify.EventDaemonStarted, "Shepherd Started", fmt.Sprintf("Shepherd daemon started for %s (%s). Plan root: %s", c.cfg.Project.Name, c.projectRoot, c.planRoot))
	go c.runLoop()
	return nil
}

// StartPaused begins background processing in paused mode without executing chunks.
func (c *Controller) StartPaused(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.lockdown {
		return errors.New("cannot start: controller is in lockdown mode")
	}
	if c.state == StateRunning || c.state == StatePaused {
		return nil
	}

	c.state = StatePaused
	_, _ = c.store.EventLog.Append("daemon.started", "operator", "execution loop started in paused preflight mode")
	c.notifyEvent(ctx, notify.EventDaemonPaused, "Shepherd Started (Preflight Mode)", fmt.Sprintf("Shepherd daemon started in paused preflight mode for %s (%s). No chunks will execute until resumed.", c.cfg.Project.Name, c.projectRoot))
	go c.runLoop()
	return nil
}

// Pause pauses processing between or during chunks.
func (c *Controller) Pause(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state == StateLockdown {
		return errors.New("cannot pause: controller is in lockdown mode")
	}
	c.state = StatePaused
	_, _ = c.store.EventLog.Append("daemon.paused", "operator", "execution loop paused")
	c.notifyEvent(ctx, notify.EventDaemonPaused, "Shepherd Paused", "Queue execution paused by operator")
	return nil
}

// Resume unpauses the execution loop.
func (c *Controller) Resume(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.lockdown {
		return errors.New("cannot resume: controller is in lockdown mode")
	}
	if c.state != StatePaused {
		return nil
	}

	c.state = StateRunning
	_, _ = c.store.EventLog.Append("daemon.resumed", "operator", "execution loop resumed")
	c.notifyEvent(ctx, notify.EventDaemonResumed, "Shepherd Resumed", "Queue execution resumed")
	go c.runLoop()
	return nil
}

// Stop cleanly stops processing at the next chunk boundary.
func (c *Controller) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state == StateIdle {
		return nil
	}
	c.state = StateStopping
	_, _ = c.store.EventLog.Append("daemon.stopping", "operator", "clean stop requested")
	c.notifyEvent(ctx, notify.EventDaemonStopped, "Shepherd Stopping", "Clean stop requested at chunk boundary")
	return nil
}

// Halt immediately terminates every running chunk (all parallel slots), waits for their
// processes to exit, rolls back the worktree, and requeues them. Idempotent.
func (c *Controller) Halt(ctx context.Context, reason string) error {
	c.mu.Lock()
	if len(c.runs) == 0 {
		c.mu.Unlock()
		return nil // idempotent when no chunk is running
	}
	halted := make(map[string]*activeRun, len(c.runs))
	for id, r := range c.runs {
		halted[id] = r
		r.cancel()
	}
	c.mu.Unlock()

	// Wait for the agent processes to die so nothing writes after the rollback.
	deadline := time.After(haltWait)
	for _, r := range halted {
		select {
		case <-r.done:
		case <-deadline:
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.commitMu.Lock()
	defer c.commitMu.Unlock()

	// One chunk: restore its base commit. Parallel chunks share the worktree and siblings may
	// have committed passing work since, so discard only uncommitted (in-flight) changes.
	target := "HEAD"
	ids := make([]string, 0, len(halted))
	for id, r := range halted {
		ids = append(ids, id)
		if len(halted) == 1 && r.preCommit != "" {
			target = r.preCommit
		}
	}
	sort.Strings(ids)
	_, _ = c.git.Exec(context.Background(), "reset", "--hard", target)
	_, _ = c.git.Exec(context.Background(), "clean", "-fd", "-e", ".shepherd")

	for _, id := range ids {
		_ = c.store.SetChunkStatus(id, "pending", "halted: "+reason, "", "")
		c.clearHandoffNote(id)
		_, _ = c.store.EventLog.Append("chunk.halted", "operator", fmt.Sprintf("chunk %s halted: %s (rollback to %s)", id, reason, target))
	}
	c.notifyEvent(ctx, notify.EventDaemonHalted, "Shepherd Halted", fmt.Sprintf("Execution halted: %s (chunks %s rolled back)", reason, strings.Join(ids, ", ")))

	c.activeChunk = nil
	c.activeCancel = nil
	c.preChunkCommit = ""
	return nil
}

// Drain toggles drain mode.
func (c *Controller) Drain(ctx context.Context, enabled bool, until *time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.drain = enabled
	c.drainUntil = until
	if enabled && c.state == StateRunning {
		c.state = StateDraining
	} else if !enabled && c.state == StateDraining {
		c.state = StateRunning
	}
	_, _ = c.store.EventLog.Append("daemon.drain", "operator", fmt.Sprintf("enabled=%v", enabled))
	c.notifyEvent(ctx, notify.EventDrainMode, "Shepherd Drain Mode", fmt.Sprintf("Drain mode set to %v", enabled))
	return nil
}

// Lockdown engages lockdown mode, halting in-flight execution and rejecting runs.
func (c *Controller) Lockdown(ctx context.Context) error {
	c.mu.Lock()
	c.lockdown = true
	c.state = StateLockdown
	c.mu.Unlock()

	_ = c.Halt(ctx, "lockdown engaged")
	_ = c.FrontRunStop(ctx)

	_, _ = c.store.EventLog.Append("daemon.lockdown", "operator", "lockdown activated")
	c.notifyEvent(ctx, notify.EventDaemonLockdown, "Shepherd Lockdown", "Lockdown engaged: all executions stopped")
	return nil
}

// Unlock releases lockdown mode.
func (c *Controller) Unlock(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lockdown = false
	c.state = StateIdle
	_, _ = c.store.EventLog.Append("daemon.unlock", "operator", "lockdown released")
	c.notifyEvent(ctx, notify.EventDaemonResumed, "Shepherd Unlocked", "Lockdown released: controller returned to idle state")
	return nil
}

// Status returns current controller daemon status.
func (c *Controller) Status(ctx context.Context) (control.DaemonStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	chunks, err := c.store.ListChunks("")
	if err != nil {
		return control.DaemonStatus{}, err
	}

	var pending, running, blocked, done int
	for _, ch := range chunks {
		switch ch.Status {
		case "pending":
			pending++
		case "running":
			running++
		case "blocked":
			blocked++
		case "done":
			done++
		}
	}

	_ = c.applyOverridesLocked()
	overrides, _ := c.store.GetConfigOverrides()
	totalCost, totalTokens, _ := c.store.GetSprintCostUSD("")

	runningID := ""
	if c.activeChunk != nil {
		runningID = c.activeChunk.ID
	}

	activeTier, _ := c.resolveDefaultTier()
	var stack []string
	seen := make(map[string]bool)
	var orderedNames []string
	for _, step := range c.cfg.Repair.Chain {
		tName := strings.Split(step, ":")[0]
		if _, ok := c.cfg.Tiers[tName]; ok && !seen[tName] {
			seen[tName] = true
			orderedNames = append(orderedNames, tName)
		}
	}
	var restNames []string
	for k := range c.cfg.Tiers {
		if !seen[k] {
			restNames = append(restNames, k)
		}
	}
	sort.Strings(restNames)
	orderedNames = append(orderedNames, restNames...)

	for _, tName := range orderedNames {
		tc := c.cfg.Tiers[tName]
		desc := fmt.Sprintf("%s (%s", tName, tc.Adapter)
		if tc.Model != "" {
			desc += ":" + tc.Model
		}
		if tc.URL != "" {
			desc += " @ " + tc.URL
		}
		desc += ")"
		if !tc.IsEnabled() {
			desc += " [OFF]"
		}
		stack = append(stack, desc)
	}

	return control.DaemonStatus{
		State:          c.state,
		RunningChunkID: runningID,
		WorkerID:       "local-worker-1",
		ActiveTier:     activeTier,
		Stack:          stack,
		DrainMode:      c.drain,
		DrainUntil:     c.drainUntil,
		Lockdown:       c.lockdown,
		QueuePending:   pending,
		QueueRunning:   running,
		QueueBlocked:   blocked,
		QueueCompleted: done,
		TotalCostUSD:   totalCost,
		TotalTokens:    totalTokens,
		Overrides:      overrides,
		NtfyTopic:      c.NtfyURL(),
		FrontRun:       c.frontRunStatus(),
		Progress:       c.progressSnapshot(),
	}, nil
}

// ConfigGet returns the effective configuration overrides.
func (c *Controller) ConfigGet(ctx context.Context) (map[string]string, error) {
	return c.store.GetConfigOverrides()
}

// ConfigSet records a live config override to take effect immediately and at chunk boundaries.
func (c *Controller) ConfigSet(ctx context.Context, key, val string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.validateConfigKey(key, val); err != nil {
		return err
	}

	if err := c.store.SetConfigOverride(key, val, "operator"); err != nil {
		return err
	}

	_ = c.applyOverridesLocked()

	_, err := c.store.EventLog.Append("config.set", "operator", fmt.Sprintf("%s=%s", key, val))
	return err
}

func (c *Controller) validateConfigKey(key, val string) error {
	switch key {
	case "policy.max_consecutive_blocks", "policy.wait_for_reset_max_min":
		if _, err := strconv.Atoi(val); err != nil {
			return fmt.Errorf("value for %s must be an integer", key)
		}
	case "policy.cloud_first_threshold", "policy.max_chunk_cost_usd", "policy.max_sprint_cost_usd":
		if _, err := strconv.ParseFloat(val, 64); err != nil {
			return fmt.Errorf("value for %s must be a float", key)
		}
	case "policy.allow_unsandboxed", "policy.require_high_review_human":
		if _, err := strconv.ParseBool(val); err != nil {
			return fmt.Errorf("value for %s must be a boolean", key)
		}
	case "repair.chain":
		return nil
	default:
		if strings.HasPrefix(key, "quota.pools.") || strings.HasPrefix(key, "tiers.") {
			return nil
		}
		return fmt.Errorf("unknown or immutable config key: %q", key)
	}
	return nil
}

// ConfigReload re-applies overrides to the running configuration.
func (c *Controller) ConfigReload(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applyOverridesLocked()
}

func (c *Controller) applyOverridesLocked() error {
	overrides, err := c.store.GetConfigOverrides()
	if err != nil {
		return err
	}
	for k, v := range overrides {
		switch k {
		case "policy.max_consecutive_blocks":
			if n, err := strconv.Atoi(v); err == nil {
				c.cfg.Policy.MaxConsecutiveBlocks = n
			}
		case "policy.wait_for_reset_max_min":
			if n, err := strconv.Atoi(v); err == nil {
				c.cfg.Policy.WaitForResetMaxMin = n
			}
		case "policy.cloud_first_threshold":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				c.cfg.Policy.CloudFirstThreshold = f
			}
		case "policy.max_chunk_cost_usd":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				c.cfg.Policy.MaxChunkCostUSD = f
			}
		case "policy.max_sprint_cost_usd":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				c.cfg.Policy.MaxSprintCostUSD = f
			}
		case "policy.allow_unsandboxed":
			if b, err := strconv.ParseBool(v); err == nil {
				c.cfg.Policy.AllowUnsandboxed = b
			}
		case "policy.require_high_review_human":
			if b, err := strconv.ParseBool(v); err == nil {
				c.cfg.Policy.RequireHighReviewHuman = b
			}
		case "repair.chain":
			parts := strings.Split(v, ",")
			var newChain []string
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					newChain = append(newChain, p)
				}
			}
			if len(newChain) > 0 {
				c.cfg.Repair.Chain = newChain
			}
		default:
			if strings.HasPrefix(k, "tiers.") {
				rem := strings.TrimPrefix(k, "tiers.")
				parts := strings.SplitN(rem, ".", 2)
				if len(parts) == 2 && c.cfg.Tiers != nil {
					tName, field := parts[0], parts[1]
					if tc, ok := c.cfg.Tiers[tName]; ok {
						switch field {
						case "enabled":
							if b, err := strconv.ParseBool(v); err == nil {
								tc.Enabled = config.BoolPtr(b)
								c.cfg.Tiers[tName] = tc
							}
						case "model":
							tc.Model = v
							c.cfg.Tiers[tName] = tc
						case "url":
							tc.URL = v
							c.cfg.Tiers[tName] = tc
						case "priority":
							if n, err := strconv.Atoi(v); err == nil {
								tc.Priority = n
								c.cfg.Tiers[tName] = tc
							}
						case "usage_cap_pct":
							if n, err := strconv.Atoi(v); err == nil {
								tc.UsageCapPct = n
								c.cfg.Tiers[tName] = tc
							}
						}
					}
				}
			} else if strings.HasPrefix(k, "quota.pools.") {
				rem := strings.TrimPrefix(k, "quota.pools.")
				parts := strings.SplitN(rem, ".", 2)
				if len(parts) == 2 && c.cfg.Quota.Pools != nil {
					pName, field := parts[0], parts[1]
					if pc, ok := c.cfg.Quota.Pools[pName]; ok {
						switch field {
						case "floor_weekly_pct":
							if n, err := strconv.Atoi(v); err == nil {
								pc.FloorWeeklyPct = n
								c.cfg.Quota.Pools[pName] = pc
							}
						case "floor_five_hour_pct":
							if n, err := strconv.Atoi(v); err == nil {
								pc.FloorFiveHourPct = n
								c.cfg.Quota.Pools[pName] = pc
							}
						case "usage_cap_pct":
							if n, err := strconv.Atoi(v); err == nil {
								pc.UsageCapPct = n
								c.cfg.Quota.Pools[pName] = pc
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// Queue methods:
// Chunks come back in natural ID order. Queued chunks that cannot start yet carry a WaitingOn reason.
func (c *Controller) QueueList(ctx context.Context) ([]db.ChunkRecord, error) {
	chunks, err := c.store.ListChunks("")
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	annotateWaiting(chunks, c.workOrders)
	return chunks, nil
}

// annotateWaiting sets WaitingOn on each pending chunk the scheduler would currently skip, using the
// same rules as nextRunnableBatchLocked: every dependency must be done, and the chunk's files must
// not overlap a running chunk.
func annotateWaiting(chunks []db.ChunkRecord, orders map[string]*workorder.WorkOrder) {
	status := make(map[string]string, len(chunks))
	for _, ch := range chunks {
		status[ch.ID] = ch.Status
	}
	var running []string
	for _, ch := range chunks {
		if ch.Status == "running" {
			running = append(running, ch.ID)
		}
	}
	const maxListed = 3
	for i := range chunks {
		ch := &chunks[i]
		if ch.Status != "pending" {
			continue
		}
		wo, ok := orders[ch.ID]
		if !ok {
			continue
		}
		var parts []string
		for _, dep := range wo.DependsOn {
			st, known := status[dep]
			switch {
			case !known:
				parts = append(parts, dep+" (not in plan)")
			case st != "done":
				parts = append(parts, fmt.Sprintf("%s (%s)", dep, st))
			}
		}
		if len(parts) > maxListed {
			parts = append(parts[:maxListed], fmt.Sprintf("+%d more", len(parts)-maxListed))
		}
		if len(parts) > 0 {
			ch.WaitingOn = "waiting on " + strings.Join(parts, ", ")
			continue
		}
		for _, id := range running {
			if rw, ok := orders[id]; ok && hasFileOverlap(wo.Files, rw.Files) {
				ch.WaitingOn = "waiting for " + id + " to finish (shared files)"
				break
			}
		}
	}
}

// queueTarget loads a chunk for an operator action. Executing chunks are refused: changing a
// running chunk's status would let the scheduler start it a second time.
func (c *Controller) queueTarget(chunkID string) (*db.ChunkRecord, error) {
	rec, err := c.store.GetChunk(chunkID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("unknown chunk %q", chunkID)
		}
		return nil, err
	}
	if rec.Status == "running" {
		return nil, fmt.Errorf("chunk %s is running: halt it first", chunkID)
	}
	return rec, nil
}

func (c *Controller) setQueueStatus(chunkID, status, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.queueTarget(chunkID); err != nil {
		return err
	}
	return c.store.SetChunkStatus(chunkID, status, reason, "", "")
}

func (c *Controller) QueueSkip(ctx context.Context, chunkID string) error {
	c.clearHandoffNote(chunkID)
	return c.setQueueStatus(chunkID, "skipped", "skipped by operator")
}

// QueueRetry requeues a chunk (and clears why it was blocked). Its model assignment is kept.
func (c *Controller) QueueRetry(ctx context.Context, chunkID string) error {
	return c.setQueueStatus(chunkID, "pending", "")
}

func (c *Controller) QueueHold(ctx context.Context, chunkID string) error {
	return c.setQueueStatus(chunkID, "held", "held by operator")
}

func (c *Controller) QueueRelease(ctx context.Context, chunkID string) error {
	return c.setQueueStatus(chunkID, "pending", "")
}

// QueuePin assigns a tier to a chunk (see QueueAssign). The pool is derived from the tier.
func (c *Controller) QueuePin(ctx context.Context, chunkID, tier, pool string) error {
	return c.QueueAssign(ctx, chunkID, tier, "")
}

// QueueSetActive checks (pending) or unchecks (inactive) one queued chunk. An inactive chunk is
// never scheduled and is not a failure; other lifecycle states are not touched.
func (c *Controller) QueueSetActive(ctx context.Context, chunkID string, active bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, err := c.queueTarget(chunkID)
	if err != nil {
		return err
	}
	if active {
		switch rec.Status {
		case "pending":
			return nil
		case "inactive":
			if err := c.store.SetChunkStatus(chunkID, "pending", "", "", ""); err != nil {
				return err
			}
			_, _ = c.store.EventLog.Append("queue.activated", "operator", fmt.Sprintf("chunk %s re-activated", chunkID))
			return nil
		}
		return fmt.Errorf("chunk %s is %s, not inactive: use requeue/unblock instead", chunkID, rec.Status)
	}
	switch rec.Status {
	case "inactive":
		return nil
	case "pending":
		if err := c.store.SetChunkStatus(chunkID, "inactive", "deactivated by operator", "", ""); err != nil {
			return err
		}
		_, _ = c.store.EventLog.Append("queue.deactivated", "operator", fmt.Sprintf("chunk %s deactivated", chunkID))
		return nil
	}
	return fmt.Errorf("chunk %s is %s: only queued (pending) chunks can be deactivated", chunkID, rec.Status)
}

// QueueSetAllActive checks or unchecks every queued chunk at once and returns how many changed.
// Running, done, blocked, skipped and review-held chunks are never touched.
func (c *Controller) QueueSetAllActive(ctx context.Context, active bool) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	from, to, reason, event := "pending", "inactive", "deactivated by operator", "queue.deactivated_all"
	if active {
		from, to, reason, event = "inactive", "pending", "", "queue.activated_all"
	}
	n, err := c.store.ReplaceStatus(from, to, reason)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		_, _ = c.store.EventLog.Append(event, "operator", fmt.Sprintf("%d chunk(s) moved %s -> %s", n, from, to))
	}
	return n, nil
}

// modelNameRE keeps an operator-chosen model from being read as a CLI flag or smuggling anything
// into an adapter's argv: it must start with a letter or digit and use a small safe alphabet.
var modelNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

// QueueAssign routes one chunk to a specific tier (and optionally a specific model) regardless of
// its complexity. An empty tier returns the chunk to automatic routing.
func (c *Controller) QueueAssign(ctx context.Context, chunkID, tier, model string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.queueTarget(chunkID); err != nil {
		return err
	}
	if tier == "" {
		if model != "" {
			return errors.New("a model needs a tier")
		}
		if err := c.store.ClearChunkPin(chunkID); err != nil {
			return err
		}
		_, _ = c.store.EventLog.Append("queue.assigned", "operator", fmt.Sprintf("chunk %s returned to automatic routing", chunkID))
		return nil
	}
	tc, ok := c.cfg.Tiers[tier]
	if !ok {
		return fmt.Errorf("unknown tier %q", tier)
	}
	if !tc.IsEnabled() {
		return fmt.Errorf("tier %q is deactivated", tier)
	}
	if model != "" && !modelNameRE.MatchString(model) {
		return fmt.Errorf("invalid model name %q", model)
	}
	if err := c.store.SetChunkPin(chunkID, tier, model); err != nil {
		return err
	}
	shown := model
	if shown == "" {
		shown = "(tier default)"
	}
	_, _ = c.store.EventLog.Append("queue.assigned", "operator", fmt.Sprintf("chunk %s assigned tier=%s model=%s", chunkID, tier, shown))
	return nil
}

// QueueRefresh re-reads the plan tree from disk: new work orders are queued, edited ones are
// updated, and operator state (inactive, held, assignments, ...) is preserved.
func (c *Controller) QueueRefresh(ctx context.Context) (control.QueueRefreshResult, error) {
	var res control.QueueRefreshResult
	orders, bad, err := plan.Load(c.projectRoot, c.planRoot)
	if err != nil {
		return res, err
	}
	for _, wo := range orders {
		existing, _ := c.store.GetChunk(wo.ID)
		if err := c.RegisterWorkOrder(wo); err != nil {
			return res, err
		}
		switch {
		case existing == nil:
			res.Added++
		case existing.Hash != wo.Hash:
			res.Updated++
		}
	}
	res.Reconciled = len(c.ReconcileDone())
	res.Invalid = len(bad)
	if len(bad) > 0 {
		res.FirstError = fmt.Sprintf("%s: %s", bad[0].Chunk, bad[0].Message)
	}
	_, _ = c.store.EventLog.Append("queue.refreshed", "operator", fmt.Sprintf("plan reloaded from disk: %d added, %d updated, %d invalid, %d reconciled from done", res.Added, res.Updated, res.Invalid, res.Reconciled))
	return res, nil
}

// QueueDiff returns the pending diff for a held or active chunk.
func (c *Controller) QueueDiff(ctx context.Context, chunkID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if patch, err := c.store.GetHeldPatch(chunkID); err == nil && patch != "" {
		return patch, nil
	}
	// Fallback to live worktree diff if available
	diffOut, err := c.git.Exec(ctx, "diff", "HEAD")
	if err != nil {
		return "", err
	}
	return diffOut, nil
}

// QueueApprove applies a held patch (if stored), commits the changes, and marks the chunk done.
func (c *Controller) QueueApprove(ctx context.Context, chunkID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	wo, exists := c.workOrders[chunkID]
	title := chunkID
	if exists && wo.Title != "" {
		title = wo.Title
	}

	c.commitMu.Lock()
	defer c.commitMu.Unlock()

	if patch, err := c.store.GetHeldPatch(chunkID); err == nil && patch != "" {
		statusOut, _ := c.git.Exec(ctx, "status", "--porcelain")
		if strings.TrimSpace(statusOut) == "" {
			tmpPatch := filepath.Join(c.projectRoot, ".shepherd", fmt.Sprintf("approve-%s.diff", chunkID))
			_ = os.WriteFile(tmpPatch, []byte(patch), 0600)
			_, applyErr := c.git.Exec(ctx, "apply", tmpPatch)
			_ = os.Remove(tmpPatch)
			if applyErr != nil {
				return fmt.Errorf("apply held patch: %w", applyErr)
			}
		}
		_ = c.store.DeleteHeldPatch(chunkID)
	}

	c.stageChunkFiles(ctx, wo)
	_, _ = c.git.Exec(ctx, "commit", "-m", fmt.Sprintf("feat(%s): %s\n\nApproved-By: operator\nCo-Authored-By: ShepherdAI <shepherd@local>", chunkID, title))

	if exists && wo != nil {
		c.markSourceDone(wo)
	}
	_ = c.store.SetChunkStatus(chunkID, "done", "approved by operator", "operator", "")
	_, _ = c.store.EventLog.Append("chunk.approved", "operator", fmt.Sprintf("chunk %s approved and committed by operator", chunkID))
	return nil
}

// QueueReject discards a held chunk's pending patch/changes and marks it blocked.
func (c *Controller) QueueReject(ctx context.Context, chunkID, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if reason == "" {
		reason = "rejected by operator"
	}

	_ = c.store.DeleteHeldPatch(chunkID)
	_, _ = c.git.Exec(ctx, "reset", "--hard", "HEAD")
	_, _ = c.git.Exec(ctx, "clean", "-fd", "-e", ".shepherd")

	_ = c.store.SetChunkStatus(chunkID, "blocked", reason, "operator", "")
	_, _ = c.store.EventLog.Append("chunk.rejected", "operator", fmt.Sprintf("chunk %s rejected by operator: %s", chunkID, redact.Sanitize(reason)))
	return nil
}

// Quota methods:
func (c *Controller) QuotaGet(ctx context.Context) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.applyOverridesLocked()
	return map[string]any{
		"pools":  c.cfg.Quota.Pools,
		"tiers":  c.cfg.Tiers,
		"chain":  c.cfg.Repair.Chain,
		"policy": c.cfg.Policy,
		"drain":  c.drain,
	}, nil
}

func (c *Controller) QuotaSetFloor(ctx context.Context, pool string, pct int) error {
	return c.ConfigSet(ctx, fmt.Sprintf("quota.pools.%s.floor_weekly_pct", pool), strconv.Itoa(pct))
}

func (c *Controller) QuotaSetReserve(ctx context.Context, pool string, pct int) error {
	return c.ConfigSet(ctx, fmt.Sprintf("quota.pools.%s.floor_five_hour_pct", pool), strconv.Itoa(pct))
}

func (c *Controller) QuotaTierToggle(ctx context.Context, tier string, enabled bool) error {
	return c.ConfigSet(ctx, fmt.Sprintf("tiers.%s.enabled", tier), strconv.FormatBool(enabled))
}

func (c *Controller) QuotaResetBucket(ctx context.Context, pool string) error {
	_, err := c.store.EventLog.Append("quota.reset", "operator", fmt.Sprintf("reset bucket %s", pool))
	return err
}

// Audit retrieves the latest hash-chained audit events.
func (c *Controller) Audit(ctx context.Context, limit int) ([]eventlog.Event, error) {
	events, err := c.store.EventLog.Since(0)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

// ValidateToken verifies bearer tokens using constant-time comparison and never matches empty env vars.
func (c *Controller) ValidateToken(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("unauthorized: empty token")
	}

	matchToken := func(candidate string) bool {
		if candidate == "" {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(token), []byte(candidate)) == 1
	}

	adminEnv := os.Getenv("SHEPHERD_ADMIN_TOKEN")
	opEnv := os.Getenv("SHEPHERD_OPERATOR_TOKEN")
	viewEnv := os.Getenv("SHEPHERD_VIEWER_TOKEN")
	anyEnvConfigured := adminEnv != "" || opEnv != "" || viewEnv != ""

	if matchToken(adminEnv) || (!anyEnvConfigured && matchToken("admin")) {
		return control.RoleAdmin, nil
	}
	if matchToken(opEnv) || (!anyEnvConfigured && matchToken("operator")) {
		return control.RoleOperator, nil
	}
	if matchToken(viewEnv) || (!anyEnvConfigured && matchToken("viewer")) {
		return control.RoleViewer, nil
	}
	return "", errors.New("unauthorized token")
}

// resolveDefaultTier picks the highest-priority enabled tier from repair.chain or tiers without assuming "primary" exists.
// hasFallbackTier reports whether any enabled tier other than the given one exists.
func (c *Controller) hasFallbackTier(current string) bool {
	for name, tc := range c.cfg.Tiers {
		if name != current && tc.IsEnabled() {
			return true
		}
	}
	return false
}

// isProviderUnavailableErr heuristically detects provider/network outages and quota
// rejections (as opposed to the model producing a bad result).
func isProviderUnavailableErr(msg string) bool {
	l := strings.ToLower(msg)
	for _, k := range []string{
		"connection refused", "no such host", "dial tcp", "i/o timeout", "timeout", "unreachable",
		"service unavailable", "bad gateway", "gateway timeout", "http 502", "http 503", "http 504",
		"status 502", "status 503", "status 504", "429", "rate limit", "quota", "overloaded",
		"unauthorized", "invalid api key", "model not found", "econnrefused", "connection reset",
	} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// isLocalTierCfg reports whether a tier runs on this machine/LAN. A local-capable adapter
// (opencode, ollama, ...) pointed at a remote HTTPS/API endpoint (e.g. Raon) is NOT local:
// it is a hosted provider and belongs to the strong/cloud stack.
func isLocalTierCfg(name string, tc config.TierConfig) bool {
	if name == "local" {
		return true
	}
	if !isLocalAdapter(tc.Adapter) {
		return false
	}
	if tc.URL == "" {
		return true
	}
	u, err := url.Parse(tc.URL)
	if err != nil || u.Hostname() == "" {
		return true
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".lan") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // public DNS name => hosted API
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// tierIsLocal resolves a tier by name from config and applies isLocalTierCfg, falling back
// to the adapter name when the tier is unknown.
func (c *Controller) tierIsLocal(tierName, adapterName string) bool {
	if tc, ok := c.cfg.Tiers[tierName]; ok {
		return isLocalTierCfg(tierName, tc)
	}
	return tierName == "local" || isLocalAdapter(adapterName)
}

func isLocalAdapter(name string) bool {
	l := strings.ToLower(name)
	return strings.Contains(l, "opencode") || strings.Contains(l, "ollama") || strings.Contains(l, "vllm") || strings.Contains(l, "local")
}

const LocalContextGuardrails = "\n\n## Local Execution Guardrails (Strict Context Conservation)\n" +
	"- NEVER run recursive or broad discovery commands (e.g. `ls -R`, `find .`, `grep -r .`). Inspect only specific targeted directories or files.\n" +
	"- NEVER read entire large files (>200 lines). Use grep to find exact line locations or read with specific offsets/limits.\n" +
	"- Stay strictly within declared `## Files`. Do not inspect unrelated modules or directories.\n" +
	"- Make targeted, precise edits directly without repetitive read loops.\n"

func (c *Controller) resolveStrongTier() (string, config.TierConfig) {
	if cfg, ok := c.cfg.Tiers["strong"]; ok && cfg.IsEnabled() {
		return "strong", cfg
	}
	if cfg, ok := c.cfg.Tiers["pro"]; ok && cfg.IsEnabled() {
		return "pro", cfg
	}
	// Look through repair chain for the first non-local tier
	for _, step := range c.cfg.Repair.Chain {
		tierName := strings.Split(step, ":")[0]
		if cfg, ok := c.cfg.Tiers[tierName]; ok && cfg.IsEnabled() {
			if !isLocalTierCfg(tierName, cfg) {
				return tierName, cfg
			}
		}
	}
	// Fall back to any enabled non-local tier
	for k, tc := range c.cfg.Tiers {
		if tc.IsEnabled() && !isLocalTierCfg(k, tc) {
			return k, tc
		}
	}
	if cfg, ok := c.cfg.Tiers["primary"]; ok && cfg.IsEnabled() {
		return "primary", cfg
	}
	return "strong", config.TierConfig{Adapter: "agy"}
}

func (c *Controller) resolveDefaultTier() (string, config.TierConfig) {
	for _, step := range c.cfg.Repair.Chain {
		tierName := strings.Split(step, ":")[0]
		if cfg, ok := c.cfg.Tiers[tierName]; ok && cfg.IsEnabled() {
			return tierName, cfg
		}
	}
	if cfg, ok := c.cfg.Tiers["primary"]; ok && cfg.IsEnabled() {
		return "primary", cfg
	}
	names := make([]string, 0, len(c.cfg.Tiers))
	for k, tc := range c.cfg.Tiers {
		if tc.IsEnabled() {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0], c.cfg.Tiers[names[0]]
	}
	return "primary", config.TierConfig{Adapter: "agy"}
}

func (c *Controller) resolveLocalTier() (string, config.TierConfig) {
	if cfg, ok := c.cfg.Tiers["local"]; ok && cfg.IsEnabled() {
		return "local", cfg
	}
	for k, tc := range c.cfg.Tiers {
		if tc.IsEnabled() && isLocalTierCfg(k, tc) {
			return k, tc
		}
	}
	if cfg, ok := c.cfg.Tiers["local"]; ok {
		return "local", cfg
	}
	return "local", config.TierConfig{
		Adapter: "opencode",
		Pool:    "local",
		URL:     "http://127.0.0.1:8080/v1",
	}
}

// isAnyCloudAvailable checks whether any configured cloud quota pool has available headroom.
// When all cloud pools are exhausted (remaining <= floor or 0%), or when noCloud is enabled,
// it returns false so the controller falls back to the local tier as a stopgap.
func (c *Controller) isAnyCloudAvailable(ctx context.Context) (bool, string) {
	if c.noCloud {
		return false, "nocloud mode active"
	}

	hasCloudPool := false
	allExhausted := true

	for pName, pc := range c.cfg.Quota.Pools {
		if pName == "local" {
			continue
		}
		hasCloudPool = true
		if pc.Mode == "polled" && len(pc.UsageCmd) > 0 {
			buckets, err := quota.PollPoolCached(ctx, pName, pc, 15*time.Second)
			if err == nil && len(buckets) > 0 {
				for _, b := range buckets {
					floor := pc.FloorWeeklyPct
					if floor <= 0 {
						floor = 20
					}
					if b.RemainingPct > floor && b.RemainingPct > 0 {
						allExhausted = false
						return true, fmt.Sprintf("pool %s has %d%% capacity (> floor %d%%)", pName, b.RemainingPct, floor)
					}
				}
			}
		} else if pc.Mode == "until-rejected" {
			allExhausted = false
			return true, fmt.Sprintf("pool %s is until-rejected (active)", pName)
		}
	}

	if !hasCloudPool {
		return true, "no cloud pools configured"
	}
	if allExhausted {
		return false, "all cloud quota pools exhausted (remaining <= floor)"
	}
	return true, "cloud available"
}

func (c *Controller) checkAndRecoverCloudPools(ctx context.Context) bool {
	if c.noCloud {
		return false
	}
	recovered := false
	for pName, pc := range c.cfg.Quota.Pools {
		if pName == "local" || pc.Mode != "polled" || len(pc.UsageCmd) == 0 {
			continue
		}
		// Force poll by passing ttl = 0
		buckets, err := quota.PollPoolCached(ctx, pName, pc, 0)
		if err == nil && len(buckets) > 0 {
			for _, b := range buckets {
				floor := pc.FloorWeeklyPct
				if floor <= 0 {
					floor = 20
				}
				if b.RemainingPct > floor && b.RemainingPct > 0 {
					recovered = true
					msg := fmt.Sprintf("cloud pool %q refreshed (%d%% remaining > floor %d%%); exiting stopgap mode, reinstituting timeouts for local tasks", pName, b.RemainingPct, floor)
					_, _ = c.store.EventLog.Append("quota.recovered", "quota", msg)
					c.notifyEvent(ctx, notify.EventModelSwitch, "Cloud Quota Refreshed", msg)
					break
				}
			}
		}
	}
	if recovered {
		c.mu.Lock()
		c.inStopgap = false
		c.mu.Unlock()
	}
	return recovered
}

// stageChunkFiles stages either the chunk's declared files or all non-.shepherd files.
func (c *Controller) stageChunkFiles(ctx context.Context, wo *workorder.WorkOrder) {
	_, _ = c.git.Exec(ctx, "add", "-A", "--", ":!.shepherd")
}

// markSourceDone updates the source markdown / workorder file to mark the chunk done.
func (c *Controller) markSourceDone(wo *workorder.WorkOrder) {
	if wo == nil || wo.Path == "" {
		return
	}

	// Resolve the absolute path of the work order file
	absPath := wo.Path
	if !filepath.IsAbs(absPath) {
		candidates := []string{
			filepath.Join(c.projectRoot, filepath.FromSlash(wo.Path)),
			filepath.Join(c.projectRoot, filepath.FromSlash(c.planRoot), filepath.FromSlash(wo.Path)),
			filepath.Join(filepath.FromSlash(c.planRoot), filepath.FromSlash(wo.Path)),
		}
		found := false
		for _, cand := range candidates {
			if _, err := os.Stat(cand); err == nil {
				absPath = cand
				found = true
				break
			}
		}
		if !found {
			absPath = candidates[0]
		}
	}
	if _, err := os.Stat(absPath); err != nil {
		return
	}

	_ = source.MarkDone(absPath, wo.ID)

	doneDir := c.cfg.Queue.DoneDir
	if doneDir == "" && (c.cfg.Queue.DoneMarker == "" || c.cfg.Queue.DoneMarker == "move-to-done") {
		doneDir = c.planRoot + "DONE"
	}

	// Check if <planRoot>DONE, docs/planDONE, or planDONE exists on disk
	for _, cand := range []string{c.planRoot + "DONE", "docs/planDONE", "planDONE"} {
		if _, err := os.Stat(filepath.Join(c.projectRoot, cand)); err == nil {
			doneDir = cand
			break
		}
	}

	// If done_dir is configured (or defaulted for move-to-done), move the completed work order file
	if doneDir != "" {
		absDoneDir := doneDir
		if !filepath.IsAbs(absDoneDir) {
			absDoneDir = filepath.Join(c.projectRoot, filepath.FromSlash(doneDir))
		}
		if err := os.MkdirAll(absDoneDir, 0755); err == nil {
			destFile := filepath.Join(absDoneDir, filepath.Base(absPath))
			if err := os.Rename(absPath, destFile); err == nil {
				_, _ = c.git.Exec(context.Background(), "rm", "-f", "--", wo.Path)
				if relDest, rErr := filepath.Rel(c.projectRoot, destFile); rErr == nil {
					wo.Path = filepath.ToSlash(relDest)
					_, _ = c.git.Exec(context.Background(), "add", "--", filepath.ToSlash(relDest))
				}
				return
			}
		}
	}

	_, _ = c.git.Exec(context.Background(), "add", "--", wo.Path)
}

// runLoop executes chunks until the queue is empty, stopped, paused, or blocked.
func (c *Controller) runLoop() {
	for {
		c.mu.Lock()
		if c.state == StateStopping {
			c.state = StateIdle
			c.mu.Unlock()
			return
		}
		if c.state == StatePaused || c.state == StateLockdown || c.state == StateIdle {
			c.mu.Unlock()
			return
		}

		_ = c.applyOverridesLocked()
		c.ReconcileDone()      // work finished outside Shepherd must unblock its dependants
		c.landFrontRunLocked() // nothing of ours is running here: the safe moment to merge the front-run lane's work

		// Check sprint-wide cost circuit breaker before scheduling next batch
		if c.cfg.Policy.MaxSprintCostUSD > 0 {
			sprintCost, _, _ := c.store.GetSprintCostUSD("")
			if sprintCost >= c.cfg.Policy.MaxSprintCostUSD {
				c.state = StatePaused
				msg := fmt.Sprintf("sprint cost circuit breaker tripped: $%.4f >= budget $%.4f", sprintCost, c.cfg.Policy.MaxSprintCostUSD)
				_, _ = c.store.EventLog.Append("daemon.paused", "cost_guard", msg)
				c.mu.Unlock()
				c.notifyEvent(context.Background(), notify.EventQueueBlocked, "Sprint Budget Exceeded", msg)
				return
			}
		}

		maxBatch := 1
		if c.cfg.Slots.MaxSlots > 1 && c.slotsPool != nil {
			maxBatch = c.cfg.Slots.MaxSlots
		}

		batch := c.nextRunnableBatchLocked(maxBatch)
		if len(batch) == 0 {
			c.state = StateIdle
			pending, _ := c.store.ListChunks("pending")
			running, _ := c.store.ListChunks("running")
			blocked, _ := c.store.ListChunks("blocked")
			held, _ := c.store.ListChunks("held")
			inactive, _ := c.store.ListChunks("inactive")
			done, _ := c.store.ListChunks("done")
			stallMsg := ""
			if len(pending) > 0 {
				if msg := c.describeStallLocked(pending); msg != c.lastStall {
					c.lastStall = msg
					stallMsg = msg
					_, _ = c.store.EventLog.Append("queue.stalled", "scheduler", msg)
				}
			}
			c.mu.Unlock()

			if stallMsg != "" {
				c.notifyEvent(context.Background(), notify.EventQueueStalled, "Queue Stalled", stallMsg)
			}
			if len(pending) == 0 && len(running) == 0 && len(done) > 0 && len(blocked) == 0 && len(held) == 0 && len(inactive) == 0 {
				c.notifyEvent(context.Background(), notify.EventSprintDone, "Sprint Completed", fmt.Sprintf("All %d planned work orders completed successfully!", len(done)))
			}
			return
		}

		c.activeChunk = batch[0]
		c.lastStall = ""
		c.mu.Unlock()

		if len(batch) == 1 {
			err := c.executeChunk(batch[0])
			c.mu.Lock()
			c.activeChunk = nil
			c.activeCancel = nil
			c.preChunkCommit = ""

			if errors.Is(err, context.Canceled) {
				// halted by the operator: not a block
			} else if err != nil {
				c.consecutiveBlocks++
				if c.cfg.Policy.MaxConsecutiveBlocks > 0 && c.consecutiveBlocks >= c.cfg.Policy.MaxConsecutiveBlocks {
					c.state = StatePaused
					_, _ = c.store.EventLog.Append("daemon.paused", "system", fmt.Sprintf("paused: %d consecutive blocks", c.consecutiveBlocks))
					c.mu.Unlock()
					return
				}
			} else {
				c.consecutiveBlocks = 0
			}
			c.mu.Unlock()
		} else {
			var wg sync.WaitGroup
			errs := make([]error, len(batch))
			for idx, wo := range batch {
				wg.Add(1)
				go func(i int, w *workorder.WorkOrder) {
					defer wg.Done()
					errs[i] = c.executeChunk(w)
				}(idx, wo)
			}
			wg.Wait()

			c.mu.Lock()
			c.activeChunk = nil
			c.activeCancel = nil
			c.preChunkCommit = ""
			for _, err := range errs {
				if errors.Is(err, context.Canceled) {
					continue // halted by the operator: not a block
				}
				if err != nil {
					c.consecutiveBlocks++
				} else {
					c.consecutiveBlocks = 0
				}
			}
			if c.cfg.Policy.MaxConsecutiveBlocks > 0 && c.consecutiveBlocks >= c.cfg.Policy.MaxConsecutiveBlocks {
				c.state = StatePaused
				_, _ = c.store.EventLog.Append("daemon.paused", "system", fmt.Sprintf("paused: %d consecutive blocks", c.consecutiveBlocks))
				c.mu.Unlock()
				return
			}
			c.mu.Unlock()
		}
	}
}

func (c *Controller) nextRunnableChunkLocked() *workorder.WorkOrder {
	batch := c.nextRunnableBatchLocked(1)
	if len(batch) == 0 {
		return nil
	}
	return batch[0]
}

func (c *Controller) nextRunnableBatchLocked(limit int) []*workorder.WorkOrder {
	if limit <= 0 {
		limit = 1
	}
	chunks, err := c.store.ListChunks("pending")
	if err != nil || len(chunks) == 0 {
		return nil
	}

	doneChunks := make(map[string]bool)
	all, _ := c.store.ListChunks("done")
	for _, d := range all {
		doneChunks[d.ID] = true
	}

	runningChunks, _ := c.store.ListChunks("running")
	var activeFileSets [][]workorder.File
	for _, rc := range runningChunks {
		if rcWO, ok := c.workOrders[rc.ID]; ok {
			activeFileSets = append(activeFileSets, rcWO.Files)
		}
	}

	var selected []*workorder.WorkOrder
	for _, ch := range chunks {
		wo, exists := c.workOrders[ch.ID]
		if !exists {
			continue
		}

		// Check dependencies
		depsSatisfied := true
		for _, dep := range wo.DependsOn {
			if !doneChunks[dep] {
				depsSatisfied = false
				break
			}
		}
		if !depsSatisfied {
			continue
		}

		// Check file overlap with running chunks or already-selected batch chunks (brief §4: Overlap means serialize)
		overlap := false
		for _, fs := range activeFileSets {
			if hasFileOverlap(wo.Files, fs) {
				overlap = true
				break
			}
		}
		if overlap {
			continue
		}

		// If Files list is empty on either chunk and we already selected one chunk, do not parallelize unknown scopes
		if len(selected) > 0 && len(wo.Files) == 0 {
			continue
		}

		selected = append(selected, wo)
		activeFileSets = append(activeFileSets, wo.Files)
		if len(selected) >= limit {
			break
		}
	}
	return selected
}

func hasFileOverlap(a, b []workorder.File) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	set := make(map[string]bool, len(a))
	for _, f := range a {
		set[filepath.Clean(f.Path)] = true
	}
	for _, f := range b {
		if set[filepath.Clean(f.Path)] {
			return true
		}
	}
	return false
}

// isSubChunk reports whether chunkID is an intermediate sub-chunk (e.g. 45A.1 or 45A.2)
// with at least one other unfinished chunk in the same group (45A.*).
// When the final remaining chunk of a group runs (e.g. 45A.3 after 45A.1 and 45A.2 are done),
// or for unsplit chunks (e.g. 24L), isSubChunk returns false so boundary gates run the full suite.
func (c *Controller) isSubChunk(chunkID string) bool {
	group := workorder.ChunkGroup(chunkID)
	if group == "" || group == chunkID {
		return false
	}
	chunks, err := c.store.ListChunks("")
	if err != nil {
		return false
	}
	for _, ch := range chunks {
		if ch.ID == chunkID {
			continue
		}
		if workorder.ChunkGroup(ch.ID) != group {
			continue
		}
		if ch.Status != "done" && ch.Status != "skipped" {
			return true
		}
	}
	return false
}

func (c *Controller) executeChunk(wo *workorder.WorkOrder) error {
	ctx, cancel := context.WithCancel(context.Background())
	run := &activeRun{cancel: cancel, done: make(chan struct{})}
	c.mu.Lock()
	if c.state == StateLockdown {
		c.mu.Unlock()
		cancel()
		return context.Canceled
	}
	// The operator may have unchecked/skipped/held the chunk after the scheduler picked it.
	if rec, err := c.store.GetChunk(wo.ID); err == nil {
		switch rec.Status {
		case "inactive", "skipped", "held", "done":
			c.mu.Unlock()
			cancel()
			return context.Canceled
		}
	}
	c.activeCancel = cancel
	c.runs[wo.ID] = run
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		delete(c.runs, wo.ID)
		c.mu.Unlock()
		close(run.done)
	}()

	// 1. Record pre-chunk commit SHA
	preCommit, err := c.git.Exec(ctx, "rev-parse", "HEAD")
	if err != nil {
		preCommit = "0000000000000000000000000000000000000000"
	}
	preCommit = strings.TrimSpace(preCommit)

	c.mu.Lock()
	c.preChunkCommit = preCommit
	run.preCommit = preCommit
	c.mu.Unlock()

	// Check cloud availability and decide routing
	cloudAvail, cloudReason := c.isAnyCloudAvailable(ctx)
	isStopgap := !cloudAvail && !c.noCloud

	c.mu.Lock()
	c.inStopgap = isStopgap
	c.mu.Unlock()

	var tierName string
	var tierCfg config.TierConfig

	if c.noCloud || isStopgap {
		// Fall back to local regardless of chunk complexity demarkation
		tierName, tierCfg = c.resolveLocalTier()
		if isStopgap {
			_, _ = c.store.EventLog.Append("chunk.stopgap", "controller", fmt.Sprintf("chunk %s routed to local stopgap (%s); complexity demarkation bypassed, timeouts disabled", wo.ID, cloudReason))
		} else if c.noCloud {
			_, _ = c.store.EventLog.Append("chunk.nocloud", "controller", fmt.Sprintf("chunk %s running local (--nocloud; timeouts disabled)", wo.ID))
		}
	} else {
		tierName, tierCfg = c.resolveDefaultTier()
	}

	// An operator's model assignment overrides complexity routing (but not --nocloud / local stopgap).
	assigned := false
	if name, tc, ok := c.applyAssignment(wo.ID, c.noCloud || isStopgap); ok {
		tierName, tierCfg, assigned = name, tc, true
		_, _ = c.store.EventLog.Append("chunk.route", "operator", fmt.Sprintf("chunk %s routed to %s [model: %s] by operator assignment, complexity routing bypassed", wo.ID, name, tc.Model))
	}

	_ = c.store.SetChunkStatus(wo.ID, "running", "", "local-worker-1", tierName)
	_, _ = c.store.EventLog.Append("chunk.started", "worker", fmt.Sprintf("chunk %s started (base commit %s)", wo.ID, preCommit))
	c.progBegin(wo.ID, "main")
	defer c.progEnd(wo.ID)
	go c.progWatch(ctx, wo.ID, c.git)

	// 2. Resolve adapter & model
	adapterName := "agy"
	if tierCfg.Adapter != "" {
		adapterName = tierCfg.Adapter
	}
	ad, hasAdapter := c.adapters[adapterName]
	if !hasAdapter {
		if isLocalAdapter(adapterName) {
			ad = adapter.NewOpenCodeAdapter()
		} else {
			ad = adapter.NewAgyAdapter()
		}
	}

	// Only apply cloud risk & effort routing if cloud is available and not in stopgap/noCloud
	if !c.noCloud && !isStopgap && !assigned {
		useStrong, routeReason := predict.Route(wo, c.predictModel, c.cfg.Policy.CloudFirstThreshold)
		if useStrong {
			sTierName, sTierCfg := c.resolveStrongTier()
			if sTierName != "" && sTierCfg.Adapter != "" {
				if strongAd, ok := c.adapters[sTierCfg.Adapter]; ok {
					ad = strongAd
					tierName = sTierName
					tierCfg = sTierCfg
					adapterName = sTierCfg.Adapter
					_, _ = c.store.EventLog.Append("chunk.route", "predictor", fmt.Sprintf("chunk %s routed to %s tier: %s", wo.ID, sTierName, routeReason))
				}
			}
		}
	}

	if !c.noCloud && !isStopgap && !assigned && c.tierLimited(tierName) {
		if fn, fc := c.fallbackTier(tierName); fn != "" {
			_, _ = c.store.EventLog.Append("chunk.route", "quota", fmt.Sprintf("chunk %s: tier %s is at its provider limit, using %s", wo.ID, tierName, fn))
			tierName, tierCfg, adapterName = fn, fc, fc.Adapter
			ad = c.adapterFor(adapterName)
		}
	}

	modelName := tierCfg.Model
	if modelName == "" {
		modelName = "default"
	}
	c.notifyEvent(ctx, notify.EventChunkStarted, fmt.Sprintf("Chunk Started: %s", wo.ID), fmt.Sprintf("Started chunk %s (\"%s\") on tier '%s' [model: %s, adapter: %s]", wo.ID, wo.Title, tierName, modelName, adapterName))

	// Hardware headroom safeguard (Improvement 1: RAM safety floor & GPU concurrency limit)
	isLocalGPU := c.tierIsLocal(tierName, adapterName)
	if c.slotsPool != nil && c.slotsPool.Guard != nil {
		if err := c.slotsPool.Guard.CheckHeadroom(isLocalGPU, 0); err != nil {
			_ = c.store.SetChunkStatus(wo.ID, "held", fmt.Sprintf("hardware headroom floor: %v", err), "local-worker-1", tierName)
			_, _ = c.store.EventLog.Append("chunk.held", "hardware", fmt.Sprintf("chunk %s held due to hardware constraint: %v", wo.ID, err))
			c.notifyEvent(ctx, notify.EventChunkHeld, "Chunk Held (Hardware Constraint): "+wo.ID, err.Error())
			return nil
		}
	}

	// 3. Prepare sandbox profile
	socketDir := filepath.Dir(control.DefaultSocketPath())
	stateDir := filepath.Join(c.projectRoot, ".shepherd")
	prof := sandbox.Profile{
		WorktreeDir:         c.projectRoot,
		AdapterDirs:         ad.Capabilities().Paths,
		ReadOnlyAdapterDirs: ad.Capabilities().ReadOnlyPaths,
		MaskDirs:            append(c.maskDirsFor(stateDir), socketDir),
		AllowUnsandboxed:    c.cfg.Policy.AllowUnsandboxed,
		AllowNetwork:        ad.Capabilities().RequiresNetwork,
		Backend:             c.cfg.Policy.SandboxBackend,
		ContainerImage:      c.cfg.Policy.ContainerImage,
	}

	// 4. Run agent (Enriched with AST repo map & golden example context)
	prompt := repomap.EnrichPrompt(wo, c.projectRoot)
	if strings.TrimSpace(prompt) == "" {
		prompt = fmt.Sprintf("Implement chunk %s: %s", wo.ID, wo.Title)
	}
	if c.tierIsLocal(tierName, adapterName) || c.noCloud {
		prompt += LocalContextGuardrails
	}
	if c.workerNoteOn() {
		c.clearHandoffNote(wo.ID) // fresh start at attempt 1
		prompt += c.handoffInstructions(wo.ID)
	}

	var chunkTimeout time.Duration
	if c.tierIsLocal(tierName, adapterName) {
		if isStopgap || c.noCloud {
			// Timeouts for local are switched off
			chunkTimeout = 0
		} else {
			// Normal timeout for local tasks
			chunkTimeout = 10 * time.Minute
		}
	}

	runOpts := adapter.RunOptions{
		WorktreeRoot: c.projectRoot,
		Model:        tierCfg.Model,
		Effort:       string(wo.Effort),
		Timeout:      chunkTimeout,
		Sandbox:      &prof,
		OnOutput:     c.progOutput(wo.ID),
	}

	c.progPhase(wo.ID, phaseAgent, tierName+" · "+modelName, 1)
	runRes, err := ad.Run(ctx, prompt, runOpts)
	c.recordAndCheckCost(wo, tierName, 1, runRes)
	// A provider limit is no fault of the chunk: move to another tier and run again.
	for i := 0; i < len(c.cfg.Tiers) && ctx.Err() == nil; i++ {
		errText := runRes.Output
		if err != nil {
			errText += " " + err.Error()
		}
		if !c.noteLimit(wo.ID, tierName, errText) {
			break
		}
		fn, fc := c.fallbackTier(tierName)
		if fn == "" {
			break
		}
		_, _ = c.store.EventLog.Append("chunk.route", "quota", fmt.Sprintf("chunk %s: tier %s hit its provider limit, retrying on %s", wo.ID, tierName, fn))
		tierName, tierCfg, adapterName = fn, fc, fc.Adapter
		ad = c.adapterFor(adapterName)
		modelName = tierCfg.Model
		if modelName == "" {
			modelName = "default"
		}
		runOpts.Model = tierCfg.Model
		c.progPhase(wo.ID, phaseAgent, tierName+" · "+modelName, 1)
		runRes, err = ad.Run(ctx, prompt, runOpts)
		c.recordAndCheckCost(wo, tierName, 1, runRes)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cleanErr := redact.Sanitize(err.Error())
		if isProviderUnavailableErr(err.Error()+" "+runRes.Output) && !c.hasFallbackTier(tierName) {
			// Model/provider is down and nothing else can take over: halt instead of blocking the chunk.
			_ = c.store.SetChunkStatus(wo.ID, "pending", "", "", "")
			c.mu.Lock()
			if c.state == StateRunning {
				c.state = StatePaused
			}
			c.mu.Unlock()
			msg := fmt.Sprintf("tier %q unavailable and no fallback tiers enabled; halted (chunk %s left pending): %s", tierName, wo.ID, cleanErr)
			_, _ = c.store.EventLog.Append("daemon.paused", "provider_guard", msg)
			c.notifyEvent(ctx, notify.EventDaemonPaused, "Shepherd Halted: Provider Unavailable", msg)
			return nil
		}
		_ = c.store.SetChunkStatus(wo.ID, "blocked", fmt.Sprintf("adapter error: %s", cleanErr), "local-worker-1", tierName)
		_, _ = c.store.EventLog.Append("chunk.blocked", "worker", fmt.Sprintf("chunk %s adapter error: %s", wo.ID, cleanErr))
		c.notifyEvent(ctx, notify.EventQueueBlocked, "Chunk Blocked: "+wo.ID, fmt.Sprintf("chunk %s adapter error: %s", wo.ID, cleanErr))
		return err
	}

	// Check chunk cost circuit breaker after initial run
	if tripped, msg := c.checkChunkCostCircuitBreaker(wo.ID, tierName); tripped {
		return errors.New(msg)
	}

	// 5. Verification
	c.progPhase(wo.ID, phaseVerify, tierName, 1)
	changedPaths, _ := c.git.ChangedPaths(ctx)
	numstat, _ := c.git.Numstat(ctx, true, "")
	woBytes, _ := os.ReadFile(filepath.Join(c.projectRoot, filepath.FromSlash(wo.Path)))

	wState := verify.WorktreeState{
		WorktreeRoot:          c.projectRoot,
		WorkOrder:             wo,
		CurrentWorkOrderBytes: woBytes,
		ChangedPaths:          changedPaths,
		Numstat:               numstat,
		Protected:             scope.NewProtected(nil),
		IsSubChunk:            c.isSubChunk(wo.ID),
	}

	var vRes verify.VerifyResult
	var vErr error

	if runRes.ExitCode != 0 {
		vRes.Passed = false
		cleanOut := redact.Sanitize(strings.TrimSpace(runRes.Output))
		if cleanOut == "" {
			cleanOut = fmt.Sprintf("adapter %s exited with code %d", adapterName, runRes.ExitCode)
		}
		vRes.Errors = fmt.Sprintf("## Adapter Execution Error (%s)\n%s\n", adapterName, cleanOut)
	} else if len(changedPaths) == 0 {
		vRes.Passed = false
		vRes.Errors = fmt.Sprintf("## Zero Changes\nAdapter %s finished with exit code 0 but made no file changes to the repository.\n", adapterName)
	} else {
		vRes, vErr = c.verifier.Verify(ctx, wState)
		for _, q := range vRes.Quarantined {
			_, _ = c.store.EventLog.Append("test.quarantined", "verifier", fmt.Sprintf("gate %s quarantined for chunk %s: failure pre-existed on base commit", q, wo.ID))
		}
	}
	if vErr == nil && vRes.Passed {
		// Review tiering policy (STRIDE T7): HIGH requires human sign-off if configured
		if wo.Review == workorder.ReviewHigh && c.cfg.Policy.RequireHighReviewHuman {
			patchOut, _ := c.git.Exec(ctx, "diff", "HEAD")
			if patchOut != "" {
				_ = c.store.SaveHeldPatch(wo.ID, patchOut)
			}
			_ = c.store.SetChunkStatus(wo.ID, "held", "requires human sign-off for HIGH review chunk (STRIDE T7)", "local-worker-1", tierName)
			_, _ = c.store.EventLog.Append("chunk.held", "policy", fmt.Sprintf("chunk %s held for human review (HIGH risk per STRIDE T7)", wo.ID))
			c.notifyEvent(ctx, notify.EventChunkHeld, "Chunk Held: "+wo.ID, fmt.Sprintf("chunk %s held for human review", wo.ID))
			return nil
		}

		// Passed! Commit changes safely without staging .shepherd state DB
		c.progPhase(wo.ID, phaseCommit, tierName, 1)
		c.commitMu.Lock()
		c.markSourceDone(wo)
		c.stageChunkFiles(ctx, wo)
		_, _ = c.git.Exec(ctx, "commit", "-m", fmt.Sprintf("feat(%s): %s\n\nCo-Authored-By: ShepherdAI <shepherd@local>", wo.ID, wo.Title))
		c.commitMu.Unlock()

		_ = c.store.SetChunkStatus(wo.ID, "done", "", "local-worker-1", tierName)
		c.clearHandoffNote(wo.ID)
		_, _ = c.store.EventLog.Append("chunk.passed", "verifier", fmt.Sprintf("chunk %s passed verification", wo.ID))
		c.notifyEvent(ctx, notify.EventChunkPassed, fmt.Sprintf("Chunk Passed: %s", wo.ID), fmt.Sprintf("Chunk %s (\"%s\") passed verification on tier '%s' (%s)", wo.ID, wo.Title, tierName, tierCfg.Model))

		if isStopgap && !c.noCloud && (c.tierIsLocal(tierName, adapterName)) {
			c.checkAndRecoverCloudPools(context.Background())
		}
		return nil
	}

	// 6. Cascading Repair with Dynamic Tier Escalation (Improvement 5)
	firstOutcome := repair.OutcomeVerifyFailed
	if runRes.ExitCode != 0 {
		firstOutcome = repair.OutcomeAdapterError
	} else if len(changedPaths) == 0 {
		firstOutcome = repair.OutcomeNoChanges
	}
	attempts := []repair.AttemptRecord{
		c.attemptRecord(1, tierName, modelName, firstOutcome, vRes, redact.Sanitize(vRes.Errors), changedPaths, numstat),
	}
	currentErrors := redact.Sanitize(vRes.Errors)

	for repairRound := 2; repairRound <= 3; repairRound++ {
		if tripped, msg := c.checkChunkCostCircuitBreaker(wo.ID, tierName); tripped {
			return errors.New(msg)
		}

		isMech := repair.IsMechanicalFix(currentErrors, c.cfg.Repair.MechanicalFix)
		isSem := repair.IsSemanticFailure(currentErrors)

		prevTier := tierName
		repairTier := tierName
		if isStopgap || c.noCloud {
			repairTier = tierName
			runOpts.Timeout = 0
		} else if isMech && c.cfg.Repair.MechanicalFix.Tier != "" {
			repairTier = c.cfg.Repair.MechanicalFix.Tier
		} else if isSem {
			sTierName, _ := c.resolveStrongTier()
			repairTier = repair.EscalateTier(tierName, true, c.cfg.Repair.Chain, sTierName)
		}

		if c.tierLimited(repairTier) {
			if fn, _ := c.fallbackTier(repairTier); fn != "" {
				repairTier = fn
			}
		}

		// Resolve adapter and model for repair tier
		repairAd := ad
		repairModel := tierCfg.Model
		if rCfg, ok := c.cfg.Tiers[repairTier]; ok {
			if rCfg.Adapter != "" {
				if a, ok := c.adapters[rCfg.Adapter]; ok {
					repairAd = a
				}
			}
			if rCfg.Model != "" {
				repairModel = rCfg.Model
			}
		}

		if repairTier != prevTier {
			c.notifyEvent(ctx, notify.EventModelSwitch, fmt.Sprintf("Model Switch: %s", wo.ID), fmt.Sprintf("Chunk %s switched tier from '%s' to '%s' [model: %s, adapter: %s, repair attempt %d]", wo.ID, prevTier, repairTier, repairModel, repairAd.Name(), repairRound))
		}

		curPaths, _ := c.git.ChangedPaths(ctx)
		repairPrompt := repair.FormatRepairPromptWithHandoff(wo.ID, wo.Body, currentErrors, attempts, 20, c.handoffOptions(wo.ID, attempts, len(curPaths) > 0))
		repairPrompt += c.handoffInstructions(wo.ID)
		if c.tierIsLocal(repairTier, repairAd.Name()) || c.noCloud {
			repairPrompt += LocalContextGuardrails
		}
		_, _ = c.store.EventLog.Append("chunk.repair", "repair", fmt.Sprintf("chunk %s repair attempt %d dispatched to tier %s (semantic=%v, mechanical=%v)", wo.ID, repairRound, repairTier, isSem, isMech))

		c.progPhase(wo.ID, phaseRepair, repairTier+" · "+repairModel, repairRound)
		repairOpts := runOpts
		repairOpts.Model = repairModel
		repairRes, rErr := repairAd.Run(ctx, repairPrompt, repairOpts)
		c.recordAndCheckCost(wo, repairTier, repairRound, repairRes)
		if isStopgap && !c.noCloud && (c.tierIsLocal(repairTier, repairAd.Name())) {
			c.checkAndRecoverCloudPools(context.Background())
		}
		if rErr != nil || repairRes.ExitCode != 0 || c.limitHit(repairRes.Output) {
			limitText := repairRes.Output
			if rErr != nil {
				limitText += " " + rErr.Error()
			}
			if c.noteLimit(wo.ID, repairTier, limitText) {
				// Not the chunk's fault, but partial work may be in the tree: record it so the
				// next tier knows, then try another tier.
				if c.handoffOn() {
					lp, _ := c.git.ChangedPaths(ctx)
					ln, _ := c.git.Numstat(ctx, true, "")
					attempts = append(attempts, c.attemptRecord(repairRound, repairTier, repairModel, repair.OutcomeLimitHit, verify.VerifyResult{}, redact.Sanitize(strings.TrimSpace(limitText)), lp, ln))
				}
				continue
			}
			cleanFail := "adapter error"
			if rErr != nil {
				cleanFail = redact.Sanitize(rErr.Error())
			} else if repairRes.ExitCode != 0 {
				cleanFail = fmt.Sprintf("adapter %s exited with code %d: %s", repairAd.Name(), repairRes.ExitCode, redact.Sanitize(strings.TrimSpace(repairRes.Output)))
			}
			ep, _ := c.git.ChangedPaths(ctx)
			en, _ := c.git.Numstat(ctx, true, "")
			attempts = append(attempts, c.attemptRecord(repairRound, repairTier, repairModel, repair.OutcomeAdapterError, verify.VerifyResult{}, cleanFail, ep, en))
			continue
		}

		changedPaths, _ = c.git.ChangedPaths(ctx)
		numstat, _ = c.git.Numstat(ctx, true, "")
		wState.ChangedPaths = changedPaths
		wState.Numstat = numstat
		wState.IsSubChunk = c.isSubChunk(wo.ID)

		if len(changedPaths) == 0 {
			attempts = append(attempts, c.attemptRecord(repairRound, repairTier, repairModel, repair.OutcomeNoChanges, verify.VerifyResult{}, fmt.Sprintf("repair attempt on %s made zero file changes", repairTier), nil, nil))
			continue
		}

		c.progPhase(wo.ID, phaseVerify, repairTier, repairRound)
		vRes2, vErr2 := c.verifier.Verify(ctx, wState)
		for _, q := range vRes2.Quarantined {
			_, _ = c.store.EventLog.Append("test.quarantined", "verifier", fmt.Sprintf("gate %s quarantined for chunk %s: failure pre-existed on base commit", q, wo.ID))
		}

		if vErr2 == nil && vRes2.Passed {
			c.progPhase(wo.ID, phaseCommit, repairTier, repairRound)
			c.commitMu.Lock()
			c.markSourceDone(wo)
			c.stageChunkFiles(ctx, wo)
			_, _ = c.git.Exec(ctx, "commit", "-m", fmt.Sprintf("fix(%s): repair verification failures on attempt %d\n\nCo-Authored-By: ShepherdAI <shepherd@local>", wo.ID, repairRound))
			c.commitMu.Unlock()

			_ = c.store.SetChunkStatus(wo.ID, "done", "", "local-worker-1", repairTier)
			c.clearHandoffNote(wo.ID)
			_, _ = c.store.EventLog.Append("chunk.passed", "repair", fmt.Sprintf("chunk %s passed after attempt %d (%s)", wo.ID, repairRound, repairTier))
			c.notifyEvent(ctx, notify.EventChunkPassed, fmt.Sprintf("Chunk Passed: %s", wo.ID), fmt.Sprintf("Chunk %s (\"%s\") passed verification after repair attempt %d on tier '%s' (%s)", wo.ID, wo.Title, repairRound, repairTier, repairModel))
			if isStopgap && !c.noCloud && (c.tierIsLocal(repairTier, repairAd.Name())) {
				c.checkAndRecoverCloudPools(context.Background())
			}
			return nil
		}

		currentErrors = redact.Sanitize(vRes2.Errors)
		attempts = append(attempts, c.attemptRecord(repairRound, repairTier, repairModel, repair.OutcomeVerifyFailed, vRes2, currentErrors, changedPaths, numstat))
	}

	// Failed verification
	reason := redact.Sanitize(vRes.Errors)
	if strings.TrimSpace(reason) == "" {
		reason = "verification failed"
	}
	_ = c.store.SetChunkStatus(wo.ID, "blocked", reason, "local-worker-1", tierName)
	if isStopgap && !c.noCloud && (c.tierIsLocal(tierName, adapterName)) {
		c.checkAndRecoverCloudPools(context.Background())
	}
	_, _ = c.store.EventLog.Append("chunk.blocked", "verifier", fmt.Sprintf("chunk %s blocked: %s", wo.ID, reason))
	c.notifyEvent(ctx, notify.EventQueueBlocked, "Chunk Blocked: "+wo.ID, fmt.Sprintf("chunk %s blocked: %s", wo.ID, reason))
	return errors.New(reason)
}

func (c *Controller) recordAndCheckCost(wo *workorder.WorkOrder, tier string, attempt int, res adapter.RunResult) {
	if res.CostUSD > 0 || res.TokensUsed > 0 {
		_ = c.store.RecordChunkCost(wo.ID, wo.Sprint, tier, res.TokensUsed, res.CostUSD)
		_, _ = c.store.EventLog.Append("chunk.cost", "telemetry", fmt.Sprintf("chunk %s attempt %d tier %s cost=$%.4f tokens=%d", wo.ID, attempt, tier, res.CostUSD, res.TokensUsed))
	}
}

func (c *Controller) checkChunkCostCircuitBreaker(chunkID, tierName string) (bool, string) {
	if c.cfg.Policy.MaxChunkCostUSD <= 0 {
		return false, ""
	}
	cost, _, err := c.store.GetChunkCostUSD(chunkID)
	if err != nil || cost < c.cfg.Policy.MaxChunkCostUSD {
		return false, ""
	}
	msg := fmt.Sprintf("chunk cost circuit breaker tripped: $%.4f >= limit $%.4f", cost, c.cfg.Policy.MaxChunkCostUSD)
	_ = c.store.SetChunkStatus(chunkID, "blocked", msg, "local-worker-1", tierName)
	_, _ = c.store.EventLog.Append("chunk.blocked", "cost_guard", fmt.Sprintf("chunk %s blocked: %s", chunkID, msg))
	c.notifyEvent(context.Background(), notify.EventQueueBlocked, "Chunk Budget Exceeded: "+chunkID, msg)
	return true, msg
}

func (c *Controller) notifyEvent(ctx context.Context, eventType, title, message string) {
	if c.notifier != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		// ntfy posts are spaced 1s apart to keep their order, so allow for a short queue.
		nCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = c.notifier.Send(nCtx, eventType, title, redact.Sanitize(message))
	}
}

// NotifyEvent sends an operator notification across configured notification channels.
func (c *Controller) NotifyEvent(ctx context.Context, eventType, title, message string) {
	c.notifyEvent(ctx, eventType, title, message)
}

// FetchAssignment implements lan.WorkerChunkProvider for remote LAN workers.
func (c *Controller) FetchAssignment(ctx context.Context, workerID, chunkID string) (*lan.FetchResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var wo *workorder.WorkOrder
	if chunkID != "" {
		var ok bool
		wo, ok = c.workOrders[chunkID]
		if !ok {
			return nil, fmt.Errorf("chunk %s not found", chunkID)
		}
	} else {
		wo = c.nextRunnableChunkLocked()
		if wo == nil {
			return nil, errors.New("no runnable chunks available")
		}
	}

	headCommit, err := c.git.Exec(ctx, "rev-parse", "HEAD")
	if err != nil {
		headCommit = "0000000000000000000000000000000000000000"
	}
	headCommit = strings.TrimSpace(headCommit)

	tierName, tierCfg := c.resolveDefaultTier()
	poolName := "default"
	if tierCfg.Pool != "" {
		poolName = tierCfg.Pool
	}

	_ = c.store.SetChunkStatus(wo.ID, "running", "", workerID, tierName)
	_, _ = c.store.EventLog.Append("chunk.started", workerID, fmt.Sprintf("chunk %s assigned to %s (base %s)", wo.ID, workerID, headCommit))

	return &lan.FetchResponse{
		ChunkID:     wo.ID,
		WorkerID:    workerID,
		Title:       wo.Title,
		BaseCommit:  headCommit,
		BranchName:  fmt.Sprintf("shepherd/run/%s", wo.ID),
		WorkOrderMD: wo.Body,
		Tier:        tierName,
		Pool:        poolName,
	}, nil
}

// SubmitResult integrates finished work from a remote LAN worker in dependency order.
func (c *Controller) SubmitResult(ctx context.Context, workerID string, req lan.ResultRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	wo, ok := c.workOrders[req.ChunkID]
	if !ok {
		return fmt.Errorf("chunk %s not found", req.ChunkID)
	}

	tierName, _ := c.resolveDefaultTier()

	if req.Status == "done" {
		if req.Patch != "" {
			tmpPatch := filepath.Join(c.projectRoot, ".shepherd", fmt.Sprintf("patch-%s.diff", req.ChunkID))
			_ = os.WriteFile(tmpPatch, []byte(req.Patch), 0600)
			defer os.Remove(tmpPatch)

			_, applyErr := c.git.Exec(ctx, "apply", tmpPatch)
			if applyErr != nil {
				cleanErr := redact.Sanitize(applyErr.Error())
				_ = c.store.SetChunkStatus(wo.ID, "blocked", fmt.Sprintf("patch apply error: %s", cleanErr), workerID, tierName)
				_, _ = c.store.EventLog.Append("chunk.blocked", workerID, fmt.Sprintf("chunk %s patch apply failed: %s", wo.ID, cleanErr))
				return fmt.Errorf("git apply: %w", applyErr)
			}
		}

		// Run verify pipeline
		changedPaths, _ := c.git.ChangedPaths(ctx)
		numstat, _ := c.git.Numstat(ctx, true, "")
		woBytes, _ := os.ReadFile(filepath.Join(c.projectRoot, filepath.FromSlash(wo.Path)))

		wState := verify.WorktreeState{
			WorktreeRoot:          c.projectRoot,
			WorkOrder:             wo,
			CurrentWorkOrderBytes: woBytes,
			ChangedPaths:          changedPaths,
			Numstat:               numstat,
			Protected:             scope.NewProtected(nil),
			IsSubChunk:            c.isSubChunk(wo.ID),
		}

		vRes, vErr := c.verifier.Verify(ctx, wState)
		for _, q := range vRes.Quarantined {
			_, _ = c.store.EventLog.Append("test.quarantined", "verifier", fmt.Sprintf("gate %s quarantined for chunk %s: failure pre-existed on base commit", q, wo.ID))
		}
		if vErr == nil && vRes.Passed {
			// Review tiering policy (STRIDE T7): HIGH requires human sign-off if configured
			if wo.Review == workorder.ReviewHigh && c.cfg.Policy.RequireHighReviewHuman {
				if req.Patch != "" {
					_ = c.store.SaveHeldPatch(wo.ID, req.Patch)
				}
				// Rollback applied patch from main worktree while held in DB so subsequent chunks can run cleanly
				if req.Patch != "" {
					_, _ = c.git.Exec(ctx, "reset", "--hard", "HEAD")
					_, _ = c.git.Exec(ctx, "clean", "-fd", "-e", ".shepherd")
				}
				_ = c.store.SetChunkStatus(wo.ID, "held", "requires human sign-off for HIGH review chunk (STRIDE T7)", workerID, tierName)
				_, _ = c.store.EventLog.Append("chunk.held", "policy", fmt.Sprintf("remote chunk %s held for human review (HIGH risk per STRIDE T7)", wo.ID))
				return nil
			}

			c.commitMu.Lock()
			c.markSourceDone(wo)
			c.stageChunkFiles(ctx, wo)
			_, _ = c.git.Exec(ctx, "commit", "-m", fmt.Sprintf("feat(%s): %s\n\nCo-Authored-By: ShepherdAI <%s@worker>", wo.ID, wo.Title, workerID))
			c.commitMu.Unlock()

			_ = c.store.SetChunkStatus(wo.ID, "done", "", workerID, tierName)
			_, _ = c.store.EventLog.Append("chunk.passed", workerID, fmt.Sprintf("chunk %s integrated in dependency order from worker %s", wo.ID, workerID))
			return nil
		}

		reason := redact.Sanitize(vRes.Errors)
		if strings.TrimSpace(reason) == "" {
			reason = "verification failed"
		}
		_ = c.store.SetChunkStatus(wo.ID, "blocked", reason, workerID, tierName)
		_, _ = c.store.EventLog.Append("chunk.blocked", workerID, fmt.Sprintf("chunk %s verification failed: %s", wo.ID, reason))
		return errors.New(reason)
	}

	cleanOut := redact.Sanitize(req.Output)
	_ = c.store.SetChunkStatus(wo.ID, req.Status, cleanOut, workerID, tierName)
	_, _ = c.store.EventLog.Append("chunk."+req.Status, workerID, fmt.Sprintf("chunk %s %s reported by %s", wo.ID, req.Status, workerID))
	return nil
}

// describeStallLocked explains why pending chunks cannot be scheduled, naming the root blockers:
// unfinished dependencies that are not themselves pending (blocked, held, orphaned running, missing).
// applyAssignment returns the tier/config an operator assigned to a chunk. It is ignored (and the
// reason audited) when the tier is gone or deactivated, or when only local execution is allowed
// and the assigned tier is not local.
func (c *Controller) applyAssignment(chunkID string, localOnly bool) (string, config.TierConfig, bool) {
	rec, err := c.store.GetChunk(chunkID)
	if err != nil || rec.PinTier == "" {
		return "", config.TierConfig{}, false
	}
	ignore := func(why string) (string, config.TierConfig, bool) {
		_, _ = c.store.EventLog.Append("chunk.assignment_ignored", "controller", fmt.Sprintf("chunk %s: assignment %s/%s ignored: %s", chunkID, rec.PinTier, rec.PinModel, why))
		return "", config.TierConfig{}, false
	}
	c.mu.Lock()
	tc, ok := c.cfg.Tiers[rec.PinTier]
	local := ok && c.tierIsLocal(rec.PinTier, tc.Adapter)
	c.mu.Unlock()
	if !ok || !tc.IsEnabled() {
		return ignore("tier is missing or deactivated")
	}
	if localOnly && !local {
		return ignore("cloud is unavailable or --nocloud is set; running local")
	}
	if rec.PinModel != "" {
		tc.Model = rec.PinModel
	}
	return rec.PinTier, tc, true
}

func (c *Controller) describeStallLocked(pending []db.ChunkRecord) string {
	all, _ := c.store.ListChunks("")
	status := make(map[string]string, len(all))
	for _, ch := range all {
		status[ch.ID] = ch.Status
	}
	roots := map[string]bool{}
	inPlan, notInPlan := 0, 0
	for _, p := range pending {
		wo, ok := c.workOrders[p.ID]
		if !ok {
			notInPlan++
			continue
		}
		inPlan++
		depsMet := true
		for _, dep := range wo.DependsOn {
			st := status[dep]
			if st == "done" {
				continue
			}
			depsMet = false
			if st == "pending" {
				continue // not a root: it is waiting too
			}
			if st == "" {
				st = "missing"
			}
			roots[fmt.Sprintf("%s (%s)", dep, st)] = true
		}
		if depsMet {
			// Dependencies are met, so a running chunk with overlapping files is holding it back.
			for _, r := range all {
				if r.Status != "running" {
					continue
				}
				if rw, ok := c.workOrders[r.ID]; ok && hasFileOverlap(wo.Files, rw.Files) {
					roots[fmt.Sprintf("%s (running, overlapping files)", r.ID)] = true
				}
			}
		}
	}

	keys := make([]string, 0, len(roots))
	for k := range roots {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	msg := fmt.Sprintf("%d pending, none runnable.", inPlan)
	if len(keys) > 0 {
		msg += " Waiting on: " + strings.Join(keys, ", ") + ". Requeue with `shepherd unblock <id>` or skip with `shepherd queue skip <id>`."
	} else if inPlan > 0 {
		msg += " No root blocker found: check depends_on for a cycle."
	}
	if notInPlan > 0 {
		msg += fmt.Sprintf(" (%d pending rows have no work order in the plan tree.)", notInPlan)
	}
	return msg
}

// ReconcileOrphanedRuns requeues chunks a previous daemon left in "running": nothing will finish
// them, and every chunk that depends on them would wait forever. Chunks run by a LAN worker that
// still holds an active lease are left alone. Call at daemon startup, before Start.
func (c *Controller) ReconcileOrphanedRuns(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.runs) > 0 {
		return nil, errors.New("cannot reconcile while chunks are executing")
	}
	running, err := c.store.ListChunks("running")
	if err != nil {
		return nil, err
	}
	var reset []string
	for _, r := range running {
		if r.WorkerID != "" && r.WorkerID != localWorkerID {
			if live, err := c.store.HasActiveLease(r.ID); err != nil || live {
				continue
			}
		}
		if err := c.store.SetChunkStatus(r.ID, "pending", "requeued: orphaned run from a previous daemon", "", ""); err != nil {
			return reset, err
		}
		_, _ = c.store.EventLog.Append("chunk.orphan_requeued", "controller", fmt.Sprintf("chunk %s was left running by a previous daemon (worker %q, tier %q); requeued", r.ID, r.WorkerID, r.Tier))
		reset = append(reset, r.ID)
	}
	sort.Strings(reset)
	return reset, nil
}
