package quota

import (
	"strings"
	"testing"
	"time"
)

func TestParseAgyUsage(t *testing.T) {
	raw := "Gemini Models\tFive hour window\t80%\t2026-10-16T22:00:00Z\n" +
		"Gemini Models\tWeekly window\t95%\t2026-10-20T00:00:00Z\n" +
		"Claude and GPT models\tFive hour window\t40%\t2026-10-16T21:30:00Z\n" +
		"Claude and GPT models\tWeekly window\t60%\t2026-10-21T05:00:00Z\n"

	buckets, err := ParseAgyUsage(raw)
	if err != nil {
		t.Fatalf("ParseAgyUsage failed: %v", err)
	}

	if len(buckets) != 4 {
		t.Fatalf("got %d buckets, want 4", len(buckets))
	}

	foundClaudeWeekly := false
	for _, b := range buckets {
		if b.Pool == "claude" && b.Window == "weekly" {
			foundClaudeWeekly = true
			if b.RemainingPct != 60 {
				t.Errorf("claude weekly pct = %d, want 60", b.RemainingPct)
			}
			if b.ResetTime.IsZero() {
				t.Errorf("expected parsed reset time for claude weekly")
			}
		}
	}
	if !foundClaudeWeekly {
		t.Errorf("claude weekly bucket not found")
	}
}

func TestParseAgyUsageTableFormat(t *testing.T) {
	raw := `Quota:
Gemini Models          Weekly Limit Remaining       8%        2026-10-24 08:25 +08
Gemini Models          Five Hour Limit Remaining    46%       2026-10-21 17:39 +08
Claude and GPT models  Weekly Limit Remaining       0%        2026-10-22 11:44 +08
Claude and GPT models  Five Hour Limit Remaining    disabled  
Gemini 4 Argon         Gemini 4 Argon Weekly Limit  1%        2026-10-24 13:49 +08
`
	buckets, err := ParseAgyUsage(raw)
	if err != nil {
		t.Fatalf("ParseAgyUsage table format failed: %v", err)
	}
	if len(buckets) != 4 {
		t.Fatalf("expected 4 active buckets, got %d: %+v", len(buckets), buckets)
	}

	var foundClaude, foundGeminiWeekly bool
	for _, b := range buckets {
		if b.Pool == "claude" && b.Window == "weekly" {
			foundClaude = true
			if b.RemainingPct != 0 {
				t.Errorf("expected claude weekly remaining 0%%, got %d%%", b.RemainingPct)
			}
			if b.ResetTime.IsZero() {
				t.Errorf("expected parsed reset time for claude weekly")
			}
		}
		if b.Pool == "gemini" && b.Window == "weekly" {
			foundGeminiWeekly = true
			if b.RemainingPct != 8 {
				t.Errorf("expected gemini weekly remaining 8%%, got %d%%", b.RemainingPct)
			}
		}
	}
	if !foundClaude {
		t.Errorf("claude weekly bucket not found in table format")
	}
	if !foundGeminiWeekly {
		t.Errorf("gemini weekly bucket not found in table format")
	}
}

func TestParseClaudeUsage(t *testing.T) {
	raw := "Current session: 25% used · resets Oct 16, 6:49pm (UTC)\n" +
		"Current week (all models): 40% used · resets Oct 20, 4:59am (UTC)\n"

	now := time.Date(2026, 10, 16, 14, 0, 0, 0, time.UTC)
	buckets, err := ParseClaudeUsage(raw, now)
	if err != nil {
		t.Fatalf("ParseClaudeUsage failed: %v", err)
	}

	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(buckets))
	}

	for _, b := range buckets {
		if b.Pool != "cc" {
			t.Errorf("got pool %q, want 'cc'", b.Pool)
		}
		if b.Window == "5h" {
			if b.RemainingPct != 75 { // 100 - 25% used
				t.Errorf("5h remaining pct = %d, want 75", b.RemainingPct)
			}
		} else if b.Window == "weekly" {
			if b.RemainingPct != 60 { // 100 - 40% used
				t.Errorf("weekly remaining pct = %d, want 60", b.RemainingPct)
			}
		}
	}
}

func TestPoolState_PolledFloors(t *testing.T) {
	pool := NewPoolState("claude", ModePolled, []int{60, 180})
	pool.FloorWeeklyPct = 25

	now := time.Now()

	// Case 1: Bucket above floor
	pool.UpdateBuckets([]Bucket{
		{Pool: "claude", Window: "weekly", RemainingPct: 30, ResetTime: now.Add(24 * time.Hour)},
	})
	avail, reason := pool.IsAvailable(5, now)
	if !avail {
		t.Errorf("expected pool to be available at 30%% (floor 25%%, cost 5%%), got reason: %s", reason)
	}

	// Case 2: Bucket at or below floor
	pool.UpdateBuckets([]Bucket{
		{Pool: "claude", Window: "weekly", RemainingPct: 24, ResetTime: now.Add(24 * time.Hour)},
	})
	avail2, reason2 := pool.IsAvailable(5, now)
	if avail2 {
		t.Errorf("expected pool to be unavailable below floor 25%%")
	}
	if !strings.Contains(reason2, "floor") {
		t.Errorf("expected reason to mention floor, got: %s", reason2)
	}
}

func TestPoolState_UntilRejectedBackoff(t *testing.T) {
	backoffSteps := []int{60, 180, 300}
	pool := NewPoolState("codex", ModeUntilRejected, backoffSteps)

	now := time.Now()

	// Initially available
	avail, _ := pool.IsAvailable(0, now)
	if !avail {
		t.Errorf("expected until-rejected pool to be initially available")
	}

	// Reject 1: backoff step 0 (60 min)
	pool.RecordRejection(now)
	avail1, reason1 := pool.IsAvailable(0, now.Add(10*time.Minute))
	if avail1 {
		t.Errorf("expected pool to be in backoff after 1st rejection")
	}
	if !strings.Contains(reason1, "cooldown") && !strings.Contains(reason1, "backoff") {
		t.Errorf("expected reason to mention cooldown or backoff, got: %s", reason1)
	}

	// After 61 minutes: available again
	availAfter60, _ := pool.IsAvailable(0, now.Add(61*time.Minute))
	if !availAfter60 {
		t.Errorf("expected pool to be available after backoff elapsed")
	}

	// Reject 2: uses step 1 (180 min) and advances counter to 2
	pool.RecordRejection(now.Add(61 * time.Minute))
	if pool.CurrentBackoffStep != 2 {
		t.Errorf("expected backoff step counter 2, got %d", pool.CurrentBackoffStep)
	}
	expectedPausedUntil := now.Add(61 * time.Minute).Add(180 * time.Minute)
	if !pool.PausedUntil.Equal(expectedPausedUntil) {
		t.Errorf("expected PausedUntil %v, got %v", expectedPausedUntil, pool.PausedUntil)
	}

	// Success resets backoff
	pool.RecordSuccess()
	if pool.CurrentBackoffStep != 0 {
		t.Errorf("expected backoff step reset to 0 after success, got %d", pool.CurrentBackoffStep)
	}
}

func TestCalculateCostMedian(t *testing.T) {
	// Simple median calculation
	samples := []int{5, 7, 3, 10, 4}
	m := Median(samples, 5)
	if m != 5 {
		t.Errorf("Median(%v) = %d, want 5", samples, m)
	}

	// Empty samples fallback to default
	if Median(nil, 6) != 6 {
		t.Errorf("Median(nil) want default 6")
	}
}
