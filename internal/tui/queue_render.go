package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/erinwnash/Shepherd/internal/db"
)

// fitCol pads or cuts s to exactly n visible runes.
func fitCol(s string, n int) string {
	if utf8.RuneCountInString(s) > n {
		r := []rune(s)
		if n > 1 {
			return string(r[:n-1]) + "…"
		}
		return string(r[:n])
	}
	return s + strings.Repeat(" ", n-utf8.RuneCountInString(s))
}

func keyHint(key, label string) string {
	return fmt.Sprintf("%s[%s]%s %s", Bold+FgHiCyan, key, Reset, label)
}

// queueCheck is the checkbox shown for a chunk: [x] will run, [ ] will not, plus lifecycle marks.
func queueCheck(status string) string {
	switch status {
	case "pending":
		return FgHiGreen + "[x]" + Reset
	case "running":
		return Bold + FgHiCyan + "[>]" + Reset
	case "done":
		return Dim + "[✓]" + Reset
	case "blocked":
		return FgHiRed + "[!]" + Reset
	case "held":
		return FgHiYellow + "[~]" + Reset
	case "skipped":
		return Dim + "[-]" + Reset
	default: // inactive
		return Dim + "[ ]" + Reset
	}
}

func queueStatusColor(status string) string {
	switch status {
	case "pending":
		return FgHiGreen
	case "running":
		return Bold + FgHiCyan
	case "blocked":
		return FgHiRed
	case "held":
		return FgHiYellow
	default:
		return Dim
	}
}

func assignedLabel(ch db.ChunkRecord) (string, string) {
	if ch.PinTier == "" {
		if ch.Tier != "" && (ch.Status == "running" || ch.Status == "done" || ch.Status == "blocked") {
			return "auto → " + ch.Tier, Dim
		}
		return "auto", Dim
	}
	model := ch.PinModel
	if model == "" {
		model = "default"
	}
	return ch.PinTier + " / " + model, Bold + FgHiMagenta
}

func queueSummary(chunks []db.ChunkRecord) string {
	counts := map[string]int{}
	for _, c := range chunks {
		counts[c.Status]++
	}
	parts := []string{fmt.Sprintf("%d chunks", len(chunks))}
	for _, k := range []string{"pending", "inactive", "running", "blocked", "held", "skipped", "done"} {
		if counts[k] > 0 {
			label := k
			if k == "pending" {
				label = "queued"
			}
			parts = append(parts, fmt.Sprintf("%s%d %s%s", queueStatusColor(k), counts[k], label, Reset))
		}
	}
	return strings.Join(parts, "  ")
}

// renderQueueTab draws the queue manager into at most avail lines.
func renderQueueTab(st DashboardState, width, avail int) []string {
	if st.QueuePicker {
		return renderQueuePicker(&st, width, avail)
	}

	const overhead = 12 // list box chrome + summary + header + 2 detail lines + controls box
	rows := avail - overhead
	if rows < 3 {
		rows = 3
	}

	idx := queueIndex(&st)
	chunks := visibleChunks(&st)
	summary := queueSummary(st.Chunks)
	summary += fmt.Sprintf("  %sfilter: %s%s", Dim, st.QueueFilter, Reset)
	if st.QueueHideDone {
		summary += fmt.Sprintf("  %s(done hidden)%s", Dim, Reset)
	}
	body := []string{summary}
	body = append(body, fmt.Sprintf("%s%s%s %s %s %s %s%s", Dim, fitCol("", 2), fitCol("ON", 3), fitCol("ID", 10), fitCol("STATUS", 9), fitCol("MODEL (assigned)", 24), "TITLE", Reset))

	if len(chunks) == 0 {
		msg := "The queue is empty. Add work orders to the plan tree, then press [f] to refresh."
		if len(st.Chunks) > 0 {
			msg = "No chunks match the current filter. [o] changes the status filter, [w] shows completed chunks."
		}
		body = append(body, fmt.Sprintf("%s%s%s", FgHiYellow, msg, Reset))
	} else {
		start := 0
		if len(chunks) > rows {
			start = idx - rows/2
			if start < 0 {
				start = 0
			}
			if start+rows > len(chunks) {
				start = len(chunks) - rows
			}
		}
		end := start + rows
		if end > len(chunks) {
			end = len(chunks)
		}
		for i := start; i < end; i++ {
			ch := chunks[i]
			cursor, weight := "  ", ""
			if i == idx {
				cursor, weight = Bold+FgHiCyan+"> "+Reset, Bold
			}
			label, color := assignedLabel(ch)
			statusText, statusColor := ch.Status, queueStatusColor(ch.Status)
			if ch.Status == "pending" && ch.WaitingOn != "" {
				statusText, statusColor = "waiting", FgYellow
			}
			body = append(body, fmt.Sprintf("%s%s %s%s %s%s%s %s%s%s %s%s",
				cursor, queueCheck(ch.Status), weight, fitCol(ch.ID, 10),
				statusColor, fitCol(statusText, 9), Reset,
				color, fitCol(label, 24), Reset,
				weight, ch.Title)+Reset)
		}
		// Pad so the box keeps a steady height while the list is short.
		for i := end - start; i < rows; i++ {
			body = append(body, "")
		}
		if len(chunks) > rows {
			body[0] += fmt.Sprintf("  %s[%d-%d/%d]%s", Dim, start+1, end, len(chunks), Reset)
		}
	}

	detail1, detail2 := "", ""
	if ch := selectedChunk(&st); ch != nil {
		detail1 = fmt.Sprintf("%s%s%s  sprint %s  attempts %d", Bold, ch.ID, Reset, ch.Sprint, ch.AttemptNo)
		if ch.Tier != "" {
			detail1 += "  last tier " + ch.Tier
		}
		switch {
		case ch.BlockedReason != "" && (ch.Status == "blocked" || ch.Status == "held"):
			detail2 = fmt.Sprintf("%sReason:%s %s", FgHiRed, Reset, ch.BlockedReason)
		case ch.Status == "pending" && ch.WaitingOn != "":
			detail2 = fmt.Sprintf("%sNot started:%s %s", FgYellow, Reset, ch.WaitingOn)
		case ch.PinTier != "":
			detail2 = fmt.Sprintf("%sAssigned:%s runs on %s whatever its complexity ([c] to route automatically)", FgHiMagenta, Reset, ch.PinTier)
		case ch.Status == "inactive":
			detail2 = fmt.Sprintf("%sUnchecked:%s the scheduler skips this chunk until you press [Space]", Dim, Reset)
		}
	}
	body = append(body, detail1, detail2)

	out := Box("QUEUE MANAGER", body, width)
	controls := []string{
		keyHint("↑/↓", "Select") + "  " + keyHint("Space", "Check/Uncheck") + "  " + keyHint("a", "Check all") + "  " + keyHint("z", "Uncheck all"),
		keyHint("x", "Requeue") + "  " + keyHint("b", "Unblock") + "  " + keyHint("m", "Model") + "  " + keyHint("c", "Auto") + "  " + keyHint("f", "Refresh from disk"),
		keyHint("o", "Filter: all/blocked/skipped") + "  " + keyHint("w", "Hide/show done"),
		Dim + "Unchecked chunks never run.  [r] start queue  [h] halt running chunk" + Reset,
	}
	out = append(out, Box("CONTROLS", controls, width)...)
	return out
}

// renderQueuePicker draws the model chooser for the selected chunk.
func renderQueuePicker(st *DashboardState, width, avail int) []string {
	opts := queueModelOptions(st)
	if st.QueuePickerIdx >= len(opts) {
		st.QueuePickerIdx = 0
	}
	rows := avail - 5
	if rows < 3 {
		rows = 3
	}
	start := 0
	if len(opts) > rows {
		start = st.QueuePickerIdx - rows/2
		if start < 0 {
			start = 0
		}
		if start+rows > len(opts) {
			start = len(opts) - rows
		}
	}
	end := start + rows
	if end > len(opts) {
		end = len(opts)
	}

	title := "ASSIGN MODEL"
	current := "auto"
	if ch := selectedChunk(st); ch != nil {
		title = "ASSIGN MODEL: " + ch.ID
		current, _ = assignedLabel(*ch)
	}
	body := []string{fmt.Sprintf("%sRuns on the chosen model whatever the chunk's complexity. Now: %s%s", Dim+Italic, current, Reset), ""}
	for i := start; i < end; i++ {
		cursor, weight := "  ", ""
		if i == st.QueuePickerIdx {
			cursor, weight = Bold+FgHiCyan+"> "+Reset, Bold
		}
		body = append(body, cursor+weight+opts[i].Label+Reset)
	}
	body = append(body, "", keyHint("↑/↓", "Choose")+"  "+keyHint("Enter", "Assign")+"  "+keyHint("Esc", "Cancel"))
	return Box(title, body, width)
}
