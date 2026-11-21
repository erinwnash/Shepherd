package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/control"
)

type fakeFront struct {
	calls []string
	err   error
}

func (f *fakeFront) FrontRunStart(_ context.Context, tier, model string) error {
	f.calls = append(f.calls, "start "+tier+"/"+model)
	return f.err
}
func (f *fakeFront) FrontRunStop(context.Context) error {
	f.calls = append(f.calls, "stop")
	return f.err
}

func frontState() *DashboardState {
	st := &DashboardState{ActiveTab: TabStack}
	st.StackItems = []StackItem{
		{Tier: "local", Adapter: "opencode", Model: "qwen", Enabled: true},
		{Tier: "strong", Adapter: "agy", Model: "big-model", Enabled: false}, // switched off
	}
	st.SelectedStackIdx = 1
	return st
}

func TestTUI_FrontRun_HotkeyAsksThenStartsOnTheSelectedOffModel(t *testing.T) {
	if mapByteToAction('y') != ActionFrontRun {
		t.Fatal("y should be the front-run hotkey")
	}
	st, api := frontState(), &fakeFront{}
	handled, _ := handleFrontRunAction(context.Background(), api, st, ActionFrontRun)
	if !handled || !st.FrontRunConfirm || len(api.calls) != 0 {
		t.Fatalf("first press must only ask: handled=%v confirm=%v calls=%v", handled, st.FrontRunConfirm, api.calls)
	}
	out := StripAnsi(Render(*st, 120, 40))
	if !strings.Contains(out, "front run with strong/big-model?") {
		t.Fatalf("dialog text missing:\n%s", out)
	}
	handled, _ = handleFrontRunAction(context.Background(), api, st, ActionConfirm)
	if !handled || st.FrontRunConfirm || len(api.calls) != 1 || api.calls[0] != "start strong/big-model" {
		t.Fatalf("confirm should start the lane: confirm=%v calls=%v", st.FrontRunConfirm, api.calls)
	}
}

func TestTUI_FrontRun_AnyOtherKeyCancels(t *testing.T) {
	st, api := frontState(), &fakeFront{}
	_, _ = handleFrontRunAction(context.Background(), api, st, ActionFrontRun)
	handled, quit := handleFrontRunAction(context.Background(), api, st, ActionCancel)
	if !handled || quit || st.FrontRunConfirm || len(api.calls) != 0 {
		t.Fatalf("cancel: handled=%v quit=%v confirm=%v calls=%v", handled, quit, st.FrontRunConfirm, api.calls)
	}
}

func TestTUI_FrontRun_StopsFromTheSamePageWithoutAsking(t *testing.T) {
	st, api := frontState(), &fakeFront{}
	st.Status.FrontRun = &control.FrontRunStatus{Tier: "strong", Model: "big-model", Current: "F12", Landed: 2, Failed: 1}
	out := StripAnsi(Render(*st, 120, 40))
	for _, want := range []string{"FRONT-RUN", "strong/big-model", "F12", "landed 2", "[y] stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q:\n%s", want, out)
		}
	}
	handled, _ := handleFrontRunAction(context.Background(), api, st, ActionFrontRun)
	if !handled || len(api.calls) != 1 || api.calls[0] != "stop" {
		t.Fatalf("y while running should stop: %v", api.calls)
	}
}

func TestTUI_FrontRun_OnlyAsksOnTheStackPageAndReportsErrors(t *testing.T) {
	st, api := frontState(), &fakeFront{}
	st.ActiveTab = TabQueue
	if handled, _ := handleFrontRunAction(context.Background(), api, st, ActionFrontRun); handled || st.FrontRunConfirm {
		t.Fatal("y must not open the dialog outside the stack page")
	}
	st.ActiveTab = TabStack
	api.err = errors.New("unknown tier")
	_, _ = handleFrontRunAction(context.Background(), api, st, ActionFrontRun)
	_, _ = handleFrontRunAction(context.Background(), api, st, ActionConfirm)
	if !strings.Contains(st.ActionErr, "unknown tier") {
		t.Errorf("start failure should surface, got %q", st.ActionErr)
	}
}

func TestTUI_FrontRun_OverviewShowsTheLane(t *testing.T) {
	st := DashboardState{ActiveTab: TabOverview}
	st.Status.State = "running"
	st.Status.FrontRun = &control.FrontRunStatus{Tier: "strong", Model: "big-model", Note: "waiting for RAM: 1.0 GB free, need 6.0 GB"}
	out := StripAnsi(Render(st, 120, 40))
	if !strings.Contains(out, "FRONT-RUN") || !strings.Contains(out, "waiting for RAM") {
		t.Fatalf("overview should show the lane and what it waits for:\n%s", out)
	}
}
