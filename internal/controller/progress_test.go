package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/adapter"
	"github.com/erinwnash/Shepherd/internal/control"
)

func shortProgress(t *testing.T, stall time.Duration) {
	t.Helper()
	oldPoll, oldStall := progressPollInterval, progressStallWindow
	progressPollInterval, progressStallWindow = 20*time.Millisecond, stall
	t.Cleanup(func() { progressPollInterval, progressStallWindow = oldPoll, oldStall })
}

func eventsOfType(t *testing.T, ctrl *Controller, typ string) []string {
	t.Helper()
	evs, _ := ctrl.store.EventLog.Since(0)
	var out []string
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e.Payload)
		}
	}
	return out
}

func progressFor(ctrl *Controller, id string) *control.ChunkProgress {
	st, _ := ctrl.Status(context.Background())
	for i := range st.Progress {
		if st.Progress[i].ID == id {
			return &st.Progress[i]
		}
	}
	return nil
}

func TestProgress_PhasesDiffAndTailAreVisibleWhileAChunkRuns(t *testing.T) {
	shortProgress(t, time.Hour)
	release := make(chan struct{})
	ctrl, _ := frontRunFixture(t, func(ctx context.Context, opts adapter.RunOptions) (adapter.RunResult, error) {
		_ = os.WriteFile(filepath.Join(opts.WorktreeRoot, "F12.txt"), []byte("a\nb\nc\n"), 0644)
		opts.OnOutput([]byte("reading files\n\x1b[32mediting F12.txt\x1b[0m\npartial lin"))
		select {
		case <-release:
		case <-ctx.Done():
		}
		return adapter.RunResult{ExitCode: 0, Output: "ok"}, nil
	})
	if err := ctrl.FrontRunStart(context.Background(), "burn", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrl.FrontRunStop(context.Background()) })
	waitFor(t, "diff and tail to show up", func() bool {
		p := progressFor(ctrl, "F12")
		return p != nil && p.Files == 1 && len(p.Tail) == 3
	})
	p := progressFor(ctrl, "F12")
	if p.Lane != "frontrun" || p.Phase != "agent" {
		t.Errorf("lane/phase = %s/%s", p.Lane, p.Phase)
	}
	if got := strings.Join(p.Tail, "|"); got != "reading files|editing F12.txt|partial lin" {
		t.Errorf("tail = %q (escapes must be stripped, the partial line shown)", got)
	}
	close(release)
	waitFor(t, "F12 done", func() bool {
		rec, _ := ctrl.store.GetChunk("F12")
		return rec != nil && rec.Status == "done"
	})
	var phases []string
	for _, m := range eventsOfType(t, ctrl, "chunk.phase") {
		if strings.HasPrefix(m, "chunk F12: ") {
			phases = append(phases, strings.Fields(m)[2])
		}
	}
	if strings.Join(phases, ",") != "agent,verify,commit" {
		t.Errorf("phase events = %v", phases)
	}
	_ = ctrl.FrontRunStop(context.Background())
	if progressFor(ctrl, "F12") != nil {
		t.Error("progress must disappear once the chunk is over")
	}
}

func TestProgress_SilentAgentIsReportedOnce(t *testing.T) {
	shortProgress(t, 150*time.Millisecond)
	ctrl, _ := frontRunFixture(t, func(ctx context.Context, opts adapter.RunOptions) (adapter.RunResult, error) {
		<-ctx.Done()
		return adapter.RunResult{}, ctx.Err()
	})
	if err := ctrl.FrontRunStart(context.Background(), "burn", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrl.FrontRunStop(context.Background()) })
	waitFor(t, "stall flag", func() bool {
		p := progressFor(ctrl, "F12")
		return p != nil && p.Stalled
	})
	time.Sleep(200 * time.Millisecond)
	if n := len(eventsOfType(t, ctrl, "chunk.stalled")); n != 1 {
		t.Errorf("stall events = %d, want exactly 1", n)
	}
	_ = ctrl.FrontRunStop(context.Background())
}

func TestProgress_TailIsRedacted(t *testing.T) {
	ctrl, _ := setupTestController(t, &mockTestAdapter{})
	ctrl.progBegin("X1", "main")
	ctrl.progOutput("X1")([]byte("token ghp_abcdefghijklmnopqrstuvwxyz0123456789 leaked\n"))
	p := ctrl.progressSnapshot()
	if len(p) != 1 || len(p[0].Tail) != 1 || strings.Contains(p[0].Tail[0], "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("secret reached the dashboard: %+v", p)
	}
}
