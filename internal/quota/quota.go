// Package quota manages quota tracking, parsing, and pool availability.
// Built from scratch: core quota engine supporting polled and until-rejected pools.
package quota

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ModePolled        = "polled"
	ModeUntilRejected = "until-rejected"
)

// Bucket represents one quota window measurement for a pool.
type Bucket struct {
	Pool         string
	Window       string
	RemainingPct int
	ResetTime    time.Time
}

var multiSpaceRegex = regexp.MustCompile(`\s{2,}`)

func parseAgyResetTime(resetStr string) time.Time {
	if resetStr == "" {
		return time.Time{}
	}
	// Try standard RFC3339 first
	if t, err := time.Parse(time.RFC3339, resetStr); err == nil {
		return t.UTC()
	}
	// Try common CLI date layouts: e.g. "2026-10-24 08:25 +08", "2026-10-24 08:25:00 +0800"
	layouts := []string{
		"2006-01-02 15:04 -07",
		"2006-01-02 15:04 -0700",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, resetStr); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ParseAgyUsage parses tab-separated or column-aligned output from `agy -p /usage`:
// <group>\t<bucket name>\t<remaining %>\t<reset time UTC>
func ParseAgyUsage(text string) ([]Bucket, error) {
	var buckets []Bucket
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "Quota:") {
			continue
		}

		var parts []string
		if strings.Contains(line, "\t") {
			parts = strings.Split(line, "\t")
		} else {
			parts = multiSpaceRegex.Split(trimmed, -1)
		}
		if len(parts) < 3 {
			continue
		}
		group := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		pctRaw := strings.TrimSpace(strings.TrimSuffix(parts[2], "%"))
		resetStr := ""
		if len(parts) >= 4 {
			resetStr = strings.TrimSpace(parts[3])
		}

		var pool string
		switch {
		case strings.Contains(group, "Gemini"):
			pool = "gemini"
		case strings.Contains(group, "Claude"):
			pool = "claude"
		case strings.Contains(group, "GPT"):
			pool = "codex"
		default:
			continue
		}

		var win string
		switch {
		case strings.Contains(name, "Argon"):
			win = "weekly-argon"
		case strings.Contains(name, "Weekly"):
			win = "weekly"
		case strings.Contains(name, "Five"):
			win = "5h"
		default:
			continue
		}

		pct, err := strconv.Atoi(pctRaw)
		if err != nil {
			continue
		}

		resetTime := parseAgyResetTime(resetStr)

		buckets = append(buckets, Bucket{
			Pool:         pool,
			Window:       win,
			RemainingPct: pct,
			ResetTime:    resetTime,
		})
	}

	return buckets, nil
}

var claudeUsedRegex = regexp.MustCompile(`:\s*(\d+)%\s*used`)

// ParseClaudeUsage parses the text output of `claude -p /usage`:
// Current session: 25% used · resets Oct 16, 6:49pm (UTC)
// Current week (all models): 40% used · resets Oct 20, 4:59am (UTC)
func ParseClaudeUsage(text string, now time.Time) ([]Bucket, error) {
	var buckets []Bucket
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	for _, line := range lines {
		var win string
		switch {
		case strings.HasPrefix(line, "Current session:"):
			win = "5h"
		case strings.HasPrefix(line, "Current week"):
			win = "weekly"
		default:
			continue
		}

		m := claudeUsedRegex.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		used, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}

		remaining := 100 - used
		if remaining < 0 {
			remaining = 0
		}

		resetTime := parseClaudeReset(line, now)

		buckets = append(buckets, Bucket{
			Pool:         "cc",
			Window:       win,
			RemainingPct: remaining,
			ResetTime:    resetTime,
		})
	}

	return buckets, nil
}

var claudeResetRegex = regexp.MustCompile(`resets\s+([A-Za-z]{3}\s+\d{1,2},\s+\d{1,2}:\d{2}(?:am|pm)?|\d{1,2}:\d{2}(?:am|pm)?)\s*\(([^)]*)\)`)

func parseClaudeReset(line string, now time.Time) time.Time {
	m := claudeResetRegex.FindStringSubmatch(line)
	if len(m) < 3 {
		return time.Time{}
	}
	whenStr := m[1]
	tzStr := m[2]

	loc, err := time.LoadLocation(tzStr)
	if err != nil {
		loc = time.UTC
	}

	// Try format "Oct 16, 6:49pm"
	layoutWithDate := "Jan 2, 3:04pm"
	t, err := time.ParseInLocation(layoutWithDate, whenStr, loc)
	if err == nil {
		year := now.In(loc).Year()
		candidate := time.Date(year, t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if candidate.Before(now.Add(-24 * time.Hour)) {
			candidate = candidate.AddDate(1, 0, 0)
		}
		return candidate.UTC()
	}

	// Try format "6:49pm" (today)
	layoutTimeOnly := "3:04pm"
	tOnly, err := time.ParseInLocation(layoutTimeOnly, whenStr, loc)
	if err == nil {
		today := now.In(loc)
		candidate := time.Date(today.Year(), today.Month(), today.Day(), tOnly.Hour(), tOnly.Minute(), 0, 0, loc)
		if candidate.Before(now) {
			candidate = candidate.AddDate(0, 0, 1)
		}
		return candidate.UTC()
	}

	return time.Time{}
}

// PoolState tracks live capacity, limits, and cooldowns for a quota pool.
type PoolState struct {
	Name               string
	Mode               string
	FloorWeeklyPct     int
	FloorFiveHourPct   int
	BackoffMin         []int
	CurrentBackoffStep int
	PausedUntil        time.Time
	Disabled           bool
	Buckets            map[string]Bucket
}

// NewPoolState creates a new PoolState instance.
func NewPoolState(name, mode string, backoffMin []int) *PoolState {
	return &PoolState{
		Name:       name,
		Mode:       mode,
		BackoffMin: backoffMin,
		Buckets:    make(map[string]Bucket),
	}
}

// UpdateBuckets updates the pool's cached window readings.
func (p *PoolState) UpdateBuckets(readings []Bucket) {
	for _, b := range readings {
		if b.Pool == p.Name {
			p.Buckets[b.Window] = b
		}
	}
}

// IsAvailable determines if the pool can serve a chunk given its estimated cost.
func (p *PoolState) IsAvailable(estimatedCostPct int, now time.Time) (bool, string) {
	if p.Disabled {
		return false, fmt.Sprintf("pool %q is disabled", p.Name)
	}

	if now.Before(p.PausedUntil) {
		remainingCooldown := p.PausedUntil.Sub(now).Round(time.Minute)
		return false, fmt.Sprintf("pool %q is in cooldown (backoff) for %v", p.Name, remainingCooldown)
	}

	if p.Mode == ModeUntilRejected {
		return true, ""
	}

	// For polled pools: check floors and remaining capacity
	weekly, hasWeekly := p.Buckets["weekly"]
	if hasWeekly {
		if p.FloorWeeklyPct > 0 && weekly.RemainingPct <= p.FloorWeeklyPct {
			return false, fmt.Sprintf("pool %q weekly bucket (%d%%) is at or below floor (%d%%)", p.Name, weekly.RemainingPct, p.FloorWeeklyPct)
		}
		if estimatedCostPct > 0 && weekly.RemainingPct < estimatedCostPct {
			return false, fmt.Sprintf("pool %q weekly bucket (%d%%) cannot cover estimated cost (%d%%)", p.Name, weekly.RemainingPct, estimatedCostPct)
		}
	}

	fiveHour, hasFiveHour := p.Buckets["5h"]
	if hasFiveHour {
		if p.FloorFiveHourPct > 0 && fiveHour.RemainingPct <= p.FloorFiveHourPct {
			return false, fmt.Sprintf("pool %q 5h bucket (%d%%) is at or below floor (%d%%)", p.Name, fiveHour.RemainingPct, p.FloorFiveHourPct)
		}
		if estimatedCostPct > 0 && fiveHour.RemainingPct < estimatedCostPct {
			return false, fmt.Sprintf("pool %q 5h bucket (%d%%) cannot cover estimated cost (%d%%)", p.Name, fiveHour.RemainingPct, estimatedCostPct)
		}
	}

	return true, ""
}

// RecordRejection triggers backoff cooldown progression when a call is rejected.
func (p *PoolState) RecordRejection(now time.Time) {
	if len(p.BackoffMin) == 0 {
		p.PausedUntil = now.Add(30 * time.Minute)
		return
	}
	idx := p.CurrentBackoffStep
	if idx >= len(p.BackoffMin) {
		idx = len(p.BackoffMin) - 1
	}
	cooldown := time.Duration(p.BackoffMin[idx]) * time.Minute
	p.PausedUntil = now.Add(cooldown)

	if p.CurrentBackoffStep+1 < len(p.BackoffMin) {
		p.CurrentBackoffStep++
	}
}

// RecordSuccess resets the backoff state upon a successful call.
func (p *PoolState) RecordSuccess() {
	p.CurrentBackoffStep = 0
	p.PausedUntil = time.Time{}
}

// Median computes the lower median of a slice of integers.
func Median(values []int, defaultVal int) int {
	if len(values) == 0 {
		return defaultVal
	}
	sorted := make([]int, len(values))
	copy(sorted, values)
	sort.Ints(sorted)
	return sorted[(len(sorted)-1)/2]
}
