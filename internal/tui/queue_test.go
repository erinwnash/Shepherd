package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/db"
)

// fakeQueue records queue calls and applies them to an in-memory list like the daemon would.
type fakeQueue struct {
	chunks  []db.ChunkRecord
	calls   []string
	failOn  string
	refresh control.QueueRefreshResult
}

func (f *fakeQueue) find(id string) *db.ChunkRecord {
	for i := range f.chunks {
		if f.chunks[i].ID == id {
			return &f.chunks[i]
		}
	}
	return nil
}

func (f *fakeQueue) QueueList(context.Context) ([]db.ChunkRecord, error) {
	return append([]db.ChunkRecord(nil), f.chunks...), nil
}
func (f *fakeQueue) QueueSetActive(_ context.Context, id string, active bool) error {
	f.calls = append(f.calls, fmt.Sprintf("active %s %v", id, active))
	if f.failOn == "active" {
		return errors.New("boom")
	}
	if c := f.find(id); c != nil {
		if active {
			c.Status = "pending"
		} else {
			c.Status = "inactive"
		}
	}
	return nil
}
func (f *fakeQueue) QueueSetAllActive(_ context.Context, active bool) (int, error) {
	f.calls = append(f.calls, fmt.Sprintf("all %v", active))
	from, to := "pending", "inactive"
	if active {
		from, to = "inactive", "pending"
	}
	n := 0
	for i := range f.chunks {
		if f.chunks[i].Status == from {
			f.chunks[i].Status = to
			n++
		}
	}
	return n, nil
}
func (f *fakeQueue) QueueRetry(_ context.Context, id string) error {
	f.calls = append(f.calls, "retry "+id)
	if c := f.find(id); c != nil {
		c.Status, c.BlockedReason = "pending", ""
	}
	return nil
}
func (f *fakeQueue) QueueAssign(_ context.Context, id, tier, model string) error {
	f.calls = append(f.calls, fmt.Sprintf("assign %s %s %s", id, tier, model))
	if c := f.find(id); c != nil {
		c.PinTier, c.PinModel = tier, model
	}
	return nil
}
func (f *fakeQueue) QueueRefresh(context.Context) (control.QueueRefreshResult, error) {
	f.calls = append(f.calls, "refresh")
	return f.refresh, nil
}

func queueFixture() (*DashboardState, *fakeQueue) {
	fq := &fakeQueue{chunks: []db.ChunkRecord{
		{ID: "01A", Title: "Done thing", Status: "done", Sprint: "S1", Tier: "primary"},
		{ID: "01B", Title: "Running thing", Status: "running", Sprint: "S1", Tier: "primary"},
		{ID: "01C", Title: "Queued thing", Status: "pending", Sprint: "S1"},
		{ID: "01D", Title: "Unchecked thing", Status: "inactive", Sprint: "S1"},
		{ID: "01E", Title: "Broken thing", Status: "blocked", Sprint: "S1", BlockedReason: "verification failed: go test", AttemptNo: 2},
		{ID: "01F", Title: "Pinned thing", Status: "pending", Sprint: "S2", PinTier: "strong", PinModel: "claude-opus-5-5"},
	}}
	st := &DashboardState{
		ActiveTab: TabQueue,
		Chunks:    append([]db.ChunkRecord(nil), fq.chunks...),
		StackItems: []StackItem{
			{Tier: "local", Adapter: "opencode", Model: "qwen-coder", Enabled: true},
			{Tier: "strong", Adapter: "claude-cli", Model: "sonnet", Enabled: true},
			{Tier: "off", Adapter: "claude-cli", Model: "haiku", Enabled: false},
		},
	}
	st.Status.State = "running"
	return st, fq
}

func act(st *DashboardState, fq *fakeQueue, a KeyAction) {
	handleQueueAction(context.Background(), fq, st, a)
}

func selectID(st *DashboardState, id string) {
	st.SelectedQueueID = id
	queueIndex(st)
}

func TestTUI_QueueTab_KeysAndTabCycle(t *testing.T) {
	for b, want := range map[byte]KeyAction{'4': ActionTab4, 'x': ActionQueueRequeue, 'a': ActionQueueCheckAll, 'z': ActionQueueUncheckAll, '\r': ActionConfirm} {
		if got := mapByteToAction(b); got != want {
			t.Errorf("byte %q = %v, want %v", b, got, want)
		}
	}
	if acts := parseInputBytes([]byte{0x1b}); len(acts) != 1 || acts[0] != ActionCancel {
		t.Errorf("a lone Esc should cancel, got %v", acts)
	}
	if acts := parseInputBytes([]byte("\x1b[A")); len(acts) != 1 || acts[0] != ActionUp {
		t.Errorf("arrow keys must still parse as arrows, got %v", acts)
	}

	st := &DashboardState{}
	ExecuteAction(context.Background(), nil, st, ActionTab4)
	if st.ActiveTab != TabQueue {
		t.Fatalf("[4] should open the queue tab, got %v", st.ActiveTab)
	}
	ExecuteAction(context.Background(), nil, st, ActionTabNext)
	if st.ActiveTab != TabOverview {
		t.Errorf("Tab should wrap from queue to overview, got %v", st.ActiveTab)
	}
	ExecuteAction(context.Background(), nil, st, ActionTabPrev)
	if st.ActiveTab != TabQueue {
		t.Errorf("Shift-Tab should wrap from overview to queue, got %v", st.ActiveTab)
	}
}

func TestTUI_QueueTab_RenderShowsEveryRowAndFits(t *testing.T) {
	st, _ := queueFixture()
	selectID(st, "01E")
	for _, size := range [][2]int{{80, 24}, {120, 40}, {80, 18}} {
		out := Render(*st, size[0], size[1])
		plain := StripAnsi(out)
		lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
		if len(lines) > size[1] {
			t.Errorf("%dx%d: rendered %d lines", size[0], size[1], len(lines))
		}
		for i, l := range lines {
			if w := VisibleLen(l); w > size[0] {
				t.Errorf("%dx%d: line %d is %d wide: %q", size[0], size[1], i, w, l)
			}
		}
		if !strings.Contains(plain, "[4] QUEUE") || !strings.Contains(plain, "QUEUE MANAGER") {
			t.Errorf("%dx%d: missing queue chrome:\n%s", size[0], size[1], plain)
		}
	}

	plain := StripAnsi(Render(*st, 100, 40))
	for _, want := range []string{
		"01A", "01B", "01C", "01D", "01E", "01F",
		"[x]", "[ ]", "[!]", "[✓]", "[>]",
		"strong / claude-opus-5-5",
		"> [!] 01E", // the cursor is on the selected row
		"verification failed: go test",
		"Check all", "Uncheck all", "Requeue", "Unblock", "Model", "Refresh from disk",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("queue tab missing %q:\n%s", want, plain)
		}
	}
}

func TestTUI_QueueTab_LongQueueScrollsWithSelection(t *testing.T) {
	st, _ := queueFixture()
	st.Chunks = nil
	for i := 0; i < 200; i++ {
		st.Chunks = append(st.Chunks, db.ChunkRecord{ID: fmt.Sprintf("C%03d", i), Title: "t", Status: "pending"})
	}
	selectID(st, "C150")
	out := StripAnsi(Render(*st, 80, 24))
	if !strings.Contains(out, "C150") {
		t.Fatalf("the selected row must stay on screen:\n%s", out)
	}
	if strings.Contains(out, "C000") {
		t.Error("window should have scrolled away from the top")
	}
	if !strings.Contains(out, "/200]") {
		t.Error("position indicator missing")
	}
	if n := len(strings.Split(strings.TrimRight(out, "\n"), "\n")); n > 24 {
		t.Errorf("rendered %d lines", n)
	}
}

func TestTUI_QueueTab_EmptyQueueIsExplained(t *testing.T) {
	st, _ := queueFixture()
	st.Chunks = nil
	if out := StripAnsi(Render(*st, 80, 24)); !strings.Contains(out, "queue is empty") {
		t.Errorf("empty queue should say so:\n%s", out)
	}
	act(st, &fakeQueue{}, ActionDown) // must not panic
	ToggleQueueItem(context.Background(), &fakeQueue{}, st)
}

func TestTUI_QueueTab_ArrowsWrapAndFollowTheChunk(t *testing.T) {
	st, fq := queueFixture()
	act(st, fq, ActionUp) // from the first row wraps to the last
	if st.SelectedQueueID != "01F" {
		t.Fatalf("Up from the top = %s, want 01F", st.SelectedQueueID)
	}
	act(st, fq, ActionDown)
	if st.SelectedQueueID != "01A" {
		t.Fatalf("Down from the bottom = %s, want 01A", st.SelectedQueueID)
	}
	selectID(st, "01D")
	// New chunks sort ahead of the cursor on the next poll: the cursor must stay on 01D.
	st.Chunks = append([]db.ChunkRecord{{ID: "00X", Status: "pending"}, {ID: "00Y", Status: "pending"}}, st.Chunks...)
	if got := selectedChunk(st); got == nil || got.ID != "01D" {
		t.Fatalf("selection drifted to %+v", got)
	}
	// The selected chunk vanished (plan file removed): clamp instead of crashing.
	st.Chunks = st.Chunks[:2]
	if got := selectedChunk(st); got == nil {
		t.Fatal("selection should clamp to a valid row")
	}
}

func TestTUI_QueueTab_ToggleDeactivatesAndReactivates(t *testing.T) {
	st, fq := queueFixture()
	selectID(st, "01C")
	act(st, fq, ActionToggle)
	if got := strings.Join(fq.calls, ";"); got != "active 01C false" {
		t.Fatalf("calls = %q", got)
	}
	if st.Chunks[2].Status != "inactive" {
		t.Errorf("list should reload after the action, got %s", st.Chunks[2].Status)
	}
	act(st, fq, ActionToggle)
	if got := fq.calls[len(fq.calls)-1]; got != "active 01C true" {
		t.Errorf("second toggle = %q", got)
	}

	// Chunks in other states are not toggled, and the user is told what to press instead.
	for _, id := range []string{"01A", "01B", "01E"} {
		fq.calls = nil
		selectID(st, id)
		st.ActionErr = ""
		act(st, fq, ActionToggle)
		if len(fq.calls) != 0 {
			t.Errorf("%s: toggle reached the daemon: %v", id, fq.calls)
		}
		if st.ActionErr == "" {
			t.Errorf("%s: no explanation for the refused toggle", id)
		}
	}

	fq.failOn = "active"
	selectID(st, "01C")
	act(st, fq, ActionToggle)
	if !strings.Contains(st.ActionErr, "boom") {
		t.Errorf("daemon errors must be shown, got %q", st.ActionErr)
	}
}

func TestTUI_QueueTab_CheckAllAndUncheckAll(t *testing.T) {
	st, fq := queueFixture()
	act(st, fq, ActionQueueUncheckAll)
	if !strings.Contains(st.LastAction, "unchecked all: 2") {
		t.Errorf("notice = %q", st.LastAction)
	}
	statuses := map[string]string{}
	for _, c := range st.Chunks {
		statuses[c.ID] = c.Status
	}
	if statuses["01B"] != "running" || statuses["01E"] != "blocked" || statuses["01A"] != "done" {
		t.Errorf("uncheck all touched non-queued chunks: %v", statuses)
	}

	st.Status.State = "idle"
	act(st, fq, ActionQueueCheckAll)
	if !strings.Contains(st.LastAction, "checked all: 3") || !strings.Contains(st.LastAction, "[r]") {
		t.Errorf("check all on an idle daemon should say how to start it, got %q", st.LastAction)
	}
	act(st, fq, ActionQueueCheckAll)
	if !strings.Contains(st.LastAction, "nothing to check") {
		t.Errorf("notice = %q", st.LastAction)
	}
}

func TestTUI_QueueTab_RequeueAndUnblockRules(t *testing.T) {
	st, fq := queueFixture()

	selectID(st, "01E")
	act(st, fq, ActionBudgetCap) // [b]
	if got := strings.Join(fq.calls, ";"); got != "retry 01E" || st.Chunks[4].Status != "pending" {
		t.Fatalf("unblock: calls=%q status=%s", got, st.Chunks[4].Status)
	}

	fq.calls = nil
	selectID(st, "01A")
	act(st, fq, ActionBudgetCap)
	if len(fq.calls) != 0 || !strings.Contains(st.ActionErr, "not blocked") {
		t.Errorf("unblock on a done chunk must be refused: calls=%v err=%q", fq.calls, st.ActionErr)
	}
	act(st, fq, ActionQueueRequeue) // done -> requeue is allowed
	if got := strings.Join(fq.calls, ";"); got != "retry 01A" {
		t.Errorf("requeue of a done chunk: %q", got)
	}

	fq.calls = nil
	selectID(st, "01B")
	act(st, fq, ActionQueueRequeue)
	if len(fq.calls) != 0 || !strings.Contains(st.ActionErr, "running") {
		t.Errorf("a running chunk must never be requeued: calls=%v err=%q", fq.calls, st.ActionErr)
	}
	selectID(st, "01C")
	st.ActionErr = ""
	act(st, fq, ActionQueueRequeue)
	if len(fq.calls) != 0 || st.ActionErr != "" {
		t.Errorf("requeue of an already queued chunk should be a no-op, calls=%v err=%q", fq.calls, st.ActionErr)
	}
}

func TestTUI_QueueTab_RefreshFromDisk(t *testing.T) {
	st, fq := queueFixture()
	fq.refresh = control.QueueRefreshResult{Added: 3, Updated: 1}
	act(st, fq, ActionFloor) // [f]
	if got := strings.Join(fq.calls, ";"); got != "refresh" {
		t.Fatalf("calls = %q", got)
	}
	if !strings.Contains(st.LastAction, "3 new, 1 updated") || st.ActionErr != "" {
		t.Errorf("notice = %q err = %q", st.LastAction, st.ActionErr)
	}

	fq.refresh = control.QueueRefreshResult{Invalid: 2, FirstError: "docs/plan/x.md: no frontmatter"}
	act(st, fq, ActionFloor)
	if !strings.Contains(st.ActionErr, "2 invalid") || !strings.Contains(st.ActionErr, "no frontmatter") {
		t.Errorf("invalid plan files must be surfaced as an error, got %q", st.ActionErr)
	}
}

func TestTUI_QueueTab_AssignModelPicker(t *testing.T) {
	st, fq := queueFixture()
	selectID(st, "01C")

	act(st, fq, ActionModelCycle) // [m]
	if !st.QueuePicker {
		t.Fatal("[m] should open the picker")
	}
	opts := queueModelOptions(st)
	if opts[0].Tier != "" {
		t.Fatal("first option must be automatic routing")
	}
	for _, o := range opts {
		if o.Tier == "off" {
			t.Fatalf("deactivated tiers must not be offered: %+v", o)
		}
	}
	// Every active tier's models are offered, including the one configured.
	var haveLocal, haveSonnet, haveOpus bool
	for _, o := range opts {
		haveLocal = haveLocal || (o.Tier == "local" && o.Model == "qwen-coder")
		haveSonnet = haveSonnet || (o.Tier == "strong" && o.Model == "sonnet")
		haveOpus = haveOpus || (o.Tier == "strong" && o.Model == "opus")
	}
	if !haveLocal || !haveSonnet || !haveOpus {
		t.Fatalf("options missing models: %+v", opts)
	}

	// While the picker is open the screen shows it, and list keys do not leak through.
	if out := StripAnsi(Render(*st, 80, 24)); !strings.Contains(out, "ASSIGN MODEL: 01C") || !strings.Contains(out, "strong (claude-cli): opus") {
		t.Errorf("picker not rendered:\n%s", out)
	}
	act(st, fq, ActionQueueUncheckAll)
	act(st, fq, ActionToggle) // Space confirms inside the picker; it must not toggle the chunk
	for _, c := range fq.calls {
		if strings.HasPrefix(c, "all") || strings.HasPrefix(c, "active") {
			t.Fatalf("queue-changing key leaked through the picker: %v", fq.calls)
		}
	}
	// (Space above confirmed the highlighted "auto" option.)
	if st.QueuePicker {
		t.Fatal("picker should be closed after confirming")
	}

	fq.calls = nil
	act(st, fq, ActionModelCycle)
	for i, o := range queueModelOptions(st) {
		if o.Tier == "strong" && o.Model == "opus" {
			st.QueuePickerIdx = i
		}
	}
	act(st, fq, ActionConfirm)
	if got := strings.Join(fq.calls, ";"); got != "assign 01C strong opus" {
		t.Fatalf("calls = %q", got)
	}
	if got := selectedChunk(st); got.PinTier != "strong" || got.PinModel != "opus" {
		t.Errorf("list not reloaded: %+v", got)
	}
	if !strings.Contains(StripAnsi(Render(*st, 100, 30)), "strong / opus") {
		t.Error("assignment should show in the list")
	}

	// Esc backs out without touching anything.
	fq.calls = nil
	act(st, fq, ActionModelCycle)
	act(st, fq, ActionCancel)
	if st.QueuePicker || len(fq.calls) != 0 {
		t.Errorf("cancel should close the picker silently: picker=%v calls=%v", st.QueuePicker, fq.calls)
	}

	// [c] returns to automatic routing.
	act(st, fq, ActionUsageCap)
	if got := strings.Join(fq.calls, ";"); got != "assign 01C  " {
		t.Errorf("clear calls = %q", got)
	}
}

func TestTUI_QueueTab_PickerStartsOnCurrentAssignmentAndRefusesRunning(t *testing.T) {
	st, fq := queueFixture()
	st.StackItems[1].Models = []string{"sonnet", "claude-opus-5-5"}
	selectID(st, "01F")
	act(st, fq, ActionModelCycle)
	if o := queueModelOptions(st)[st.QueuePickerIdx]; o.Tier != "strong" || o.Model != "claude-opus-5-5" {
		t.Errorf("picker should open on the current assignment, got %+v", o)
	}
	act(st, fq, ActionCancel)

	selectID(st, "01B")
	act(st, fq, ActionModelCycle)
	if st.QueuePicker || !strings.Contains(st.ActionErr, "running") {
		t.Errorf("assigning a running chunk must be refused: picker=%v err=%q", st.QueuePicker, st.ActionErr)
	}

	st.StackItems = nil
	selectID(st, "01C")
	act(st, fq, ActionModelCycle)
	if st.QueuePicker || !strings.Contains(st.ActionErr, "Stack tab") {
		t.Errorf("no tiers: picker=%v err=%q", st.QueuePicker, st.ActionErr)
	}
}

func TestTUI_QueueTab_StackEditingKeysDoNothingHere(t *testing.T) {
	st, fq := queueFixture()
	st.ConfigPath = "" // nothing may be written either
	before := append([]StackItem(nil), st.StackItems...)
	for _, a := range []KeyAction{ActionToggle, ActionPrioUp, ActionPrioDown, ActionModelCycle, ActionUsageCap, ActionFloor, ActionRoutingRule, ActionBudgetCap, ActionHumanGate, ActionQuickTest, ActionQuickTestAll} {
		st.QueuePicker = false
		ExecuteAction(context.Background(), nil, st, a)
		for i := range before {
			if st.StackItems[i].Enabled != before[i].Enabled || st.StackItems[i].Model != before[i].Model || st.StackItems[i].Priority != before[i].Priority || st.StackItems[i].UsageCapPct != before[i].UsageCapPct {
				t.Fatalf("action %v on the queue tab edited stack tier %d: %+v", a, i, st.StackItems[i])
			}
		}
	}
	_ = fq
}

func TestTUI_QueueTab_OfflineIsHandled(t *testing.T) {
	st, _ := queueFixture()
	selectID(st, "01C")
	for _, a := range []KeyAction{ActionToggle, ActionQueueCheckAll, ActionQueueUncheckAll, ActionQueueRequeue, ActionFloor, ActionUsageCap} {
		st.ActionErr = ""
		ExecuteAction(context.Background(), nil, st, a) // nil client: daemon offline
		if a != ActionQueueRequeue && st.ActionErr == "" {
			t.Errorf("action %v offline should explain itself", a)
		}
	}
	selectID(st, "01A")
	ExecuteAction(context.Background(), nil, st, ActionQueueRequeue)
	if !strings.Contains(st.ActionErr, "offline") {
		t.Errorf("requeue offline: %q", st.ActionErr)
	}
}

func TestTUI_QueueTab_GlobalControlsStillWork(t *testing.T) {
	st, _ := queueFixture()
	if !ExecuteAction(context.Background(), nil, st, ActionQuit) {
		t.Error("q must still quit from the queue tab")
	}
	st.QueuePicker = true
	if !ExecuteAction(context.Background(), nil, st, ActionQuit) {
		t.Error("quit must work with the picker open too")
	}
	st.QueuePicker = false
	ExecuteAction(context.Background(), nil, st, ActionTab1)
	if st.ActiveTab != TabOverview {
		t.Error("[1] must leave the queue tab")
	}
}

func TestTUI_QueueTabIsAlwaysInTheTabBar(t *testing.T) {
	for _, tab := range []DashboardTab{TabOverview, TabLogs, TabStack, TabQueue} {
		for _, w := range []int{60, 80, 120} {
			if out := StripAnsi(Render(DashboardState{ActiveTab: tab}, w, 24)); !strings.Contains(out, "[4] QUEUE") {
				t.Errorf("tab %v, width %d: the [4] QUEUE tab is missing from the tab bar", tab, w)
			}
		}
	}
}

func visibleIDs(st *DashboardState) string {
	var ids []string
	for _, c := range visibleChunks(st) {
		ids = append(ids, c.ID)
	}
	return strings.Join(ids, ",")
}

func TestTUI_QueueTab_FiltersAndHideDone(t *testing.T) {
	st, fq := queueFixture()
	st.Chunks = append(st.Chunks, db.ChunkRecord{ID: "01G", Title: "Skipped thing", Status: "skipped"})
	all := visibleIDs(st)

	act(st, fq, ActionQueueFilter) // all -> blocked
	if got := visibleIDs(st); got != "01E" {
		t.Fatalf("blocked filter shows %q", got)
	}
	act(st, fq, ActionQueueFilter) // blocked -> skipped
	if got := visibleIDs(st); got != "01G" {
		t.Fatalf("skipped filter shows %q", got)
	}
	if ch := selectedChunk(st); ch == nil || ch.ID != "01G" {
		t.Fatalf("selection should land on a visible row, got %+v", ch)
	}
	act(st, fq, ActionQueueFilter) // skipped -> all
	if got := visibleIDs(st); got != all {
		t.Fatalf("all filter shows %q, want %q", got, all)
	}

	act(st, fq, ActionQueueHideDone)
	if got := visibleIDs(st); strings.Contains(got, "01A") || !strings.Contains(got, "01C") {
		t.Fatalf("hide-done shows %q", got)
	}
	// The hide-done toggle is independent of the status filter.
	act(st, fq, ActionQueueFilter)
	if got := visibleIDs(st); got != "01E" || !st.QueueHideDone {
		t.Fatalf("filter + hide-done shows %q hide=%v", got, st.QueueHideDone)
	}
	act(st, fq, ActionQueueHideDone)
	if st.QueueHideDone {
		t.Fatal("second press should show done chunks again")
	}

	// A filter with no matches is explained rather than shown as an empty queue.
	st.Chunks = st.Chunks[:2]
	out := StripAnsi(Render(*st, 100, 30))
	if !strings.Contains(out, "No chunks match the current filter") {
		t.Errorf("empty filter result not explained:\n%s", out)
	}
}
