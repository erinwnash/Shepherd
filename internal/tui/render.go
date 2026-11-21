package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// StripAnsi removes ANSI escape codes for accurate length calculations.
func StripAnsi(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			// Skip escape sequence
			i++
			if i >= len(s) {
				break
			}
			if s[i] == '[' {
				// CSI sequence: read until final byte in 0x40..0x7E
				i++
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7E) {
					i++
				}
			} else if s[i] == ']' {
				// OSC sequence: read until BEL (\x07) or ST (\x1b\)
				for i < len(s) && s[i] != '\x07' && s[i] != '\x1b' {
					i++
				}
				if i < len(s) && s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
					i++
				}
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TruncateAnsi truncates a string with ANSI escape codes to maxLen visible characters,
// preserving escape sequences and ensuring any opened formatting is reset.
func TruncateAnsi(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if VisibleLen(s) <= maxLen {
		return s
	}

	var b strings.Builder
	visCount := 0

	i := 0
	for i < len(s) {
		if s[i] == '\x1b' {
			start := i
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7E) {
					i++
				}
			}
			if i < len(s) {
				b.WriteString(s[start : i+1])
				i++
			}
			continue
		}

		if visCount >= maxLen {
			break
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		i += size
		visCount++
	}

	b.WriteString(Reset)
	return b.String()
}

// VisibleLen returns the length of a string without ANSI formatting.
func VisibleLen(s string) int {
	return utf8.RuneCountInString(StripAnsi(s))
}

// PadRight pads s to target visible width.
func PadRight(s string, width int) string {
	vl := VisibleLen(s)
	if vl >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vl)
}

// RenderGauge formats a visual percentage bar: [████████░░░░░░░░] 50%
func RenderGauge(pct int, barWidth int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	if barWidth <= 0 {
		barWidth = 16
	}

	filled := (pct * barWidth) / 100
	unfilled := barWidth - filled

	color := FgHiGreen
	if pct >= 80 {
		color = FgHiRed
	} else if pct >= 60 {
		color = FgHiYellow
	}

	bar := color + strings.Repeat("█", filled) + Dim + strings.Repeat("░", unfilled) + Reset
	return fmt.Sprintf("[%s] %3d%%", bar, pct)
}

// Box formats a border around content lines to standard width.
func Box(title string, lines []string, width int) []string {
	if width < 40 {
		width = 80
	}

	innerWidth := width - 4
	if innerWidth < 10 {
		innerWidth = 10
	}

	var out []string

	// Top line: ┌─ TITLE ────────────────┐
	titleStr := ""
	if title != "" {
		titleStr = fmt.Sprintf(" %s%s%s ", Bold, title, Reset)
	}
	dashCount := (width - 2) - VisibleLen(titleStr) - 1
	if dashCount < 1 {
		dashCount = 1
	}
	topLine := fmt.Sprintf("%s┌─%s%s┐%s", FgHiBlack, titleStr+FgHiBlack, strings.Repeat("─", dashCount), Reset)
	out = append(out, topLine)

	// Content lines: │ Content                │
	for _, l := range lines {
		if VisibleLen(l) > innerWidth {
			l = TruncateAnsi(l, innerWidth)
		}
		vl := VisibleLen(l)
		pad := innerWidth - vl
		if pad < 0 {
			pad = 0
		}
		row := fmt.Sprintf("%s│%s %s%s %s│%s", FgHiBlack, Reset, l, strings.Repeat(" ", pad), FgHiBlack, Reset)
		out = append(out, row)
	}

	// Bottom line: └────────────────────────┘
	bottomLine := fmt.Sprintf("%s└%s┘%s", FgHiBlack, strings.Repeat("─", width-2), Reset)
	out = append(out, bottomLine)

	return out
}

// Render compiles the full dashboard screen as a single string.
func Render(st DashboardState, width, height int) string {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}

	var lines []string

	// 0. Dashboard Tab Navigation Bar
	tab1 := fmt.Sprintf("%s[1] OVERVIEW%s", Dim+FgHiBlack, Reset)
	tab2 := fmt.Sprintf("%s[2] LOGS%s", Dim+FgHiBlack, Reset)
	tab3 := fmt.Sprintf("%s[3] STACK & RULES%s", Dim+FgHiBlack, Reset)
	tab4 := fmt.Sprintf("%s[4] QUEUE%s", Dim+FgHiBlack, Reset)

	switch st.ActiveTab {
	case TabOverview:
		tab1 = fmt.Sprintf("%s%s[1] OVERVIEW%s", Bold, FgHiCyan, Reset)
	case TabLogs:
		tab2 = fmt.Sprintf("%s%s[2] LOGS%s", Bold, FgHiGreen, Reset)
	case TabStack:
		tab3 = fmt.Sprintf("%s%s[3] STACK & RULES%s", Bold, FgHiCyan, Reset)
	case TabQueue:
		tab4 = fmt.Sprintf("%s%s[4] QUEUE%s", Bold, FgHiCyan, Reset)
	}

	tabBar := fmt.Sprintf(" %s   %s   %s   %s", tab1, tab2, tab3, tab4)
	lines = append(lines, tabBar, "")

	// 1. Header Box
	stateColor := FgHiGreen
	switch strings.ToLower(st.Status.State) {
	case "paused":
		stateColor = FgHiYellow
	case "lockdown":
		stateColor = BgRed + FgHiWhite
	case "draining":
		stateColor = FgHiCyan
	case "stopping":
		stateColor = FgHiMagenta
	case "idle":
		stateColor = FgHiBlack
	}

	clockStr := st.Timestamp.Format("2006-01-02 15:04:05")
	if st.Timestamp.IsZero() {
		clockStr = time.Now().Format("2006-01-02 15:04:05")
	}

	drainBadge := fmt.Sprintf("%sOFF%s", Dim, Reset)
	if st.Status.DrainMode {
		drainBadge = fmt.Sprintf("%s%sACTIVE%s", Bold, FgHiCyan, Reset)
	}

	lockBadge := fmt.Sprintf("%sOFF%s", Dim, Reset)
	if st.Status.Lockdown {
		lockBadge = fmt.Sprintf("%s%sACTIVE%s", Bold, BgRed+FgHiWhite, Reset)
	}

	headerLine1 := fmt.Sprintf("State: [%s%s%s%s]   Drain: [%s]   Lockdown: [%s]   Time: %s",
		Bold, stateColor, strings.ToUpper(st.Status.State), Reset, drainBadge, lockBadge, clockStr)

	headerBox := Box("SHEPHERD CONTROL STATION", []string{headerLine1}, width)
	lines = append(lines, headerBox...)

	switch st.ActiveTab {
	case TabLogs:
		avail := height - 10
		if avail < 3 {
			avail = 3
		}
		var logContent []string
		logContent = append(logContent, fmt.Sprintf("%sLive streaming SHA-256 hash-chained audit log & daemon activity%s", Dim+Italic, Reset), "")
		rawLogs := FormatLiveLogLines(st.AuditEvents)
		if len(rawLogs) > avail {
			rawLogs = rawLogs[len(rawLogs)-avail:]
		}
		logContent = append(logContent, rawLogs...)
		lines = append(lines, Box("LIVE EVENT LOG & AUDIT STREAM", logContent, width)...)

	case TabStack:
		if st.FrontRunConfirm {
			lines = append(lines, frontRunDialog(st, width)...)
		} else if st.Status.FrontRun != nil {
			lines = append(lines, " "+frontRunSummary(st.Status.FrontRun)+fmt.Sprintf("  %s[y] stop%s", Bold+FgHiCyan, Reset), "")
		}
		lines = append(lines, renderStackTab(st, width, height)...)

	case TabQueue:
		// Leave room for the notice line and the two footer lines.
		lines = append(lines, renderQueueTab(st, width, height-len(lines)-3)...)

	default: // TabOverview
		// The ntfy QR panel sits to the right of the overview when the terminal is wide enough,
		// below it when there is spare height, and is left out otherwise.
		bodyStart := len(lines)
		bodyW := width
		qrLines, qrW := qrBox(st)
		sideBySide := qrLines != nil && width >= 80+1+qrW && height >= bodyStart+len(qrLines)+6
		if sideBySide {
			bodyW = width - qrW - 1
		}
		maxQuota := 5
		maxChunks := 4
		maxAlerts := 4
		if height <= 26 {
			maxQuota = 4
			maxChunks = 2
			maxAlerts = 1
		}

		// 2. Quota Gauges Pane
		var quotaLines []string
		if len(st.Quota) > 0 {
			tiersMap, _ := st.Quota["tiers"].(map[string]any)
			if pools, ok := st.Quota["pools"].(map[string]any); ok && len(pools) > 0 {
				poolNames := make([]string, 0, len(pools))
				for name := range pools {
					poolNames = append(poolNames, name)
				}
				sort.Slice(poolNames, func(i, j int) bool {
					if poolNames[i] == "local" || poolNames[i] == "opencode" {
						return true
					}
					if poolNames[j] == "local" || poolNames[j] == "opencode" {
						return false
					}
					return poolNames[i] < poolNames[j]
				})

				for _, name := range poolNames {
					if len(quotaLines) >= maxQuota {
						break
					}
					pMap, _ := pools[name].(map[string]any)
					mode := ""
					floor := 20
					if pMap != nil {
						if m, ok := pMap["Mode"].(string); ok && m != "" {
							mode = m
						} else if m, ok := pMap["mode"].(string); ok && m != "" {
							mode = m
						}
						if f, ok := pMap["FloorWeeklyPct"].(float64); ok && f > 0 {
							floor = int(f)
						} else if f, ok := pMap["floor_weekly_pct"].(float64); ok && f > 0 {
							floor = int(f)
						}
					}

					if name == "local" || name == "opencode" {
						endpoint := "http://127.0.0.1:8080/v1"
						modelStr := ""
						if tiersMap != nil {
							if tLocal, ok := tiersMap[name].(map[string]any); ok {
								if u, ok := tLocal["URL"].(string); ok && u != "" {
									endpoint = u
								} else if u, ok := tLocal["url"].(string); ok && u != "" {
									endpoint = u
								}
								if m, ok := tLocal["Model"].(string); ok && m != "" {
									modelStr = m
								} else if m, ok := tLocal["model"].(string); ok && m != "" {
									modelStr = m
								}
							}
						}
						extra := fmt.Sprintf("Endpoint: %s", endpoint)
						if modelStr != "" {
							extra += fmt.Sprintf(" (%s)", modelStr)
						}
						quotaLines = append(quotaLines, fmt.Sprintf("%s%-14s%s [ %s%sREADY%s ]  %s", Bold, "opencode (loc)", Reset, Bold, FgHiGreen, Reset, extra))
					} else if mode == "until-rejected" {
						label := fmt.Sprintf("%s (rej)", name)
						quotaLines = append(quotaLines, fmt.Sprintf("%s%-14s%s [ %s%sREADY%s ]              Backoff: 0m", Bold, label, Reset, Bold, FgHiGreen, Reset))
					} else {
						label := fmt.Sprintf("%s (poll)", name)
						pct := 35
						resetStr := "3d 12h"
						hasReading := false
						if st.QuotaReadings != nil {
							if b, ok := st.QuotaReadings[name]; ok {
								pct = b.RemainingPct
								hasReading = true
								if !b.ResetTime.IsZero() {
									d := time.Until(b.ResetTime)
									if d > 0 {
										days := int(d.Hours()) / 24
										hours := int(d.Hours()) % 24
										if days > 0 {
											resetStr = fmt.Sprintf("%dd %02dh", days, hours)
										} else {
											resetStr = fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
										}
									} else {
										resetStr = "now"
									}
								}
							}
						}
						gauge := RenderGauge(pct, 14)
						color := Reset
						if hasReading && pct <= floor {
							color = Bold + FgHiRed
						}
						quotaLines = append(quotaLines, fmt.Sprintf("%s%-14s%s %s%s  Weekly Floor: %d%%  Reset: %s%s", Bold, label, Reset, color, gauge, floor, resetStr, Reset))
					}
				}
			}
		}
		if len(quotaLines) == 0 {
			if height <= 26 {
				quotaLines = append(quotaLines,
					fmt.Sprintf("%s%-14s%s [ %s%sREADY%s ]  Endpoint: http://127.0.0.1:8080/v1", Bold, "opencode (loc)", Reset, Bold, FgHiGreen, Reset),
					fmt.Sprintf("%s%-14s%s %s  Floor: 20%%  Reset: 2d 14h", Bold, "claude (poll)", Reset, RenderGauge(42, 14)),
				)
			} else {
				quotaLines = append(quotaLines,
					fmt.Sprintf("%s%-14s%s [ %s%sREADY%s ]  Endpoint: http://127.0.0.1:8080/v1", Bold, "opencode (loc)", Reset, Bold, FgHiGreen, Reset),
					fmt.Sprintf("%s%-14s%s %s  Floor: 20%%  Reset: 2d 14h", Bold, "claude (poll)", Reset, RenderGauge(42, 14)),
					fmt.Sprintf("%s%-14s%s %s  Floor: 15%%  Reset: 5d 08h", Bold, "agy (poll)", Reset, RenderGauge(18, 14)),
					fmt.Sprintf("%s%-14s%s [ %s%sREADY%s ]              Backoff: 0m", Bold, "codex (rej)", Reset, Bold, FgHiGreen, Reset),
				)
			}
		}
		lines = append(lines, Box("QUOTA GAUGES & WINDOWS", quotaLines, bodyW)...)

		// 3. Worker & Execution Pane
		var workerLines []string
		if st.Status.RunningChunkID != "" {
			workerLines = append(workerLines,
				fmt.Sprintf("Worker: %s%s%s   Running Chunk: %s%s%s",
					Bold, st.Status.WorkerID, Reset, Bold+FgHiCyan, st.Status.RunningChunkID, Reset),
			)
			if height > 26 {
				workerLines = append(workerLines, fmt.Sprintf("Status: running agent & verification pipeline"))
			}
		} else {
			workerLines = append(workerLines, fmt.Sprintf("%sWorker idle. Waiting for next runnable chunk.%s", Dim, Reset))
		}
		if len(st.Status.Progress) > 0 {
			tailRows := 0
			if height > 26 {
				tailRows = min(8, (height-18)/(2*len(st.Status.Progress)))
			}
			workerLines = append(workerLines, progressLines(st.Status.Progress, tailRows)...)
		}
		if len(st.Status.Stack) > 0 && height > 26 {
			workerLines = append(workerLines, fmt.Sprintf("Stack:  %s  %s([6/s] Prioritize/Caps/Test)%s", strings.Join(st.Status.Stack, " -> "), Dim, Reset))
		}
		if st.Status.FrontRun != nil {
			workerLines = append(workerLines, frontRunSummary(st.Status.FrontRun)+fmt.Sprintf("  %s([3] then [y] to stop)%s", Dim, Reset))
		}
		lines = append(lines, Box("ACTIVE WORKER", workerLines, bodyW)...)

		// 4. Queue Pane
		var queueLines []string
		summaryLine := fmt.Sprintf("Summary: %s%d pending%s, %s%d running%s, %s%d blocked%s, %s%d completed%s",
			FgHiCyan, st.Status.QueuePending, Reset,
			FgHiYellow, st.Status.QueueRunning, Reset,
			FgHiRed, st.Status.QueueBlocked, Reset,
			FgHiGreen, st.Status.QueueCompleted, Reset,
		)
		queueLines = append(queueLines, summaryLine)

		shownCount := 0
		for _, ch := range st.Chunks {
			if shownCount >= maxChunks {
				break
			}
			statusColor := FgHiBlack
			switch ch.Status {
			case "running":
				statusColor = FgHiYellow
			case "pending":
				statusColor = FgHiCyan
			case "done":
				statusColor = FgHiGreen
			case "blocked":
				statusColor = FgHiRed
			}
			chLine := fmt.Sprintf("  %-6s [%s%-7s%s] %-30s", ch.ID, statusColor, strings.ToUpper(ch.Status), Reset, ch.Title)
			queueLines = append(queueLines, chLine)
			shownCount++
		}
		lines = append(lines, Box("QUEUE STATUS", queueLines, bodyW)...)

		// 5. System Alerts / Audit Pane
		var alertLines []string
		if len(st.Status.Overrides) > 0 {
			var ovList []string
			for k, v := range st.Status.Overrides {
				ovList = append(ovList, fmt.Sprintf("%s=%s", k, v))
			}
			alertLines = append(alertLines, fmt.Sprintf("%sOverrides:%s %s", FgHiYellow, Reset, strings.Join(ovList, ", ")))
		}
		if len(st.AuditEvents) > 0 {
			for i := len(st.AuditEvents) - 1; i >= 0 && len(alertLines) < maxAlerts; i-- {
				ev := st.AuditEvents[i]
				ts := ev.Timestamp.Format("15:04:05")
				alertLines = append(alertLines, fmt.Sprintf("[%s] %s%s%s: %s", ts, Bold, ev.Type, Reset, ev.Payload))
			}
		}
		if len(alertLines) == 0 {
			alertLines = append(alertLines, fmt.Sprintf("%sSystem normal. All gates active and verified.%s", Dim, Reset))
		}
		lines = append(lines, Box("ALERTS & AUDIT LEDGER", alertLines, bodyW)...)

		switch {
		case sideBySide:
			merged := joinColumns(lines[bodyStart:], bodyW, qrLines, 1)
			lines = append(lines[:bodyStart], merged...)
		case qrLines != nil && len(lines)+len(qrLines)+6 <= height:
			lines = append(lines, qrLines...)
		}
	}

	// 6. Action / Status Message
	if st.ActionErr != "" {
		lines = append(lines, fmt.Sprintf(" %sError:%s %s", BgRed+FgHiWhite, Reset, st.ActionErr))
	} else if st.LastAction != "" {
		lines = append(lines, fmt.Sprintf(" %sNotice:%s %s", FgHiGreen, Reset, st.LastAction))
	}

	// 7. Footer Hotkeys
	var hotkeys string
	if width <= 80 {
		hotkeys = fmt.Sprintf("%s[1-4]%s Tabs  %s[s]%s Stack  %s[p]%s Pause  %s[r]%s Resume  %s[h]%s Halt  %s[d]%s Drain  %s[q]%s Quit",
			Bold+FgHiCyan, Reset, Bold+FgHiCyan, Reset, Bold+FgHiCyan, Reset, Bold+FgHiCyan, Reset,
			Bold+FgHiCyan, Reset, Bold+FgHiCyan, Reset, Bold+FgHiCyan, Reset)
	} else {
		hotkeys = fmt.Sprintf("%s[1-4/Tab]%s Tabs  %s[s]%s Stack/Rules  %s[p]%s Pause  %s[r]%s Resume  %s[h]%s Halt  %s[d]%s Drain  %s[q]%s Quit",
			Bold+FgHiCyan, Reset, Bold+FgHiCyan, Reset, Bold+FgHiCyan, Reset, Bold+FgHiCyan, Reset,
			Bold+FgHiCyan, Reset, Bold+FgHiCyan, Reset, Bold+FgHiCyan, Reset)
	}
	lines = append(lines, "", " "+hotkeys)
	footerLen := 2

	// 8. ntfy topic (Overview only): the key to subscribe to and send commands on.
	if st.ActiveTab == TabOverview {
		lines = append(lines, " "+ntfyLine(st))
		footerLen = 3
	}

	// Strict clamp: ensure total rendered lines never exceed height
	if height > 5 && len(lines) > height {
		footer := lines[len(lines)-footerLen:]
		cutCount := height - footerLen
		if cutCount > 0 && cutCount <= len(lines) {
			lines = append(lines[:cutCount], footer...)
		}
	}

	return strings.Join(lines, "\n") + "\n"
}

func ntfyLine(st DashboardState) string {
	switch {
	case st.Status.NtfyTopic != "":
		return fmt.Sprintf("%sntfy:%s %s%s%s  %s[n n] rotate  [i] QR %s%s", Bold+FgHiMagenta, Reset, Bold, displayTopic(st.Status.NtfyTopic), Reset, Dim, qrState(st), Reset)
	case strings.HasPrefix(st.Status.State, "offline"):
		return fmt.Sprintf("%sntfy:%s %sdaemon offline%s", Bold+FgHiMagenta, Reset, Dim, Reset)
	default:
		return fmt.Sprintf("%sntfy:%s %snot configured (set [notify] ntfy_topic, NTFY_TOPIC or NOTIFY_CMD)%s", Bold+FgHiMagenta, Reset, Dim, Reset)
	}
}

func qrState(st DashboardState) string {
	if st.QRHidden {
		return "off"
	}
	return "on"
}

// displayTopic drops the scheme so the line reads like what you type into the ntfy app.
func displayTopic(u string) string {
	u = strings.TrimPrefix(u, "https://")
	return strings.TrimPrefix(u, "http://")
}

func renderStackTab(st DashboardState, width, height int) []string {
	var out []string

	var stackLines []string
	stackLines = append(stackLines,
		fmt.Sprintf("%sPrioritize models & CLIs, activate/deactivate tiers, set usage caps, and quick-test live:%s", Dim+Italic, Reset),
		fmt.Sprintf("%s%-3s %-4s %-6s %-18s %-22s %-14s %s%s",
			Dim, "SEL", "PRIO", "STATE", "TIER (ADAPTER)", "MODEL / ENDPOINT", "CAP / FLOOR", "TEST STATUS", Reset),
	)

	maxStackRows := len(st.StackItems)
	if height <= 26 && maxStackRows > 8 {
		maxStackRows = 8
	}

	if len(st.StackItems) == 0 {
		stackLines = append(stackLines, fmt.Sprintf("%sNo stack tiers loaded. Run 'shepherd wizard' to discover routers and CLIs.%s", FgHiYellow, Reset))
	} else {
		startIdx := 0
		if len(st.StackItems) > maxStackRows {
			startIdx = st.SelectedStackIdx - maxStackRows/2
			if startIdx < 0 {
				startIdx = 0
			}
			if startIdx+maxStackRows > len(st.StackItems) {
				startIdx = len(st.StackItems) - maxStackRows
			}
		}
		endIdx := startIdx + maxStackRows
		if endIdx > len(st.StackItems) {
			endIdx = len(st.StackItems)
		}

		for i := startIdx; i < endIdx; i++ {
			it := st.StackItems[i]
			sel := "   "
			rowBold := ""
			if i == st.SelectedStackIdx {
				sel = Bold + FgHiCyan + " > " + Reset
				rowBold = Bold
			}
			stateBadge := FgHiGreen + "[ON ]" + Reset
			if !it.Enabled {
				stateBadge = FgHiRed + "[OFF]" + Reset
			}
			tierCol := fmt.Sprintf("%s (%s)", it.Tier, it.Adapter)
			if len(tierCol) > 18 {
				tierCol = tierCol[:18]
			}
			modelCol := it.Model
			if modelCol == "" {
				modelCol = "default"
			}
			if it.URL != "" {
				shortURL := strings.TrimPrefix(strings.TrimPrefix(it.URL, "http://"), "https://")
				modelCol = fmt.Sprintf("%s @ %s", modelCol, shortURL)
			}
			if len(modelCol) > 22 {
				modelCol = modelCol[:19] + "..."
			}
			capPct := it.UsageCapPct
			if capPct <= 0 {
				capPct = 100
			}
			capCol := fmt.Sprintf("Cap:%3d%%", capPct)
			if it.FloorPct > 0 {
				capCol = fmt.Sprintf("C:%d%% F:%d%%", capPct, it.FloorPct)
			}
			probeStr := it.ProbeStatus
			probeColor := Dim
			if probeStr == "" {
				probeStr = "[t] to test"
			} else if it.ProbeOk {
				probeColor = FgHiGreen
			} else {
				probeColor = FgHiRed
			}
			row := fmt.Sprintf("%s%s#%-3d %s  %-18s %-22s %-14s %s%s%s",
				sel, rowBold, i+1, stateBadge, tierCol, modelCol, capCol, probeColor, probeStr, Reset)
			stackLines = append(stackLines, row)
		}
	}

	var activeChain []string
	for _, it := range st.StackItems {
		if it.Enabled {
			att := it.Attempts
			if att <= 0 {
				att = 2
			}
			activeChain = append(activeChain, fmt.Sprintf("%s:%d", it.Tier, att))
		}
	}
	chainStr := "none (all tiers deactivated)"
	if len(activeChain) > 0 {
		chainStr = strings.Join(activeChain, " -> ")
	}
	stackLines = append(stackLines, "", fmt.Sprintf("Active Escalation Ladder: %s%s%s", Bold+FgHiCyan, chainStr, Reset))
	out = append(out, Box("AI ROUTER & CLI PRIORITY STACK", stackLines, width)...)

	// Routing Rules & Usage Caps Box
	thresh := st.Rules.CloudFirstThreshold
	if thresh <= 0 {
		thresh = 0.50
	}
	ruleMode := "Balanced (0.50)"
	if thresh >= 0.70 {
		ruleMode = "Local-First (0.75 - prefer local AI router, escalate on failure)"
	} else if thresh <= 0.30 {
		ruleMode = "Cloud-First (0.25 - route medium/complex chunks directly to cloud)"
	}
	budgetStr := "Unlimited ($0)"
	if st.Rules.MaxSprintCostUSD > 0 {
		budgetStr = fmt.Sprintf("$%.2f / sprint  ($%.2f / chunk)", st.Rules.MaxSprintCostUSD, st.Rules.MaxChunkCostUSD)
	}
	humanGateStr := "OFF"
	if st.Rules.RequireHighReviewHuman {
		humanGateStr = "ON (require human sign-off on HIGH review chunks)"
	}

	var ruleLines []string
	if height <= 26 {
		ruleLines = []string{
			fmt.Sprintf("%sRouting [g]:%s %s  %sBudget [b]:%s %s", Bold, Reset, ruleMode, Bold, Reset, budgetStr),
			fmt.Sprintf("%sHuman Gate [v]:%s %s", Bold, Reset, humanGateStr),
			fmt.Sprintf("%sControls:%s [j/k] Select  [Space] On/Off  [+/-] Prio  [m] Model  [c] Cap  [t] Test", Bold+FgHiCyan, Reset),
		}
	} else {
		ruleLines = []string{
			fmt.Sprintf("%sRouting Rule      [g]:%s %s", Bold, Reset, ruleMode),
			fmt.Sprintf("%sUsage Budget Cap  [b]:%s %s", Bold, Reset, budgetStr),
			fmt.Sprintf("%sHuman Review Gate [v]:%s %s", Bold, Reset, humanGateStr),
			"",
			fmt.Sprintf("%sControls:%s [j/k] Select  [Space/e] Activate/Deactivate  [u/d or +/-] Move Priority Up/Down", Bold+FgHiCyan, Reset),
			fmt.Sprintf("          [m] Cycle Model  [c] Usage Cap%%  [f] Weekly Floor%%  [t] Quick-Test  [T] Test All  [y] Front-run"),
			fmt.Sprintf("          %sTip:%s Press [t] to probe live models, [m] to cycle, or run 'agy models' to list all", Dim+Italic, Reset),
		}
	}
	out = append(out, Box("ROUTING RULES & USAGE CAPS", ruleLines, width)...)

	return out
}
