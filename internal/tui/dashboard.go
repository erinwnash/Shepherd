package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/eventlog"
	"github.com/erinwnash/Shepherd/internal/quota"
	"golang.org/x/term"
)

// KeyAction represents a parsed semantic user action from the terminal.
type KeyAction int

const (
	ActionNone KeyAction = iota
	ActionQuit
	ActionTab1
	ActionTab2
	ActionTab3
	ActionTab4
	ActionTabNext
	ActionTabPrev
	ActionUp
	ActionDown
	ActionToggle
	ActionPrioUp
	ActionPrioDown
	ActionModelCycle
	ActionUsageCap
	ActionFloor
	ActionRoutingRule
	ActionBudgetCap
	ActionHumanGate
	ActionQuickTest
	ActionQuickTestAll
	ActionPause
	ActionResume
	ActionHalt
	ActionDrain
	ActionLockdown
	ActionUnlock
	ActionNtfyRotate
	ActionConfirm
	ActionCancel
	ActionQueueRequeue
	ActionQueueCheckAll
	ActionQueueUncheckAll
	ActionQueueFilter
	ActionQueueHideDone
	ActionQRToggle
	ActionFrontRun
)

// Run executes the live interactive dashboard loop starting on the overview tab.
func Run(ctx context.Context, client *control.Client, in io.Reader, out io.Writer, pollInterval time.Duration) error {
	return RunWithTab(ctx, client, in, out, pollInterval, TabOverview)
}

// RunWithTab executes the live interactive dashboard loop starting on the specified tab.
func RunWithTab(ctx context.Context, client *control.Client, in io.Reader, out io.Writer, pollInterval time.Duration, initialTab DashboardTab) error {
	if pollInterval <= 0 {
		pollInterval = 1 * time.Second
	}

	// Detect if running in an interactive terminal
	isTerm := false
	var oldState *term.State
	if file, ok := in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		isTerm = true
		rawState, err := term.MakeRaw(int(file.Fd()))
		if err == nil {
			oldState = rawState
			defer func() {
				_ = term.Restore(int(file.Fd()), oldState)
				// Disable mouse reporting, focus reporting, bracketed paste, restore alt screen and cursor
				fmt.Fprint(out, "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1006l\x1b[?2004l"+AltScreenOff+CursorShow)
			}()
			// Enter alt screen, hide cursor, and proactively disable mouse/focus reporting
			fmt.Fprint(out, AltScreenOn+CursorHide+"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1006l\x1b[?2004l")
		}
	}

	state := DashboardState{
		Timestamp: time.Now(),
		ActiveTab: initialTab,
	}

	// Keypress channel
	keyCh := make(chan KeyAction, 16)
	if isTerm {
		go func() {
			buf := make([]byte, 128)
			for {
				n, err := in.Read(buf)
				if err != nil || n == 0 {
					return
				}
				actions := parseInputBytes(buf[:n])
				for _, act := range actions {
					keyCh <- act
				}
			}
		}()
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Quota usage commands (e.g. `agy -p /usage`) can take seconds; poll them off the UI loop.
	qp := newQuotaPoller()

	// Initial fetch
	fetchState(ctx, client, &state)
	qp.apply(&state)
	renderScreen(out, state)

	for {
		select {
		case <-ctx.Done():
			return nil

		case act := <-keyCh:
			prevTab := state.ActiveTab
			if ExecuteAction(ctx, client, &state, act) {
				return nil
			}

			// Moving the cursor needs no round trip; anything else refreshes immediately.
			if !(isCursorMove(act) && state.ActiveTab == prevTab) {
				fetchState(ctx, client, &state)
			}
			qp.apply(&state)
			renderScreen(out, state)

		case <-ticker.C:
			fetchState(ctx, client, &state)
			qp.apply(&state)
			renderScreen(out, state)
		}
	}
}

func parseInputBytes(data []byte) []KeyAction {
	var actions []KeyAction
	i := 0
	for i < len(data) {
		b := data[i]
		if b == 0x1b { // ESC
			i++
			if i >= len(data) {
				// Standalone ESC
				actions = append(actions, ActionCancel)
				break
			}
			if data[i] == '[' {
				// CSI sequence: advance to terminating character (0x40 - 0x7E)
				start := i + 1
				i++
				for i < len(data) && (data[i] < 0x40 || data[i] > 0x7E) {
					i++
				}
				if i < len(data) {
					termChar := data[i]
					seq := string(data[start:i])
					switch termChar {
					case 'A': // Up arrow
						actions = append(actions, ActionUp)
					case 'B': // Down arrow
						actions = append(actions, ActionDown)
					case 'C': // Right arrow
						actions = append(actions, ActionTabNext)
					case 'D': // Left arrow
						actions = append(actions, ActionTabPrev)
					case 'Z': // Shift-Tab
						actions = append(actions, ActionTabPrev)
					case '~':
						if seq == "5" {
							actions = append(actions, ActionUp) // Page Up
						} else if seq == "6" {
							actions = append(actions, ActionDown) // Page Down
						}
					default:
						// Mouse events (\x1b[<...M), focus (\x1b[I, \x1b[O), queries (\x1b[?...)
						// Safely consumed and discarded!
					}
					i++ // Consume terminating char
				}
			} else if data[i] == 'O' {
				// SS3 sequence: \x1bOA, \x1bOB, etc.
				i++
				if i < len(data) {
					switch data[i] {
					case 'A':
						actions = append(actions, ActionUp)
					case 'B':
						actions = append(actions, ActionDown)
					case 'C':
						actions = append(actions, ActionTabNext)
					case 'D':
						actions = append(actions, ActionTabPrev)
					}
					i++
				}
			} else {
				i++
			}
			continue
		}

		act := mapByteToAction(b)
		if act != ActionNone {
			actions = append(actions, act)
		}
		i++
	}
	return actions
}

func mapByteToAction(b byte) KeyAction {
	switch b {
	case 'q', 'Q', 3: // 'q' or Ctrl+C
		return ActionQuit
	case '1':
		return ActionTab1
	case '2':
		return ActionTab2
	case '3':
		return ActionTab3
	case '4':
		return ActionTab4
	case '\r', '\n':
		return ActionConfirm
	case 'x', 'X':
		return ActionQueueRequeue
	case 'a', 'A':
		return ActionQueueCheckAll
	case 'z', 'Z':
		return ActionQueueUncheckAll
	case 'o', 'O':
		return ActionQueueFilter
	case 'w', 'W':
		return ActionQueueHideDone
	case 'i', 'I':
		return ActionQRToggle
	case 'y', 'Y':
		return ActionFrontRun
	case '\t':
		return ActionTabNext
	case 's', 'S':
		return ActionTab3
	case 'j', 'J':
		return ActionDown
	case 'k', 'K':
		return ActionUp
	case ' ', 'e', 'E':
		return ActionToggle
	case '+', '=':
		return ActionPrioUp
	case '-', '_':
		return ActionPrioDown
	case 'm', 'M':
		return ActionModelCycle
	case 'c', 'C':
		return ActionUsageCap
	case 'f', 'F':
		return ActionFloor
	case 'g', 'G':
		return ActionRoutingRule
	case 'b', 'B':
		return ActionBudgetCap
	case 'v', 'V':
		return ActionHumanGate
	case 't':
		return ActionQuickTest
	case 'T':
		return ActionQuickTestAll
	case 'p', 'P':
		return ActionPause
	case 'r', 'R':
		return ActionResume
	case 'h', 'H':
		return ActionHalt
	case 'd', 'D':
		return ActionDrain
	case 'u', 'U':
		return ActionUnlock
	case 'l', 'L':
		return ActionLockdown
	case 'n', 'N':
		return ActionNtfyRotate
	default:
		return ActionNone
	}
}

// HandleKey processes a single keypress in the TUI dashboard and returns true if the dashboard should exit.
func HandleKey(ctx context.Context, client *control.Client, state *DashboardState, k byte) bool {
	act := mapByteToAction(k)
	return ExecuteAction(ctx, client, state, act)
}

// ExecuteAction performs the state update or control action associated with act.
func ExecuteAction(ctx context.Context, client *control.Client, state *DashboardState, act KeyAction) bool {
	if handled, quit := handleFrontRunAction(ctx, frontRunClient(client), state, act); handled {
		return quit
	}
	if state.ActiveTab == TabQueue {
		if handled, quit := handleQueueAction(ctx, queueClient(client), state, act); handled {
			return quit
		}
	}
	switch act {
	case ActionQuit:
		return true
	case ActionTab1:
		state.ActiveTab = TabOverview
	case ActionTab2:
		state.ActiveTab = TabLogs
	case ActionTab3:
		state.ActiveTab = TabStack
	case ActionTab4:
		state.ActiveTab = TabQueue
	case ActionTabNext:
		state.ActiveTab = (state.ActiveTab + 1) % tabCount
	case ActionTabPrev:
		state.ActiveTab = (state.ActiveTab + tabCount - 1) % tabCount
	case ActionDown:
		if state.ActiveTab == TabStack && len(state.StackItems) > 0 {
			state.SelectedStackIdx = (state.SelectedStackIdx + 1) % len(state.StackItems)
		}
	case ActionUp:
		if state.ActiveTab == TabStack && len(state.StackItems) > 0 {
			state.SelectedStackIdx = (state.SelectedStackIdx - 1 + len(state.StackItems)) % len(state.StackItems)
		}
	case ActionToggle:
		ToggleStackItem(ctx, client, state)
	case ActionPrioUp:
		MoveStackPriority(ctx, client, state, -1)
	case ActionPrioDown:
		MoveStackPriority(ctx, client, state, 1)
	case ActionModelCycle:
		CycleStackModel(ctx, client, state)
	case ActionUsageCap:
		CycleStackUsageCap(ctx, client, state)
	case ActionFloor:
		CycleStackFloor(ctx, client, state)
	case ActionRoutingRule:
		CycleRoutingRule(ctx, client, state)
	case ActionBudgetCap:
		CycleBudgetCap(ctx, client, state)
	case ActionHumanGate:
		ToggleHumanGate(ctx, client, state)
	case ActionQuickTest:
		QuickTestSelected(ctx, state)
	case ActionQuickTestAll:
		QuickTestAll(ctx, state)
	case ActionPause:
		if client != nil {
			if err := client.Pause(ctx); err != nil {
				state.ActionErr = fmt.Sprintf("pause failed: %v", err)
			} else {
				state.LastAction = "paused"
				state.ActionErr = ""
			}
		}
	case ActionResume:
		if client != nil {
			if err := client.Resume(ctx); err != nil {
				state.ActionErr = fmt.Sprintf("resume failed: %v", err)
			} else {
				state.LastAction = "resumed"
				state.ActionErr = ""
			}
		}
	case ActionHalt:
		if client != nil {
			if err := client.Halt(ctx, "operator halt from TUI"); err != nil {
				state.ActionErr = fmt.Sprintf("halt failed: %v", err)
			} else {
				state.LastAction = "halted and rolled back"
				state.ActionErr = ""
			}
		}
	case ActionDrain:
		if state.ActiveTab == TabStack {
			MoveStackPriority(ctx, client, state, 1)
		} else if client != nil {
			newDrain := !state.Status.DrainMode
			if err := client.Drain(ctx, newDrain, nil); err != nil {
				state.ActionErr = fmt.Sprintf("drain toggle failed: %v", err)
			} else {
				state.LastAction = fmt.Sprintf("drain mode set to %v", newDrain)
				state.ActionErr = ""
			}
		}
	case ActionLockdown:
		if client != nil {
			if err := client.Lockdown(ctx); err != nil {
				state.ActionErr = fmt.Sprintf("lockdown failed: %v", err)
			} else {
				state.LastAction = "lockdown engaged"
				state.ActionErr = ""
			}
		}
	case ActionQRToggle:
		state.QRHidden = !state.QRHidden
		if state.QRHidden {
			state.LastAction = "ntfy QR code hidden"
		} else {
			state.LastAction = "ntfy QR code shown"
		}
		state.ActionErr = ""
	case ActionNtfyRotate:
		// Rotation disconnects every phone subscribed to the old topic: require a second press.
		if state.ntfyRotateArmed.IsZero() || time.Since(state.ntfyRotateArmed) > ntfyRotateWindow {
			state.ntfyRotateArmed = time.Now()
			state.LastAction = "rotate ntfy topic? press n again within 5s (phones must re-subscribe to the new topic)"
			state.ActionErr = ""
			break
		}
		state.ntfyRotateArmed = time.Time{}
		if client != nil {
			topic, err := client.RotateNtfyTopic(ctx)
			if err != nil {
				state.ActionErr = fmt.Sprintf("ntfy rotate failed: %v", err)
			} else {
				state.Status.NtfyTopic = topic
				state.LastAction = "ntfy topic rotated: subscribe to " + displayTopic(topic)
				state.ActionErr = ""
			}
		}
	case ActionUnlock:
		if state.ActiveTab == TabStack {
			MoveStackPriority(ctx, client, state, -1)
		} else if client != nil {
			if err := client.Unlock(ctx); err != nil {
				state.ActionErr = fmt.Sprintf("unlock failed: %v", err)
			} else {
				state.LastAction = "unlocked"
				state.ActionErr = ""
			}
		}
	}
	return false
}

func fetchState(ctx context.Context, client *control.Client, st *DashboardState) {
	st.Timestamp = time.Now()

	if client != nil {
		limit := 10
		if st.ActiveTab == TabLogs {
			limit = 35
		}
		// The four reads are independent: issue them together instead of one after another.
		var (
			wg     sync.WaitGroup
			status control.DaemonStatus
			stErr  error
			chunks []db.ChunkRecord
			chErr  error
			quotaV map[string]any
			qErr   error
			events []eventlog.Event
			auErr  error
		)
		wg.Add(4)
		go func() { defer wg.Done(); status, stErr = client.Status(ctx) }()
		go func() { defer wg.Done(); chunks, chErr = client.QueueList(ctx) }()
		go func() { defer wg.Done(); quotaV, qErr = client.QuotaGet(ctx) }()
		go func() { defer wg.Done(); events, auErr = client.Audit(ctx, limit) }()
		wg.Wait()

		if stErr == nil {
			st.Status = status
		} else if st.Status.State == "" {
			st.Status.State = "offline (config mode)"
		}
		if chErr == nil {
			st.Chunks = chunks
		}
		if qErr == nil {
			st.Quota = quotaV
		}
		if auErr == nil {
			st.AuditEvents = events
		}
	}

	SyncStackFromState(st)

	// Poll live quota for all polled pools
	poolsConfig := make(map[string]config.PoolConfig)
	if len(st.Quota) > 0 {
		if rawPools, ok := st.Quota["pools"].(map[string]any); ok {
			for pName, pVal := range rawPools {
				if pMap, ok := pVal.(map[string]any); ok {
					var pc config.PoolConfig
					if m, ok := stringField(pMap, "Mode", "mode"); ok {
						pc.Mode = m
					}
					if p, ok := stringField(pMap, "Parser", "parser"); ok {
						pc.Parser = p
					}
					if rawCmd, ok := pMap["UsageCmd"].([]any); ok {
						for _, c := range rawCmd {
							if s, ok := c.(string); ok {
								pc.UsageCmd = append(pc.UsageCmd, s)
							}
						}
					} else if rawCmdStr, ok := pMap["UsageCmd"].([]string); ok {
						pc.UsageCmd = rawCmdStr
					} else if rawCmd, ok := pMap["usage_cmd"].([]any); ok {
						for _, c := range rawCmd {
							if s, ok := c.(string); ok {
								pc.UsageCmd = append(pc.UsageCmd, s)
							}
						}
					} else if rawCmdStr, ok := pMap["usage_cmd"].([]string); ok {
						pc.UsageCmd = rawCmdStr
					}
					if pc.Mode == "polled" && len(pc.UsageCmd) == 0 {
						if pName == "gemini" || pName == "claude" {
							pc.UsageCmd = []string{"agy", "-p", "/usage"}
							pc.Parser = "agy-usage"
						} else if pName == "cc" {
							pc.UsageCmd = []string{"claude", "-p", "/usage"}
							pc.Parser = "claude-usage"
						}
					}
					poolsConfig[pName] = pc
				}
			}
		}
	}
	for _, it := range st.StackItems {
		if it.PoolMode == "polled" && it.Pool != "" {
			if _, ok := poolsConfig[it.Pool]; !ok {
				pc := config.PoolConfig{
					Mode: "polled",
				}
				if it.Adapter == "agy" {
					pc.UsageCmd = []string{"agy", "-p", "/usage"}
					pc.Parser = "agy-usage"
				} else if it.Adapter == "claude-cli" || it.Adapter == "claude" {
					pc.UsageCmd = []string{"claude", "-p", "/usage"}
					pc.Parser = "claude-usage"
				}
				poolsConfig[it.Pool] = pc
			}
		}
	}
	st.pollPools = poolsConfig
}

func renderScreen(out io.Writer, state DashboardState) {
	w, h := 80, 24
	if f, ok := out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if tw, th, err := term.GetSize(int(f.Fd())); err == nil && tw > 0 && th > 0 {
			w, h = tw, th
		}
	} else if term.IsTerminal(int(os.Stdout.Fd())) {
		if tw, th, err := term.GetSize(int(os.Stdout.Fd())); err == nil && tw > 0 && th > 0 {
			w, h = tw, th
		}
	}

	content := Render(state, w, h)
	rawLines := strings.Split(content, "\n")
	if len(rawLines) > 0 && rawLines[len(rawLines)-1] == "" {
		rawLines = rawLines[:len(rawLines)-1]
	}

	if len(rawLines) > h {
		rawLines = rawLines[:h]
	}

	var b strings.Builder
	b.WriteString("\x1b[H") // Move cursor to top-left (1,1)
	for i, line := range rawLines {
		b.WriteString(line)
		b.WriteString("\x1b[K") // Clear line to right margin
		if i < len(rawLines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\x1b[J") // Clear remaining lines to bottom of terminal
	fmt.Fprint(out, b.String())
}

// isCursorMove reports whether an action only moves the selection (no daemon state changes).
func isCursorMove(act KeyAction) bool {
	return act == ActionUp || act == ActionDown
}

// quotaPoller runs the slow polled-quota usage commands in the background so the UI never waits on
// them. apply() hands the freshest finished readings to the UI loop and starts the next poll.
type quotaPoller struct {
	mu       sync.Mutex
	readings map[string]quota.Bucket
	fresh    bool
	running  bool
}

func newQuotaPoller() *quotaPoller { return &quotaPoller{} }

func (q *quotaPoller) apply(st *DashboardState) {
	q.mu.Lock()
	if q.fresh {
		st.QuotaReadings = q.readings
		q.fresh = false
	}
	start := !q.running && len(st.pollPools) > 0
	if start {
		q.running = true
	}
	q.mu.Unlock()
	if !start {
		return
	}
	pools := st.pollPools
	go func() {
		// Detached from the UI loop; PollCommand bounds each command with its own timeout.
		readings := quota.PollAllPools(context.Background(), pools)
		q.mu.Lock()
		q.readings, q.fresh, q.running = readings, true, false
		q.mu.Unlock()
	}()
}
