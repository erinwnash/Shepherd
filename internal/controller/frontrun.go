package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/erinwnash/Shepherd/internal/adapter"
	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/predict"
	"github.com/erinwnash/Shepherd/internal/quota"
	"github.com/erinwnash/Shepherd/internal/redact"
	"github.com/erinwnash/Shepherd/internal/repomap"
	"github.com/erinwnash/Shepherd/internal/sandbox"
	"github.com/erinwnash/Shepherd/internal/scope"
	"github.com/erinwnash/Shepherd/internal/verify"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

// Front-run burn lane: a second queue that spends a chosen model (even one switched off in the
// stack) on high-complexity chunks far down the main queue. It never touches the main working tree
// while it works: each chunk is built and verified in its own git worktree, and the finished patch
// is merged only when the main queue is between chunks, so the two lanes cannot see each other's
// half-done edits.
const (
	// frontRunSkipAhead is how many pending items stay with the main queue before the lane starts
	// cherry-picking.
	frontRunSkipAhead = 10
	// frontRunRAMMarginGB is held back on top of slots.min_free_ram_gb before the lane starts a
	// chunk, so it never takes the host to the floor the main queue relies on.
	frontRunRAMMarginGB = 2.0
	// frontRunMaxFailures consecutive failed attempts stop the lane rather than burn tokens.
	frontRunMaxFailures = 3
	// frontRunMinSessionPct is the least 5-hour session quota the lane's pool may have left for it to
	// start a new chunk. A pool's own floor_five_hour_pct wins when it is higher.
	frontRunMinSessionPct = 15
	frontRunWorker        = "frontrun"
)

// frontRunPollInterval is how often the lane re-checks what it is waiting for.
var frontRunPollInterval = 5 * time.Second

// frontRunLane is the running lane. The counters and note are guarded by Controller.frMu, a leaf lock.
type frontRunLane struct {
	tier    string
	cfg     config.TierConfig
	cancel  context.CancelFunc
	done    chan struct{}
	tried   map[string]bool
	current string
	note    string
	landed  int
	failed  int
}

// frontRunReady is a verified patch waiting for the main queue to be between chunks. Guarded by Controller.mu.
type frontRunReady struct {
	wo     *workorder.WorkOrder
	patch  string
	tier   string
	result chan error
}

// pickFrontRun chooses the next chunk for the lane from the main queue's pending list (in queue
// order). The first skipAhead items are never taken. After that a chunk qualifies when it is high
// complexity, every precursor is already done, it has not been tried by the lane this session, the
// operator has not assigned it a model, and its files collide with nothing running or sitting in the
// protected head of the queue.
func pickFrontRun(pending []db.ChunkRecord, orders map[string]*workorder.WorkOrder, status map[string]string,
	runningFiles [][]workorder.File, tried map[string]bool, skipAhead int, isHigh func(*workorder.WorkOrder) bool) *workorder.WorkOrder {
	if len(pending) <= skipAhead {
		return nil
	}
	busy := append([][]workorder.File(nil), runningFiles...)
	for _, rec := range pending[:skipAhead] {
		if wo, ok := orders[rec.ID]; ok {
			busy = append(busy, wo.Files)
		}
	}
	for _, rec := range pending[skipAhead:] {
		wo, ok := orders[rec.ID]
		if !ok || tried[rec.ID] || rec.PinTier != "" || !isHigh(wo) {
			continue
		}
		ready := true
		for _, dep := range wo.DependsOn {
			if status[dep] != "done" {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		clash := false
		for _, fs := range busy {
			if hasFileOverlap(wo.Files, fs) {
				clash = true
				break
			}
		}
		if !clash {
			return wo
		}
	}
	return nil
}

// FrontRunStart starts the lane on a tier (which may be switched off) and optional model.
func (c *Controller) FrontRunStart(ctx context.Context, tier, model string) error {
	c.mu.Lock()
	locked := c.lockdown
	tc, ok := c.cfg.Tiers[tier]
	c.mu.Unlock()
	if locked {
		return errors.New("cannot front-run: controller is in lockdown mode")
	}
	if !ok {
		return fmt.Errorf("unknown tier %q", tier)
	}
	if model != "" {
		tc.Model = model
	}

	c.frMu.Lock()
	if c.frontRun != nil {
		c.frMu.Unlock()
		return errors.New("front-run is already running")
	}
	laneCtx, cancel := context.WithCancel(context.Background())
	lane := &frontRunLane{tier: tier, cfg: tc, cancel: cancel, done: make(chan struct{}), tried: map[string]bool{}}
	c.frontRun = lane
	c.frMu.Unlock()

	_, _ = c.store.EventLog.Append("frontrun.started", "operator", fmt.Sprintf("front-run lane started on tier %s [model: %s]", tier, tc.Model))
	go c.runFrontRun(laneCtx, lane)
	return nil
}

// FrontRunStop stops the lane. Its current chunk goes back to the main queue.
func (c *Controller) FrontRunStop(ctx context.Context) error {
	c.frMu.Lock()
	lane := c.frontRun
	c.frMu.Unlock()
	if lane == nil {
		return nil
	}
	lane.cancel()
	select {
	case <-lane.done:
	case <-time.After(30 * time.Second):
		return errors.New("front-run lane did not stop within 30s")
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (c *Controller) frontRunStatus() *control.FrontRunStatus {
	c.frMu.Lock()
	defer c.frMu.Unlock()
	l := c.frontRun
	if l == nil {
		return nil
	}
	return &control.FrontRunStatus{Tier: l.tier, Model: l.cfg.Model, Current: l.current, Landed: l.landed, Failed: l.failed, Note: l.note}
}

func (c *Controller) frontRunUpdate(l *frontRunLane, f func(*frontRunLane)) {
	c.frMu.Lock()
	f(l)
	c.frMu.Unlock()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// frontRunRAMWait reports why the lane should not start (or continue) work, or "" when RAM allows.
func (c *Controller) frontRunRAMWait(margin float64) string {
	if c.slotsPool == nil || c.slotsPool.Guard == nil || c.slotsPool.Guard.RAMChecker == nil {
		return ""
	}
	g := c.slotsPool.Guard
	free, err := g.RAMChecker()
	if err != nil {
		return "" // cannot measure: do not stall the lane on it
	}
	if need := g.MinFreeRAMGB + margin; free < need {
		return fmt.Sprintf("waiting for RAM: %.1f GB free, need %.1f GB", free, need)
	}
	return ""
}

// frontRunQuotaWait polls the lane's pool and reports why a new chunk must not start: the 5-hour
// session bucket has less than 15% left. Pools that are not polled (local, until-rejected) and
// readings that cannot be taken do not block the lane.
func (c *Controller) frontRunQuotaWait(ctx context.Context, l *frontRunLane) string {
	c.mu.Lock()
	pool := l.cfg.Pool
	pc, ok := c.cfg.Quota.Pools[pool]
	poll := c.frontRunPoll
	c.mu.Unlock()
	if !ok || pc.Mode != "polled" || len(pc.UsageCmd) == 0 {
		return ""
	}
	if poll == nil {
		poll = func(ctx context.Context, pool string, pc config.PoolConfig) ([]quota.Bucket, error) {
			return quota.PollPoolCached(ctx, pool, pc, frontRunPollInterval)
		}
	}
	buckets, err := poll(ctx, pool, pc)
	if err != nil {
		return ""
	}
	need := frontRunMinSessionPct
	if pc.FloorFiveHourPct > need {
		need = pc.FloorFiveHourPct
	}
	for _, b := range buckets {
		if b.Window == "5h" && b.RemainingPct < need {
			msg := fmt.Sprintf("waiting for 5h session quota on %s: %d%% left, need %d%%", pool, b.RemainingPct, need)
			if !b.ResetTime.IsZero() {
				msg += " (resets " + b.ResetTime.Local().Format("15:04") + ")"
			}
			return msg
		}
	}
	return ""
}

func (c *Controller) runFrontRun(ctx context.Context, l *frontRunLane) {
	defer func() {
		c.frMu.Lock()
		c.frontRun = nil
		c.frMu.Unlock()
		_, _ = c.store.EventLog.Append("frontrun.stopped", "frontrun", fmt.Sprintf("front-run lane stopped: %d landed, %d handed back", l.landed, l.failed))
		close(l.done)
	}()

	_, _ = c.QueueRefresh(ctx)
	consecutiveFailures := 0
	for ctx.Err() == nil {
		c.mu.Lock()
		state, locked := c.state, c.lockdown
		c.mu.Unlock()
		if locked || state == StateLockdown {
			return
		}
		if state == StatePaused {
			c.frontRunUpdate(l, func(l *frontRunLane) { l.note = "paused with the main queue" })
			sleepCtx(ctx, frontRunPollInterval)
			continue
		}
		if why := c.frontRunRAMWait(frontRunRAMMarginGB); why != "" {
			c.frontRunUpdate(l, func(l *frontRunLane) { l.note = why })
			sleepCtx(ctx, frontRunPollInterval)
			continue
		}
		if why := c.frontRunQuotaWait(ctx, l); why != "" {
			c.frontRunUpdate(l, func(l *frontRunLane) { l.note = why })
			sleepCtx(ctx, frontRunPollInterval)
			continue
		}
		wo := c.nextFrontRunChunk(l)
		if wo == nil {
			c.frontRunUpdate(l, func(l *frontRunLane) {
				l.note = fmt.Sprintf("waiting: no high-complexity chunk beyond the next %d", frontRunSkipAhead)
			})
			sleepCtx(ctx, frontRunPollInterval)
			continue
		}
		c.frontRunUpdate(l, func(l *frontRunLane) { l.current, l.note = wo.ID, "" })
		landed, err := c.runFrontRunChunk(ctx, l, wo)
		c.frontRunUpdate(l, func(l *frontRunLane) {
			l.current = ""
			l.tried[wo.ID] = true
			switch {
			case landed:
				l.landed++
			case err != nil && ctx.Err() == nil:
				l.failed++
			}
		})
		switch {
		case landed:
			consecutiveFailures = 0
		case err != nil && ctx.Err() == nil:
			consecutiveFailures++
			if consecutiveFailures >= frontRunMaxFailures {
				c.frontRunUpdate(l, func(l *frontRunLane) {
					l.note = fmt.Sprintf("stopped after %d failed attempts in a row", consecutiveFailures)
				})
				_, _ = c.store.EventLog.Append("frontrun.gave_up", "frontrun", fmt.Sprintf("front-run lane stopped after %d consecutive failures; last: %v", consecutiveFailures, err))
				return
			}
		}
		// The main queue may have moved on (plan edits, work finished by hand): refresh it after every chunk.
		_, _ = c.QueueRefresh(ctx)
	}
}

// nextFrontRunChunk picks and claims a chunk. The claim marks it running, so the main queue skips
// it and its dependants wait for it.
func (c *Controller) nextFrontRunChunk(l *frontRunLane) *workorder.WorkOrder {
	c.mu.Lock()
	defer c.mu.Unlock()
	pending, err := c.store.ListChunks("pending")
	if err != nil {
		return nil
	}
	all, _ := c.store.ListChunks("")
	status := make(map[string]string, len(all))
	for _, rec := range all {
		status[rec.ID] = rec.Status
	}
	var runningFiles [][]workorder.File
	for _, rec := range all {
		if rec.Status == "running" {
			if wo, ok := c.workOrders[rec.ID]; ok {
				runningFiles = append(runningFiles, wo.Files)
			}
		}
	}
	c.frMu.Lock()
	tried := make(map[string]bool, len(l.tried))
	for k, v := range l.tried {
		tried[k] = v
	}
	c.frMu.Unlock()

	high := func(wo *workorder.WorkOrder) bool {
		strong, _ := predict.Route(wo, c.predictModel, c.cfg.Policy.CloudFirstThreshold)
		return strong
	}
	wo := pickFrontRun(pending, c.workOrders, status, runningFiles, tried, frontRunSkipAhead, high)
	if wo == nil {
		return nil
	}
	if err := c.store.SetChunkStatus(wo.ID, "running", "front-run", frontRunWorker, l.tier); err != nil {
		return nil
	}
	_, _ = c.store.EventLog.Append("frontrun.chunk_started", "frontrun", fmt.Sprintf("chunk %s claimed by the front-run lane (%s) [model: %s]", wo.ID, l.tier, l.cfg.Model))
	return wo
}

func (c *Controller) frontRunRelease(wo *workorder.WorkOrder, why string) {
	_ = c.store.SetChunkStatus(wo.ID, "pending", "", "", "")
	_, _ = c.store.EventLog.Append("frontrun.chunk_released", "frontrun", fmt.Sprintf("chunk %s handed back to the main queue: %s", wo.ID, why))
}

// prepareFrontRunTree checks the lane's worktree out at sha, clean. Ignored files (node_modules,
// build caches) are kept, and tools that resolve dependencies upward still find the main tree's.
func (c *Controller) prepareFrontRunTree(ctx context.Context, sha string) (string, *verify.GitRunner, error) {
	path := filepath.Join(c.projectRoot, ".shepherd", "frontrun")
	rootGit := verify.NewGitRunner(c.projectRoot)
	reset := func() (*verify.GitRunner, error) {
		g := verify.NewGitRunner(path)
		for _, args := range [][]string{{"checkout", "-f", "--detach", sha}, {"reset", "--hard", sha}, {"clean", "-fd"}} {
			if _, err := g.Exec(ctx, args...); err != nil {
				return nil, err
			}
		}
		return g, nil
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		if g, err := reset(); err == nil {
			return path, g, nil
		}
	}
	_ = os.RemoveAll(path)
	_, _ = rootGit.Exec(ctx, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", nil, err
	}
	if _, err := rootGit.Exec(ctx, "worktree", "add", "--detach", path, sha); err != nil {
		return "", nil, fmt.Errorf("create front-run worktree: %w", err)
	}
	g, err := reset()
	return path, g, err
}

// frontRunVerifier is the main verifier with its sandbox pointed at the lane's worktree.
func (c *Controller) frontRunVerifier(root string) *verify.Verifier {
	v := *c.verifier
	if c.cfg.Policy.SandboxBackend != "" {
		v.SandboxWrapper = func(argv []string) ([]string, error) {
			return sandbox.Wrap(argv, sandbox.Profile{
				WorktreeDir:      root,
				AllowUnsandboxed: c.cfg.Policy.AllowUnsandboxed,
				AllowNetwork:     false,
				Backend:          c.cfg.Policy.SandboxBackend,
				ContainerImage:   c.cfg.Policy.ContainerImage,
			})
		}
	}
	return &v
}

// runFrontRunChunk builds one chunk in the lane's worktree and, if it verifies, lands it on the main
// branch. It reports whether the chunk landed; any other outcome leaves the chunk for the main queue.
func (c *Controller) runFrontRunChunk(ctx context.Context, l *frontRunLane, wo *workorder.WorkOrder) (bool, error) {
	fail := func(err error) (bool, error) {
		c.frontRunRelease(wo, redact.Sanitize(err.Error()))
		return false, err
	}

	sha, err := c.git.Exec(ctx, "rev-parse", "HEAD")
	if err != nil {
		return fail(err)
	}
	sha = strings.TrimSpace(sha)
	root, wtGit, err := c.prepareFrontRunTree(ctx, sha)
	if err != nil {
		return fail(err)
	}

	c.progBegin(wo.ID, "frontrun")
	defer c.progEnd(wo.ID)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go c.progWatch(watchCtx, wo.ID, wtGit)

	adapterName := "agy"
	if l.cfg.Adapter != "" {
		adapterName = l.cfg.Adapter
	}
	ad, ok := c.adapters[adapterName]
	if !ok {
		if isLocalAdapter(adapterName) {
			ad = adapter.NewOpenCodeAdapter()
		} else {
			ad = adapter.NewAgyAdapter()
		}
	}
	isLocal := c.tierIsLocal(l.tier, adapterName)

	prof := sandbox.Profile{
		WorktreeDir:         root,
		AdapterDirs:         ad.Capabilities().Paths,
		ReadOnlyAdapterDirs: ad.Capabilities().ReadOnlyPaths,
		MaskDirs:            []string{filepath.Join(c.projectRoot, ".shepherd"), filepath.Dir(control.DefaultSocketPath())},
		AllowUnsandboxed:    c.cfg.Policy.AllowUnsandboxed,
		AllowNetwork:        ad.Capabilities().RequiresNetwork,
		Backend:             c.cfg.Policy.SandboxBackend,
		ContainerImage:      c.cfg.Policy.ContainerImage,
	}
	prompt := repomap.EnrichPrompt(wo, root)
	if strings.TrimSpace(prompt) == "" {
		prompt = fmt.Sprintf("Implement chunk %s: %s", wo.ID, wo.Title)
	}
	var timeout time.Duration
	if isLocal {
		prompt += LocalContextGuardrails
		timeout = 10 * time.Minute
	}

	// If free RAM falls to the floor while the model runs, give the memory back to the host.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var lowRAMHit atomic.Bool
	go func() {
		t := time.NewTicker(frontRunPollInterval)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if why := c.frontRunRAMWait(0); why != "" {
					lowRAMHit.Store(true)
					c.frontRunUpdate(l, func(l *frontRunLane) { l.note = "yielding: " + why })
					cancelRun()
					return
				}
			}
		}
	}()

	c.progPhase(wo.ID, phaseAgent, l.tier+" · "+l.cfg.Model, 1)
	runRes, runErr := ad.Run(runCtx, prompt, adapter.RunOptions{
		OnOutput:     c.progOutput(wo.ID),
		WorktreeRoot: root,
		Model:        l.cfg.Model,
		Effort:       string(wo.Effort),
		Timeout:      timeout,
		Sandbox:      &prof,
	})
	c.recordAndCheckCost(wo, l.tier, 1, runRes)
	cancelRun()
	if ctx.Err() != nil {
		c.frontRunRelease(wo, "front-run stopped")
		return false, ctx.Err()
	}
	if lowRAMHit.Load() {
		c.frontRunRelease(wo, "host RAM fell to the floor; yielded")
		sleepCtx(ctx, 10*frontRunPollInterval)
		return false, errors.New("yielded to low RAM")
	}
	if runErr != nil {
		return fail(fmt.Errorf("adapter error: %w", runErr))
	}

	changed, _ := wtGit.ChangedPaths(ctx)
	if runRes.ExitCode != 0 {
		return fail(fmt.Errorf("adapter %s exited with code %d: %s", adapterName, runRes.ExitCode, tailLine(runRes.Output)))
	}
	if len(changed) == 0 {
		return fail(errors.New("zero changes: the model made no file changes"))
	}
	numstat, _ := wtGit.Numstat(ctx, true, "")
	c.progPhase(wo.ID, phaseVerify, l.tier, 1)
	woBytes, _ := os.ReadFile(filepath.Join(c.projectRoot, filepath.FromSlash(wo.Path)))
	vRes, vErr := c.frontRunVerifier(root).Verify(ctx, verify.WorktreeState{
		WorktreeRoot:          root,
		WorkOrder:             wo,
		CurrentWorkOrderBytes: woBytes,
		ChangedPaths:          changed,
		Numstat:               numstat,
		Protected:             scope.NewProtected(nil),
		IsSubChunk:            c.isSubChunk(wo.ID),
	})
	if ctx.Err() != nil {
		c.frontRunRelease(wo, "front-run stopped")
		return false, ctx.Err()
	}
	if vErr != nil {
		return fail(vErr)
	}
	if !vRes.Passed {
		return fail(fmt.Errorf("verification failed: %s", firstLine(vRes.Errors)))
	}

	c.progPhase(wo.ID, phaseCommit, l.tier, 1)
	if _, err := wtGit.Exec(ctx, "add", "-A", "--", ":!.shepherd"); err != nil {
		return fail(err)
	}
	patch, err := wtGit.Exec(ctx, "diff", "--cached", "--binary", "HEAD")
	if err != nil || strings.TrimSpace(patch) == "" {
		return fail(fmt.Errorf("could not extract the patch: %v", err))
	}

	// Same human sign-off policy as the main queue: HIGH review waits for approval instead of landing.
	if wo.Review == workorder.ReviewHigh && c.cfg.Policy.RequireHighReviewHuman {
		_ = c.store.SaveHeldPatch(wo.ID, patch)
		_ = c.store.SetChunkStatus(wo.ID, "held", "requires human sign-off for HIGH review chunk (front-run)", frontRunWorker, l.tier)
		_, _ = c.store.EventLog.Append("chunk.held", "frontrun", fmt.Sprintf("chunk %s held for human review (front-run, HIGH risk per STRIDE T7)", wo.ID))
		return false, nil
	}

	if err := c.landFrontRun(ctx, &frontRunReady{wo: wo, patch: patch, tier: l.tier, result: make(chan error, 1)}); err != nil {
		if ctx.Err() != nil {
			c.frontRunRelease(wo, "front-run stopped before the patch landed")
			return false, ctx.Err()
		}
		return fail(err)
	}
	return true, nil
}

// tailLine is the last non-empty line of an adapter's output, enough to tell why it died.
func tailLine(out string) string {
	lines := strings.Split(strings.TrimSpace(redact.Sanitize(out)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 300 {
		last = last[:300]
	}
	return last
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
		if strings.HasPrefix(s, "##") {
			return s
		}
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// landFrontRun hands a verified patch to the main tree and waits for it to be merged. Merging
// happens only while no main chunk is running (the main loop calls landFrontRunLocked between
// chunks; this loop also tries, for when the main queue is idle), so a landed commit can never
// be wiped by a halt/rollback of a chunk that started before it.
func (c *Controller) landFrontRun(ctx context.Context, r *frontRunReady) error {
	c.mu.Lock()
	c.frReady = r
	c.landFrontRunLocked()
	c.mu.Unlock()
	for {
		select {
		case err := <-r.result:
			return err
		case <-ctx.Done():
			c.mu.Lock()
			mine := c.frReady == r
			if mine {
				c.frReady = nil
			}
			c.mu.Unlock()
			if mine {
				return ctx.Err()
			}
			return <-r.result // the merge was already under way
		case <-time.After(frontRunPollInterval / 4):
			c.mu.Lock()
			c.landFrontRunLocked()
			c.mu.Unlock()
		}
	}
}

// landFrontRunLocked merges the waiting patch if no main chunk is active. c.mu must be held.
func (c *Controller) landFrontRunLocked() {
	r := c.frReady
	if r == nil || c.activeChunk != nil || len(c.runs) > 0 {
		return
	}
	c.frReady = nil
	err := c.applyFrontRunPatch(r)
	if err == nil {
		_ = c.store.SetChunkStatus(r.wo.ID, "done", "front-run", frontRunWorker, r.tier)
		_, _ = c.store.EventLog.Append("frontrun.chunk_landed", "frontrun", fmt.Sprintf("chunk %s finished by the front-run lane and merged", r.wo.ID))
	}
	r.result <- err
}

func (c *Controller) applyFrontRunPatch(r *frontRunReady) error {
	ctx := context.Background()
	tmp := filepath.Join(c.projectRoot, ".shepherd", fmt.Sprintf("frontrun-%s.patch", r.wo.ID))
	if err := os.WriteFile(tmp, []byte(r.patch), 0600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	// git apply is all-or-nothing: on a conflict the main tree is left exactly as it was.
	if _, err := c.git.Exec(ctx, "apply", "--binary", "--recount", tmp); err != nil {
		return fmt.Errorf("patch no longer applies to the main tree: %w", err)
	}
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	c.markSourceDone(r.wo)
	c.stageChunkFiles(ctx, r.wo)
	_, _ = c.git.Exec(ctx, "commit", "-m", fmt.Sprintf("feat(%s): %s\n\nFront-run: %s\nCo-Authored-By: ShepherdAI <shepherd@local>", r.wo.ID, r.wo.Title, r.tier))
	return nil
}
