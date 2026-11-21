package quota

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	limitRegex = regexp.MustCompile(`(?i)(hit your (session |usage |weekly |5-hour |five-hour )?limit|session limit|usage limit (reached|exceeded)|reached your (session |usage |weekly )?limit)`)
	// "resets 11pm (Asia/Singapore)", "resets 6:49pm (UTC)", "resets Oct 16, 6:49pm (UTC)".
	limitResetRegex = regexp.MustCompile(`(?i)resets\s+(?:([A-Za-z]{3})\s+(\d{1,2}),\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(([^)]*)\)`)
)

// DefaultLimitCooldown applies when a limit message carries no parsable reset time.
const DefaultLimitCooldown = 30 * time.Minute

// DetectLimit reports whether text is a provider's "you hit your session/usage limit"
// rejection and, when it names one, when the limit resets. A message without a parsable
// reset time yields now + DefaultLimitCooldown.
func DetectLimit(text string, now time.Time) (bool, time.Time) {
	if !limitRegex.MatchString(text) {
		return false, time.Time{}
	}
	m := limitResetRegex.FindStringSubmatch(text)
	if m == nil {
		return true, now.Add(DefaultLimitCooldown)
	}
	loc, err := time.LoadLocation(m[6])
	if err != nil {
		loc = time.UTC
	}
	hour, _ := strconv.Atoi(m[3])
	min := 0
	if m[4] != "" {
		min, _ = strconv.Atoi(m[4])
	}
	hour %= 12
	if strings.EqualFold(m[5], "pm") {
		hour += 12
	}
	local := now.In(loc)
	at := time.Date(local.Year(), local.Month(), local.Day(), hour, min, 0, 0, loc)
	if m[1] != "" {
		if mt, perr := time.ParseInLocation("Jan 2", m[1]+" "+m[2], loc); perr == nil {
			at = time.Date(local.Year(), mt.Month(), mt.Day(), hour, min, 0, 0, loc)
		}
	}
	if !at.After(now) {
		at = at.AddDate(0, 0, 1)
	}
	return true, at.UTC()
}
