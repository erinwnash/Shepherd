package quota

import (
	"testing"
	"time"
)

func TestDetectLimit(t *testing.T) {
	now := time.Date(2026, 10, 25, 4, 41, 0, 0, time.UTC) // 12:41 in Singapore
	ok, until := DetectLimit("You've hit your session limit · resets 11pm (Asia/Singapore)", now)
	if !ok {
		t.Fatal("expected a limit")
	}
	if want := time.Date(2026, 10, 25, 15, 0, 0, 0, time.UTC); !until.Equal(want) {
		t.Fatalf("until = %v, want %v", until, want)
	}
	ok, until = DetectLimit("You've hit your usage limit", now)
	if !ok || !until.Equal(now.Add(DefaultLimitCooldown)) {
		t.Fatalf("no reset time: ok=%v until=%v", ok, until)
	}
	if ok, _ := DetectLimit("compile error: unexpected token", now); ok {
		t.Fatal("ordinary failure misread as a limit")
	}
	// A reset time already past rolls to the next day.
	_, until = DetectLimit("session limit resets 12pm (Asia/Singapore)", now)
	if want := time.Date(2026, 10, 26, 4, 0, 0, 0, time.UTC); !until.Equal(want) {
		t.Fatalf("rolled until = %v, want %v", until, want)
	}
}
