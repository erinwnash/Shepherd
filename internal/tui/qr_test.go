package tui

import (
	"context"
	"strings"
	"testing"
)

const qrTopicA = "https://ntfy.sh/example-batch-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
const qrTopicB = "https://ntfy.sh/example-batch-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"

func qrState0(topic string) DashboardState {
	st := DashboardState{ActiveTab: TabOverview}
	st.Status.State = "running"
	st.Status.NtfyTopic = topic
	return st
}

func TestTUI_QR_OnByDefaultAndFitsEveryLayout(t *testing.T) {
	for _, size := range [][2]int{{140, 40}, {100, 60}} { // side by side, stacked
		st := qrState0(qrTopicA)
		out := Render(st, size[0], size[1])
		plain := StripAnsi(out)
		if !strings.Contains(plain, "NTFY QR") {
			t.Fatalf("%dx%d: QR panel missing by default", size[0], size[1])
		}
		lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
		if len(lines) > size[1] {
			t.Errorf("%dx%d: %d lines", size[0], size[1], len(lines))
		}
		for _, l := range lines {
			if n := VisibleLen(l); n > size[0] {
				t.Errorf("%dx%d: line is %d wide: %q", size[0], size[1], n, l)
			}
		}
		if !strings.Contains(lines[len(lines)-1], "ntfy:") {
			t.Errorf("%dx%d: ntfy line should stay last, got %q", size[0], size[1], lines[len(lines)-1])
		}
	}
}

func TestTUI_QR_TooSmallTerminalLeavesItOut(t *testing.T) {
	st := qrState0(qrTopicA)
	if out := StripAnsi(Render(st, 80, 24)); strings.Contains(out, "NTFY QR") {
		t.Fatalf("QR must not squeeze into a small terminal:\n%s", out)
	}
}

func TestTUI_QR_Toggle(t *testing.T) {
	st := &DashboardState{ActiveTab: TabOverview}
	st.Status.NtfyTopic = qrTopicA
	if mapByteToAction('i') != ActionQRToggle {
		t.Fatal("i should toggle the QR code")
	}
	ExecuteAction(context.Background(), nil, st, ActionQRToggle)
	if !st.QRHidden || strings.Contains(StripAnsi(Render(*st, 140, 40)), "NTFY QR") {
		t.Fatal("QR should be hidden after one toggle")
	}
	ExecuteAction(context.Background(), nil, st, ActionQRToggle)
	if st.QRHidden || !strings.Contains(StripAnsi(Render(*st, 140, 40)), "NTFY QR") {
		t.Fatal("QR should be back after a second toggle")
	}
}

// A rotated topic arrives through Status.NtfyTopic; the panel must encode the new one.
func TestTUI_QR_FollowsTopicRotation(t *testing.T) {
	a, b := qrState0(qrTopicA), qrState0(qrTopicB)
	if Render(a, 140, 40) == Render(b, 140, 40) {
		t.Fatal("QR did not change when the topic changed")
	}
	if strings.Join(renderQR(qrTopicA), "") == strings.Join(renderQR(qrTopicB), "") {
		t.Fatal("different topics encoded to the same code")
	}
}

func TestTUI_QR_NoTopicNoPanel(t *testing.T) {
	st := qrState0("")
	if box, _ := qrBox(st); box != nil {
		t.Fatal("no topic, no QR panel")
	}
}
