package tui

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/eventlog"
)

func TestTUI_RenderGauge(t *testing.T) {
	tests := []struct {
		pct      int
		barWidth int
		contains string
	}{
		{0, 10, "0%"},
		{50, 10, "50%"},
		{100, 10, "100%"},
		{-5, 10, "0%"},
		{150, 10, "100%"},
	}

	for _, tt := range tests {
		gauge := RenderGauge(tt.pct, tt.barWidth)
		plain := StripAnsi(gauge)
		if !strings.Contains(plain, tt.contains) {
			t.Errorf("RenderGauge(%d) = %q; want %q", tt.pct, plain, tt.contains)
		}
	}
}

func TestTUI_Box(t *testing.T) {
	lines := []string{"Hello", "World"}
	box := Box("TEST BOX", lines, 40)
	if len(box) != 4 {
		t.Fatalf("expected 4 box lines, got %d", len(box))
	}
	top := StripAnsi(box[0])
	if !strings.Contains(top, "TEST BOX") {
		t.Errorf("expected top line to contain title, got: %s", top)
	}
	bot := StripAnsi(box[3])
	if !strings.HasPrefix(bot, "└") || !strings.HasSuffix(bot, "┘") {
		t.Errorf("malformed bottom border: %s", bot)
	}
}

func TestTUI_Render_AllStates(t *testing.T) {
	states := []struct {
		name       string
		state      string
		drain      bool
		lockdown   bool
		runningID  string
		wantSubstr []string
	}{
		{
			name:       "idle",
			state:      "idle",
			wantSubstr: []string{"IDLE", "Worker idle", "Summary:"},
		},
		{
			name:       "running",
			state:      "running",
			runningID:  "03A",
			wantSubstr: []string{"RUNNING", "Running Chunk: 03A"},
		},
		{
			name:       "paused",
			state:      "paused",
			wantSubstr: []string{"PAUSED"},
		},
		{
			name:       "draining",
			state:      "draining",
			drain:      true,
			wantSubstr: []string{"DRAINING", "ACTIVE"},
		},
		{
			name:       "lockdown",
			state:      "lockdown",
			lockdown:   true,
			wantSubstr: []string{"LOCKDOWN"},
		},
	}

	for _, tt := range states {
		t.Run(tt.name, func(t *testing.T) {
			st := DashboardState{
				Status: control.DaemonStatus{
					State:          tt.state,
					RunningChunkID: tt.runningID,
					WorkerID:       "worker-test-1",
					DrainMode:      tt.drain,
					Lockdown:       tt.lockdown,
					QueuePending:   3,
					QueueRunning:   1,
					QueueBlocked:   0,
					QueueCompleted: 5,
				},
				Chunks: []db.ChunkRecord{
					{ID: "01A", Title: "Setup", Status: "done"},
					{ID: "02A", Title: "Core", Status: "pending"},
				},
				AuditEvents: []eventlog.Event{
					{Seq: 1, Timestamp: time.Now(), Type: "daemon.started", Payload: "init"},
				},
			}

			out := Render(st, 80, 24)
			plain := StripAnsi(out)

			for _, want := range tt.wantSubstr {
				if !strings.Contains(plain, want) {
					t.Errorf("Render() for %s missing substring %q\nRendered:\n%s", tt.name, want, plain)
				}
			}

			// Ensure standard hotkeys are always rendered in footer
			if !strings.Contains(plain, "[p] Pause") || !strings.Contains(plain, "[q] Quit") {
				t.Errorf("Render() missing hotkey footer: %s", plain)
			}
		})
	}
}

func TestTUI_Run_Cancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var in bytes.Buffer
	var out bytes.Buffer

	client := control.NewClient("/dev/null/dummy.sock", "")

	// Cancel context after short duration
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := Run(ctx, client, &in, &out, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected Run error: %v", err)
	}

	if out.Len() == 0 {
		t.Fatal("expected at least one frame rendered to out")
	}
}

func TestTUI_Render_Tabs(t *testing.T) {
	st := DashboardState{
		Status: control.DaemonStatus{State: "running"},
		AuditEvents: []eventlog.Event{
			{Seq: 1, Type: "chunk.started", Actor: "worker-1", Payload: "chunk 01A started"},
			{Seq: 2, Type: "chunk.passed", Actor: "worker-1", Payload: "chunk 01A passed verification"},
		},
		TopLines:   []string{"Tasks: 200 total", "Cpu: 5% user"},
		LlamaLines: []string{"GPUs discovered: 1", "util=5.0%"},
		NvtopLines: []string{"GeForce RTX 2080 Ti", "Memory: 200MiB"},
	}

	// 1. Overview tab
	st.ActiveTab = TabOverview
	out := StripAnsi(Render(st, 80, 24))
	if !strings.Contains(out, "[1] OVERVIEW") || !strings.Contains(out, "QUOTA GAUGES") {
		t.Errorf("Overview tab render missing expected sections: %s", out)
	}

	// 2. Logs tab
	st.ActiveTab = TabLogs
	out = StripAnsi(Render(st, 80, 24))
	if !strings.Contains(out, "[2] LOGS") || !strings.Contains(out, "chunk 01A passed verification") {
		t.Errorf("Logs tab render missing expected log output: %s", out)
	}

	// 3. Stack & Rules tab
	st.ActiveTab = TabStack
	st.StackItems = []StackItem{
		{Tier: "local", Adapter: "opencode", Model: "qwen-coder", URL: "http://127.0.0.1:8080/v1", Enabled: true, Priority: 1, Attempts: 3, UsageCapPct: 100},
		{Tier: "sonnet", Adapter: "agy", Model: "sonnet", Pool: "claude", Enabled: true, Priority: 2, Attempts: 2, FloorPct: 25, UsageCapPct: 80},
	}
	out = StripAnsi(Render(st, 80, 24))
	if !strings.Contains(out, "[3] STACK & RULES") || !strings.Contains(out, "AI ROUTER & CLI PRIORITY STACK") || !strings.Contains(out, "local:3 -> sonnet:2") {
		t.Errorf("Stack tab render missing expected stack sections:\n%s", out)
	}
}

func TestTUI_StackPrioritizationCapsAndPersistence(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := tmp + "/shepherd.toml"
	initialTOML := `schema_version = 1

[project]
name = "tui-test"
repo = "."
base_branch = "main"

[queue]
source = "markdown-dir"
root = "docs/plan"
done_marker = "move-to-done"

[verify]
gates = [{ name = "scope", builtin = "scope", hard = true }]

[repair]
chain = ["local:3", "sonnet:2"]
mechanical_fix = { tier = "sonnet", max_errors = 3, codes = ["E1"] }

[tiers.local]
adapter = "opencode"
model = "model-1"
url = "http://127.0.0.1:8080/v1"
pool = "local"

[tiers.sonnet]
adapter = "agy"
model = "sonnet"
pool = "claude"

[quota.pools.local]
mode = "until-rejected"
backoff_min = [10, 30]

[quota.pools.claude]
mode = "polled"
usage_cmd = ["agy", "-p", "/usage"]
parser = "agy-usage"
floor_weekly_pct = 25

[decide]
backend = "rules"
timeout_ms = 2000

[policy]
max_consecutive_blocks = 3
wait_for_reset_max_min = 45
cloud_first_threshold = 0.5
`
	if err := writeTestFile(cfgPath, initialTOML); err != nil {
		t.Fatal(err)
	}

	st := DashboardState{
		ConfigPath: cfgPath,
		ActiveTab:  TabStack,
	}
	SyncStackFromState(&st)
	if len(st.StackItems) != 2 {
		t.Fatalf("expected 2 stack items loaded from shepherd.toml, got %d", len(st.StackItems))
	}

	ctx := context.Background()
	// Move item #1 (local) DOWN in priority ('d' on TabStack)
	HandleKey(ctx, nil, &st, 'd')
	if st.StackItems[0].Tier != "sonnet" || st.StackItems[1].Tier != "local" {
		t.Errorf("expected sonnet #1 and local #2 after move down, got %+v", st.StackItems)
	}

	// Cycle usage cap on selected item (local, now at index 1): 100% -> 80%
	HandleKey(ctx, nil, &st, 'c')
	if st.StackItems[1].UsageCapPct != 80 {
		t.Errorf("expected UsageCapPct 80, got %d", st.StackItems[1].UsageCapPct)
	}

	// Toggle local OFF (' ')
	HandleKey(ctx, nil, &st, ' ')
	if st.StackItems[1].Enabled {
		t.Errorf("expected local tier to be deactivated")
	}

	// Cycle routing rule ('g') and budget cap ('b')
	HandleKey(ctx, nil, &st, 'g')
	if st.Rules.CloudFirstThreshold != 0.75 {
		t.Errorf("expected CloudFirstThreshold 0.75, got %v", st.Rules.CloudFirstThreshold)
	}
	HandleKey(ctx, nil, &st, 'b')
	if st.Rules.MaxSprintCostUSD != 5.0 {
		t.Errorf("expected MaxSprintCostUSD 5.0, got %v", st.Rules.MaxSprintCostUSD)
	}

	// Reload from disk to verify persistence in shepherd.toml
	stReload := DashboardState{ConfigPath: cfgPath}
	SyncStackFromState(&stReload)
	if stReload.StackItems[0].Tier != "sonnet" {
		t.Errorf("expected persisted #1 tier sonnet, got %s", stReload.StackItems[0].Tier)
	}
	if stReload.StackItems[1].Enabled {
		t.Errorf("expected persisted local tier to remain disabled")
	}
	if stReload.StackItems[1].UsageCapPct != 80 {
		t.Errorf("expected persisted local usage cap 80%%, got %d", stReload.StackItems[1].UsageCapPct)
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

func TestTUI_Monitors(t *testing.T) {
	// Top snapshot test
	topLines := GetTopSnapshot()
	if len(topLines) == 0 {
		t.Errorf("expected non-empty top snapshot")
	}

	// LlamaGpuTop script lookup test
	script := FindLlamaGpuTopScript()
	if script == "" {
		t.Logf("FindLlamaGpuTopScript returned empty (might not be in standard root)")
	}

	// Nvtop snapshot test
	nvLines := GetNvtopSnapshot()
	if len(nvLines) == 0 {
		t.Errorf("expected non-empty nvtop snapshot")
	}
}

func TestTUI_Stack_CandidateModels_AgyGemini(t *testing.T) {
	models := defaultCandidateModels("agy", "")
	foundFlash := false
	foundArgon := false
	for _, m := range models {
		if strings.Contains(m, "flash") {
			foundFlash = true
		}
		if strings.Contains(m, "argon") {
			foundArgon = true
		}
	}
	if !foundFlash || !foundArgon {
		t.Errorf("expected defaultCandidateModels for agy to include flash and argon, got %v", models)
	}

	st := DashboardState{
		StackItems: []StackItem{
			{
				Tier:     "flash",
				Adapter:  "agy",
				Model:    models[0],
				Models:   models,
				Enabled:  true,
				Priority: 1,
			},
		},
		SelectedStackIdx: 0,
	}

	CycleStackModel(t.Context(), nil, &st)
	if st.StackItems[0].Model != models[1] {
		t.Errorf("expected model cycled to %s, got %s", models[1], st.StackItems[0].Model)
	}
}

func TestTUI_InputEscapeSequencesAndMouseIgnored(t *testing.T) {
	// 1. Mouse reporting sequences must be discarded and NOT trigger actions
	mouseSeq := []byte("\x1b[<35;24;12M\x1b[<0;10;5m\x1b[M@!#")
	acts := parseInputBytes(mouseSeq)
	if len(acts) != 0 {
		t.Errorf("expected mouse escape sequences to produce 0 actions, got %d: %+v", len(acts), acts)
	}

	// 2. Arrow keys must map to navigation actions, not bracket tab-switch
	upSeq := []byte("\x1b[A")
	downSeq := []byte("\x1b[B")
	rightSeq := []byte("\x1b[C")
	leftSeq := []byte("\x1b[D")
	shiftTabSeq := []byte("\x1b[Z")

	if acts := parseInputBytes(upSeq); len(acts) != 1 || acts[0] != ActionUp {
		t.Errorf("Up arrow expected ActionUp, got %+v", acts)
	}
	if acts := parseInputBytes(downSeq); len(acts) != 1 || acts[0] != ActionDown {
		t.Errorf("Down arrow expected ActionDown, got %+v", acts)
	}
	if acts := parseInputBytes(rightSeq); len(acts) != 1 || acts[0] != ActionTabNext {
		t.Errorf("Right arrow expected ActionTabNext, got %+v", acts)
	}
	if acts := parseInputBytes(leftSeq); len(acts) != 1 || acts[0] != ActionTabPrev {
		t.Errorf("Left arrow expected ActionTabPrev, got %+v", acts)
	}
	if acts := parseInputBytes(shiftTabSeq); len(acts) != 1 || acts[0] != ActionTabPrev {
		t.Errorf("Shift-Tab expected ActionTabPrev, got %+v", acts)
	}

	// 3. Tab navigation state transitions
	st := DashboardState{ActiveTab: TabOverview}
	ExecuteAction(context.Background(), nil, &st, ActionTabNext)
	if st.ActiveTab != TabLogs {
		t.Errorf("expected TabLogs after ActionTabNext, got %v", st.ActiveTab)
	}
	ExecuteAction(context.Background(), nil, &st, ActionTabNext)
	if st.ActiveTab != TabStack {
		t.Errorf("expected TabStack after ActionTabNext, got %v", st.ActiveTab)
	}
	ExecuteAction(context.Background(), nil, &st, ActionTabPrev)
	if st.ActiveTab != TabLogs {
		t.Errorf("expected TabLogs after ActionTabPrev, got %v", st.ActiveTab)
	}
}

func TestTUI_RenderLinesFitTerminalHeight(t *testing.T) {
	st := DashboardState{
		Status: control.DaemonStatus{
			State:        "running",
			WorkerID:     "worker-1",
			QueuePending: 10,
		},
		Chunks: []db.ChunkRecord{
			{ID: "01A", Title: "Task 1", Status: "running"},
			{ID: "02A", Title: "Task 2", Status: "pending"},
			{ID: "03A", Title: "Task 3", Status: "pending"},
			{ID: "04A", Title: "Task 4", Status: "pending"},
			{ID: "05A", Title: "Task 5", Status: "pending"},
		},
		AuditEvents: []eventlog.Event{
			{Seq: 1, Type: "a", Payload: "1"},
			{Seq: 2, Type: "b", Payload: "2"},
			{Seq: 3, Type: "c", Payload: "3"},
			{Seq: 4, Type: "d", Payload: "4"},
			{Seq: 5, Type: "e", Payload: "5"},
			{Seq: 6, Type: "f", Payload: "6"},
		},
		StackItems: []StackItem{
			{Tier: "t1", Adapter: "a1", Model: "m1", Enabled: true},
			{Tier: "t2", Adapter: "a2", Model: "m2", Enabled: true},
			{Tier: "t3", Adapter: "a3", Model: "m3", Enabled: true},
			{Tier: "t4", Adapter: "a4", Model: "m4", Enabled: true},
			{Tier: "t5", Adapter: "a5", Model: "m5", Enabled: true},
		},
	}

	for _, tab := range []DashboardTab{TabOverview, TabLogs, TabStack, TabQueue} {
		st.ActiveTab = tab
		rendered := Render(st, 80, 24)
		lines := strings.Split(rendered, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}

		if len(lines) > 24 {
			t.Errorf("Tab %v rendered %d lines, exceeding standard terminal height 24", tab, len(lines))
		}

		for lineIdx, line := range lines {
			vl := VisibleLen(line)
			if vl > 80 {
				t.Errorf("Tab %v line %d visible width %d exceeds width 80: %q", tab, lineIdx, vl, line)
			}
		}
	}
}

func TestTUI_TruncateAnsi(t *testing.T) {
	colored := "\x1b[32mHelloWorld\x1b[0m"
	trunc := TruncateAnsi(colored, 5)
	if VisibleLen(trunc) != 5 {
		t.Errorf("TruncateAnsi visible length = %d; want 5", VisibleLen(trunc))
	}
	plain := StripAnsi(trunc)
	if plain != "Hello" {
		t.Errorf("TruncateAnsi plain text = %q; want %q", plain, "Hello")
	}
}




// The ntfy topic (the key you subscribe to, and send commands on) is the last line of the
// Overview tab, and survives the height clamp on a small terminal.
func TestTUI_Render_NtfyTopicAtBottomOfOverview(t *testing.T) {
	st := DashboardState{ActiveTab: TabOverview}
	st.Status.State = "running"
	st.Status.NtfyTopic = "https://ntfy.sh/example-batch-AAAAAAAAAAAAAAAA"
	for _, h := range []int{24, 40} {
		lines := strings.Split(strings.TrimRight(StripAnsi(Render(st, 80, h)), "\n"), "\n")
		last := lines[len(lines)-1]
		if !strings.Contains(last, "ntfy.sh/example-batch-AAAAAAAAAAAAAAAA") {
			t.Fatalf("height %d: last line %q should show the ntfy topic", h, last)
		}
		if len(lines) > h {
			t.Fatalf("height %d: rendered %d lines", h, len(lines))
		}
	}

	st.ActiveTab = TabLogs
	if strings.Contains(StripAnsi(Render(st, 80, 24)), "example-batch") {
		t.Fatal("topic should only be on the Overview tab")
	}

	st.ActiveTab = TabOverview
	st.Status.NtfyTopic = ""
	if out := StripAnsi(Render(st, 80, 24)); !strings.Contains(out, "ntfy: not configured") {
		t.Fatalf("missing not-configured hint:\n%s", out)
	}
}

// Rotating the ntfy topic cuts off every subscribed phone, so it needs a second press to confirm.
func TestTUI_NtfyRotateNeedsDoublePress(t *testing.T) {
	st := &DashboardState{ActiveTab: TabOverview}
	if mapByteToAction('n') != ActionNtfyRotate {
		t.Fatal("n should map to ntfy rotate")
	}
	ExecuteAction(context.Background(), nil, st, ActionNtfyRotate)
	if !strings.Contains(st.LastAction, "press n again") {
		t.Fatalf("first press should arm, got %q", st.LastAction)
	}
	st.ntfyRotateArmed = time.Now().Add(-10 * time.Second) // confirmation window expired
	ExecuteAction(context.Background(), nil, st, ActionNtfyRotate)
	if !strings.Contains(st.LastAction, "press n again") {
		t.Fatalf("expired arm should re-arm, got %q", st.LastAction)
	}
}
