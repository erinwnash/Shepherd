package tui

import (
	"fmt"
	"strings"

	"github.com/erinwnash/Shepherd/internal/control"
)

// fmtSecs renders a span of seconds as "42s", "7m05s" or "1h02m".
func fmtSecs(s int) string {
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	}
}

// progressSummary is the one-line state of a chunk in flight: phase, time, and how much it has touched.
func progressSummary(p control.ChunkProgress) string {
	var b strings.Builder
	lane := ""
	if p.Lane == "frontrun" {
		lane = FgHiMagenta + "front-run " + Reset
	}
	fmt.Fprintf(&b, "%s%s%s%s", lane, Bold+FgHiCyan, p.ID, Reset)
	phase := p.Phase
	if phase == "" {
		phase = "starting"
	}
	fmt.Fprintf(&b, "  %s%s%s", Bold, phase, Reset)
	if p.Attempt > 1 {
		fmt.Fprintf(&b, " #%d", p.Attempt)
	}
	fmt.Fprintf(&b, " %s (%s total)", fmtSecs(p.PhaseSec), fmtSecs(p.TotalSec))
	if p.Detail != "" {
		fmt.Fprintf(&b, "  %s%s%s", Dim, p.Detail, Reset)
	}
	fmt.Fprintf(&b, "  %d files %s+%d%s/%s-%d%s", p.Files, FgHiGreen, p.Added, Reset, FgHiRed, p.Deleted, Reset)
	if p.Stalled {
		fmt.Fprintf(&b, "  %s%sSTALLED %s%s", Bold, FgHiRed, fmtSecs(p.IdleSec), Reset)
	} else if p.Files > 0 || p.IdleSec >= 30 {
		fmt.Fprintf(&b, "  %slast change %s ago%s", Dim, fmtSecs(p.IdleSec), Reset)
	}
	return b.String()
}

// progressLines is the progress block for the overview: a summary line per chunk in flight, each
// followed by up to tailRows lines of the agent's latest output.
func progressLines(ps []control.ChunkProgress, tailRows int) []string {
	var out []string
	for _, p := range ps {
		out = append(out, progressSummary(p))
		if tailRows <= 0 {
			continue
		}
		tail := p.Tail
		if len(tail) > tailRows {
			tail = tail[len(tail)-tailRows:]
		}
		if len(tail) == 0 {
			out = append(out, Dim+"  │ (no output yet)"+Reset)
		}
		for _, l := range tail {
			out = append(out, Dim+"  │ "+Reset+l)
		}
	}
	return out
}
