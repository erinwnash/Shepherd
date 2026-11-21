package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/db"
)

// queueAPI is the slice of the control client the queue manager uses; *control.Client satisfies it.
type queueAPI interface {
	QueueList(ctx context.Context) ([]db.ChunkRecord, error)
	QueueSetActive(ctx context.Context, chunkID string, active bool) error
	QueueSetAllActive(ctx context.Context, active bool) (int, error)
	QueueRetry(ctx context.Context, chunkID string) error
	QueueAssign(ctx context.Context, chunkID, tier, model string) error
	QueueRefresh(ctx context.Context) (control.QueueRefreshResult, error)
}

// queueClient avoids wrapping a nil *control.Client in a non-nil interface (offline/config mode).
func queueClient(c *control.Client) queueAPI {
	if c == nil {
		return nil
	}
	return c
}

// modelOption is one thing a chunk can be assigned to. The zero value means "route automatically".
type modelOption struct {
	Tier  string
	Model string // empty: the tier's configured model
	Label string
}

// queueModelOptions lists every model available to this session: each active stack tier with its
// known models, preceded by automatic routing.
func queueModelOptions(st *DashboardState) []modelOption {
	opts := []modelOption{{Label: "auto: route by complexity"}}
	for _, it := range st.StackItems {
		if !it.Enabled {
			continue
		}
		models := it.Models
		if len(models) == 0 {
			models = defaultCandidateModels(it.Adapter, it.Model)
		}
		if len(models) == 0 {
			opts = append(opts, modelOption{Tier: it.Tier, Label: fmt.Sprintf("%s (%s): tier default", it.Tier, it.Adapter)})
			continue
		}
		for _, m := range models {
			opts = append(opts, modelOption{Tier: it.Tier, Model: m, Label: fmt.Sprintf("%s (%s): %s", it.Tier, it.Adapter, m)})
		}
	}
	return opts
}

// QueueFilter selects which statuses the queue tab lists.
type QueueFilter int

const (
	QueueFilterAll QueueFilter = iota
	QueueFilterBlocked
	QueueFilterSkipped
	queueFilterCount
)

func (f QueueFilter) String() string {
	switch f {
	case QueueFilterBlocked:
		return "blocked"
	case QueueFilterSkipped:
		return "skipped"
	}
	return "all"
}

// visibleChunks applies the queue tab's filters to the full chunk list. The cursor, scrolling and
// every selection-based action work on this list, never on st.Chunks directly.
func visibleChunks(st *DashboardState) []db.ChunkRecord {
	if st.QueueFilter == QueueFilterAll && !st.QueueHideDone {
		return st.Chunks
	}
	out := make([]db.ChunkRecord, 0, len(st.Chunks))
	for _, c := range st.Chunks {
		if st.QueueHideDone && c.Status == "done" {
			continue
		}
		if (st.QueueFilter == QueueFilterBlocked && c.Status != "blocked") ||
			(st.QueueFilter == QueueFilterSkipped && c.Status != "skipped") {
			continue
		}
		out = append(out, c)
	}
	return out
}

// queueIndex returns the index of the selected chunk, re-anchoring on its ID when the list moved
// under the cursor (polling reorders and adds rows), and clamping otherwise.
func queueIndex(st *DashboardState) int {
	chunks := visibleChunks(st)
	n := len(chunks)
	if n == 0 {
		st.SelectedQueueIdx = 0
		return -1
	}
	if st.SelectedQueueID != "" {
		for i, c := range chunks {
			if c.ID == st.SelectedQueueID {
				st.SelectedQueueIdx = i
				return i
			}
		}
	}
	if st.SelectedQueueIdx < 0 {
		st.SelectedQueueIdx = 0
	}
	if st.SelectedQueueIdx >= n {
		st.SelectedQueueIdx = n - 1
	}
	st.SelectedQueueID = chunks[st.SelectedQueueIdx].ID
	return st.SelectedQueueIdx
}

func selectedChunk(st *DashboardState) *db.ChunkRecord {
	chunks := visibleChunks(st)
	if i := queueIndex(st); i >= 0 {
		return &chunks[i]
	}
	return nil
}

func moveQueueSelection(st *DashboardState, delta int) {
	chunks := visibleChunks(st)
	n := len(chunks)
	if n == 0 {
		return
	}
	i := queueIndex(st)
	i = ((i+delta)%n + n) % n
	st.SelectedQueueIdx = i
	st.SelectedQueueID = chunks[i].ID
}

func (st *DashboardState) queueFail(format string, a ...any) {
	st.ActionErr = fmt.Sprintf(format, a...)
}

func (st *DashboardState) queueOK(format string, a ...any) {
	st.LastAction = fmt.Sprintf(format, a...)
	st.ActionErr = ""
}

// idleHint reminds the operator that activating work does not start a stopped queue.
func idleHint(st *DashboardState) string {
	if strings.EqualFold(st.Status.State, "idle") {
		return " (daemon idle: press [r] to start the queue)"
	}
	return ""
}

func reloadChunks(ctx context.Context, api queueAPI, st *DashboardState) {
	if api == nil {
		return
	}
	if chunks, err := api.QueueList(ctx); err == nil {
		st.Chunks = chunks
	}
}

// ToggleQueueItem checks/unchecks the selected chunk (queued <-> inactive).
func ToggleQueueItem(ctx context.Context, api queueAPI, st *DashboardState) {
	ch := selectedChunk(st)
	if ch == nil {
		return
	}
	var active bool
	switch ch.Status {
	case "pending":
		active = false
	case "inactive":
		active = true
	default:
		st.queueFail("%s is %s: only queued/inactive chunks toggle. [x] requeues, [b] unblocks", ch.ID, ch.Status)
		return
	}
	if api == nil {
		st.queueFail("daemon offline: cannot change the queue")
		return
	}
	if err := api.QueueSetActive(ctx, ch.ID, active); err != nil {
		st.queueFail("%v", err)
		return
	}
	reloadChunks(ctx, api, st)
	if active {
		st.queueOK("%s checked: queued again%s", ch.ID, idleHint(st))
	} else {
		st.queueOK("%s unchecked: it will not run until re-checked", ch.ID)
	}
}

// SetAllQueue checks or unchecks every queued chunk.
func SetAllQueue(ctx context.Context, api queueAPI, st *DashboardState, active bool) {
	if api == nil {
		st.queueFail("daemon offline: cannot change the queue")
		return
	}
	n, err := api.QueueSetAllActive(ctx, active)
	if err != nil {
		st.queueFail("%v", err)
		return
	}
	reloadChunks(ctx, api, st)
	switch {
	case n == 0 && active:
		st.queueOK("nothing to check: no unchecked chunks")
	case n == 0:
		st.queueOK("nothing to uncheck: no queued chunks")
	case active:
		st.queueOK("checked all: %d chunk(s) queued again%s", n, idleHint(st))
	default:
		st.queueOK("unchecked all: %d queued chunk(s) paused (running work is untouched)", n)
	}
}

// RequeueQueueItem puts a finished, skipped, blocked, held or unchecked chunk back in the queue.
func RequeueQueueItem(ctx context.Context, api queueAPI, st *DashboardState) {
	ch := selectedChunk(st)
	if ch == nil {
		return
	}
	switch ch.Status {
	case "pending":
		st.queueOK("%s is already queued", ch.ID)
		return
	case "running":
		st.queueFail("%s is running: halt it first [h]", ch.ID)
		return
	}
	if api == nil {
		st.queueFail("daemon offline: cannot change the queue")
		return
	}
	if err := api.QueueRetry(ctx, ch.ID); err != nil {
		st.queueFail("%v", err)
		return
	}
	reloadChunks(ctx, api, st)
	st.queueOK("%s requeued (was %s)%s", ch.ID, ch.Status, idleHint(st))
}

// UnblockQueueItem clears a blocked chunk's failure and queues it again.
func UnblockQueueItem(ctx context.Context, api queueAPI, st *DashboardState) {
	ch := selectedChunk(st)
	if ch == nil {
		return
	}
	if ch.Status != "blocked" {
		st.queueFail("%s is %s, not blocked", ch.ID, ch.Status)
		return
	}
	if api == nil {
		st.queueFail("daemon offline: cannot change the queue")
		return
	}
	if err := api.QueueRetry(ctx, ch.ID); err != nil {
		st.queueFail("%v", err)
		return
	}
	reloadChunks(ctx, api, st)
	st.queueOK("%s unblocked and queued%s", ch.ID, idleHint(st))
}

// RefreshQueue re-reads the plan tree from disk and reloads the list.
func RefreshQueue(ctx context.Context, api queueAPI, st *DashboardState) {
	if api == nil {
		st.queueFail("daemon offline: cannot refresh the queue")
		return
	}
	res, err := api.QueueRefresh(ctx)
	if err != nil {
		st.queueFail("refresh failed: %v", err)
		return
	}
	reloadChunks(ctx, api, st)
	msg := fmt.Sprintf("queue refreshed from disk: %d new, %d updated", res.Added, res.Updated)
	if res.Reconciled > 0 {
		msg += fmt.Sprintf(", %d marked done (finished outside Shepherd)", res.Reconciled)
	}
	if res.Invalid > 0 {
		msg += fmt.Sprintf(", %d invalid file(s): %s", res.Invalid, res.FirstError)
		st.LastAction, st.ActionErr = "", msg
		return
	}
	st.queueOK("%s", msg)
}

// ClearQueueAssignment returns the selected chunk to automatic routing.
func ClearQueueAssignment(ctx context.Context, api queueAPI, st *DashboardState) {
	ch := selectedChunk(st)
	if ch == nil {
		return
	}
	assignQueueItem(ctx, api, st, ch, modelOption{})
}

func assignQueueItem(ctx context.Context, api queueAPI, st *DashboardState, ch *db.ChunkRecord, opt modelOption) {
	if ch.Status == "running" {
		st.queueFail("%s is running: halt it first [h]", ch.ID)
		return
	}
	if api == nil {
		st.queueFail("daemon offline: cannot change the queue")
		return
	}
	if err := api.QueueAssign(ctx, ch.ID, opt.Tier, opt.Model); err != nil {
		st.queueFail("%v", err)
		return
	}
	id := ch.ID
	reloadChunks(ctx, api, st)
	if opt.Tier == "" {
		st.queueOK("%s: automatic routing", id)
		return
	}
	model := opt.Model
	if model == "" {
		model = "tier default"
	}
	st.queueOK("%s assigned to %s / %s, whatever its complexity", id, opt.Tier, model)
}

// OpenQueuePicker starts choosing a model for the selected chunk, with its current assignment highlighted.
func OpenQueuePicker(st *DashboardState) {
	ch := selectedChunk(st)
	if ch == nil {
		return
	}
	if ch.Status == "running" {
		st.queueFail("%s is running: halt it first [h]", ch.ID)
		return
	}
	opts := queueModelOptions(st)
	if len(opts) <= 1 {
		st.queueFail("no active tiers to assign: add or activate one on the Stack tab [3]")
		return
	}
	st.QueuePicker = true
	st.QueuePickerIdx = 0
	for i, o := range opts {
		if o.Tier == ch.PinTier && o.Model == ch.PinModel {
			st.QueuePickerIdx = i
			break
		}
	}
	st.ActionErr = ""
	st.LastAction = ""
}

// handleQueueAction runs actions that mean something different on the queue tab. It returns
// handled=false for actions that should fall through to the global handler (tabs, pause, halt...).
func handleQueueAction(ctx context.Context, api queueAPI, st *DashboardState, act KeyAction) (handled, quit bool) {
	if st.QueuePicker {
		opts := queueModelOptions(st)
		switch act {
		case ActionQuit:
			return true, true
		case ActionUp:
			st.QueuePickerIdx = (st.QueuePickerIdx - 1 + len(opts)) % len(opts)
		case ActionDown:
			st.QueuePickerIdx = (st.QueuePickerIdx + 1) % len(opts)
		case ActionCancel, ActionModelCycle:
			st.QueuePicker = false
		case ActionConfirm, ActionToggle:
			st.QueuePicker = false
			if ch := selectedChunk(st); ch != nil && st.QueuePickerIdx < len(opts) {
				assignQueueItem(ctx, api, st, ch, opts[st.QueuePickerIdx])
			}
		}
		return true, false // the picker owns the keyboard until it closes
	}

	switch act {
	case ActionUp:
		moveQueueSelection(st, -1)
	case ActionDown:
		moveQueueSelection(st, 1)
	case ActionToggle:
		ToggleQueueItem(ctx, api, st)
	case ActionQueueCheckAll:
		SetAllQueue(ctx, api, st, true)
	case ActionQueueUncheckAll:
		SetAllQueue(ctx, api, st, false)
	case ActionQueueRequeue:
		RequeueQueueItem(ctx, api, st)
	case ActionQueueFilter:
		st.QueueFilter = (st.QueueFilter + 1) % queueFilterCount
		st.queueOK("showing %s chunks", st.QueueFilter)
	case ActionQueueHideDone:
		st.QueueHideDone = !st.QueueHideDone
		if st.QueueHideDone {
			st.queueOK("completed chunks hidden")
		} else {
			st.queueOK("completed chunks shown")
		}
	case ActionBudgetCap: // [b] unblock
		UnblockQueueItem(ctx, api, st)
	case ActionFloor: // [f] refresh from disk
		RefreshQueue(ctx, api, st)
	case ActionModelCycle, ActionConfirm: // [m] / Enter: pick a model
		OpenQueuePicker(st)
	case ActionUsageCap: // [c] back to automatic routing
		ClearQueueAssignment(ctx, api, st)
	case ActionPrioUp, ActionPrioDown, ActionRoutingRule, ActionHumanGate, ActionQuickTest, ActionQuickTestAll, ActionCancel:
		// Stack-tab editing keys: never edit tiers or rules while looking at the queue.
	default:
		return false, false
	}
	return true, false
}
