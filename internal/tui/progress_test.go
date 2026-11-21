package tui

import (
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/control"
)

func TestTUI_OverviewShowsPhaseDiffAndOutputTail(t *testing.T) {
	st := DashboardState{ActiveTab: TabOverview}
	st.Status.RunningChunkID = "46D.1"
	st.Status.WorkerID = "w1"
	st.Status.Progress = []control.ChunkProgress{{
		ID: "46D.1", Lane: "main", Phase: "agent", Detail: "cc · sonnet", PhaseSec: 125, TotalSec: 130,
		Files: 3, Added: 40, Deleted: 2, IdleSec: 12, Tail: []string{"reading parser.go", "editing parser.go"},
	}}
	out := StripAnsi(Render(st, 120, 40))
	for _, want := range []string{"46D.1", "agent", "2m05s", "3 files", "+40", "-2", "editing parser.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("overview missing %q:\n%s", want, out)
		}
	}

	st.Status.Progress[0].Stalled, st.Status.Progress[0].IdleSec = true, 700
	if out := StripAnsi(Render(st, 120, 40)); !strings.Contains(out, "STALLED 11m40s") {
		t.Errorf("stall not flagged:\n%s", out)
	}

	st.Status.Progress[0].Tail = nil
	if out := StripAnsi(Render(st, 120, 40)); !strings.Contains(out, "no output yet") {
		t.Errorf("empty tail should say so:\n%s", out)
	}
}
