package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/erinwnash/Shepherd/internal/control"
)

// frontRunAPI is the slice of the control client the front-run lane controls use.
type frontRunAPI interface {
	FrontRunStart(ctx context.Context, tier, model string) error
	FrontRunStop(ctx context.Context) error
}

// frontRunClient avoids wrapping a nil *control.Client in a non-nil interface (offline/config mode).
func frontRunClient(c *control.Client) frontRunAPI {
	if c == nil {
		return nil
	}
	return c
}

func frontRunLabel(tier, model string) string {
	if model == "" {
		return tier
	}
	return tier + "/" + model
}

// handleFrontRunAction owns [y] and the confirmation dialog. It reports whether it consumed act.
func handleFrontRunAction(ctx context.Context, api frontRunAPI, st *DashboardState, act KeyAction) (handled, quit bool) {
	if st.FrontRunConfirm {
		switch act {
		case ActionQuit:
			return true, true
		case ActionConfirm, ActionFrontRun:
			st.FrontRunConfirm = false
			if api == nil {
				st.ActionErr = "daemon offline: cannot start front-run"
				return true, false
			}
			if err := api.FrontRunStart(ctx, st.frontRunTier, st.frontRunModel); err != nil {
				st.LastAction, st.ActionErr = "", fmt.Sprintf("front-run failed to start: %v", err)
				return true, false
			}
			st.LastAction, st.ActionErr = "front-run started with "+frontRunLabel(st.frontRunTier, st.frontRunModel), ""
		default:
			st.FrontRunConfirm = false
			st.LastAction, st.ActionErr = "front-run cancelled", ""
		}
		return true, false // the dialog owns the keyboard until it closes
	}
	if act != ActionFrontRun {
		return false, false
	}
	if st.Status.FrontRun != nil {
		if api == nil {
			st.ActionErr = "daemon offline: cannot stop front-run"
			return true, false
		}
		if err := api.FrontRunStop(ctx); err != nil {
			st.LastAction, st.ActionErr = "", fmt.Sprintf("front-run failed to stop: %v", err)
			return true, false
		}
		st.Status.FrontRun = nil
		st.LastAction, st.ActionErr = "front-run stopped; its chunk went back to the main queue", ""
		return true, false
	}
	if st.ActiveTab != TabStack || len(st.StackItems) == 0 {
		return false, false
	}
	idx := st.SelectedStackIdx
	if idx < 0 || idx >= len(st.StackItems) {
		idx = 0
	}
	it := st.StackItems[idx]
	st.frontRunTier, st.frontRunModel = it.Tier, it.Model
	st.FrontRunConfirm = true
	st.LastAction, st.ActionErr = "", ""
	return true, false
}

// frontRunDialog is the small confirmation box shown on the stack page.
func frontRunDialog(st DashboardState, width int) []string {
	w := 64
	if width > 0 && w > width {
		w = width
	}
	return Box("FRONT-RUN", []string{
		fmt.Sprintf("Do you want to front run with %s%s%s?", Bold+FgHiCyan, frontRunLabel(st.frontRunTier, st.frontRunModel), Reset),
		fmt.Sprintf("%sWorks high-complexity chunks %d+ items ahead, in its own worktree.%s", Dim, 10, Reset),
		fmt.Sprintf("%s[Enter/y]%s Start   %s[any other key]%s Cancel", Bold+FgHiGreen, Reset, Bold, Reset),
	}, w)
}

// frontRunSummary is the one-line state of a running lane, shared by the stack page and the overview.
func frontRunSummary(f *control.FrontRunStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%sFRONT-RUN%s %s", Bold, FgHiMagenta, Reset, frontRunLabel(f.Tier, f.Model))
	if f.Current != "" {
		fmt.Fprintf(&b, "  working on %s%s%s", Bold+FgHiCyan, f.Current, Reset)
	}
	fmt.Fprintf(&b, "  landed %d", f.Landed)
	if f.Failed > 0 {
		fmt.Fprintf(&b, "  handed back %d", f.Failed)
	}
	if f.Note != "" {
		fmt.Fprintf(&b, "  %s%s%s", Dim, f.Note, Reset)
	}
	return b.String()
}
