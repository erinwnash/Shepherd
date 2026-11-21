// Package repair implements the repair chain and mechanical-fix tier evaluation.
// Built from scratch: core repair orchestration logic.
package repair

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/erinwnash/Shepherd/internal/config"
)

var tsErrorCodeRegex = regexp.MustCompile(`TS\d+`)

// IsMechanicalFix evaluates whether the verification errors qualify as a small mechanical fix.
// Mechanical fixes are small syntax/type/import errors suitable for a lightweight model (e.g. Haiku).
// Scope violations, content-loss, and protected path errors are never mechanical.
func IsMechanicalFix(errors string, cfg config.MechanicalFixConfig) bool {
	if strings.TrimSpace(errors) == "" {
		return false
	}

	// Never mechanical if scope, content-loss, or protected path gates tripped
	lower := strings.ToLower(errors)
	if strings.Contains(lower, "lost most of their content") ||
		strings.Contains(lower, "files changed that the work order does not list") ||
		strings.Contains(lower, "protected paths modified") ||
		strings.Contains(lower, "work order modified") {
		return false
	}

	// Extract TypeScript or similar error codes
	foundCodes := tsErrorCodeRegex.FindAllString(errors, -1)
	if len(foundCodes) > 0 {
		if len(foundCodes) > cfg.MaxErrors {
			return false
		}
		if len(cfg.Codes) > 0 {
			allowed := make(map[string]bool, len(cfg.Codes))
			for _, c := range cfg.Codes {
				allowed[c] = true
			}
			for _, code := range foundCodes {
				if !allowed[code] {
					return false
				}
			}
		}
		return true
	}

	// If no TS codes matched, count error lines in compiler/gate failure
	errLineCount := 0
	for _, line := range strings.Split(errors, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "error") || strings.Contains(trimmed, "undefined") {
			errLineCount++
		}
	}

	return errLineCount > 0 && errLineCount <= cfg.MaxErrors
}

// ChainStep represents one tier and its round budget in the repair chain.
type ChainStep struct {
	Tier      string
	MaxRounds int
}

// Tracker tracks repair rounds used across tiers for a chunk.
type Tracker struct {
	Steps          []ChainStep
	MechanicalTier string
	used           map[string]int
}

// NewTracker parses chain specifications (e.g. ["haiku:1", "sonnet:2", "opus:1"]) into a Tracker.
func NewTracker(chainSpecs []string, mechanicalTier string) (*Tracker, error) {
	var steps []ChainStep
	for _, spec := range chainSpecs {
		parts := strings.Split(spec, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid repair chain spec %q (want 'tier:rounds')", spec)
		}
		rounds, err := strconv.Atoi(parts[1])
		if err != nil || rounds <= 0 {
			return nil, fmt.Errorf("invalid rounds in repair chain spec %q: %w", spec, err)
		}
		steps = append(steps, ChainStep{Tier: parts[0], MaxRounds: rounds})
	}

	return &Tracker{
		Steps:          steps,
		MechanicalTier: mechanicalTier,
		used:           make(map[string]int),
	}, nil
}

// Next selects the next available tier for repair. If preferMechanical is true and the
// mechanical tier has rounds remaining in the chain, it is selected first.
func (t *Tracker) Next(preferMechanical bool) (string, int, bool) {
	if preferMechanical && t.MechanicalTier != "" {
		for _, step := range t.Steps {
			if step.Tier == t.MechanicalTier {
				used := t.used[step.Tier]
				if used < step.MaxRounds {
					t.used[step.Tier] = used + 1
					return step.Tier, used + 1, true
				}
			}
		}
	}

	// Advance sequentially through the repair chain
	for _, step := range t.Steps {
		used := t.used[step.Tier]
		if used < step.MaxRounds {
			t.used[step.Tier] = used + 1
			return step.Tier, used + 1, true
		}
	}

	return "", 0, false
}

// Refund gives back a round if a tier call could not serve (e.g. quota/capacity error 75).
func (t *Tracker) Refund(tier string) {
	if t.used[tier] > 0 {
		t.used[tier]--
	}
}

// IsSpent reports whether all rounds across all tiers in the repair chain are exhausted.
func (t *Tracker) IsSpent() bool {
	for _, step := range t.Steps {
		if t.used[step.Tier] < step.MaxRounds {
			return false
		}
	}
	return true
}

// FormatRepairPrompt formats the model prompt for a repair round.
func FormatRepairPrompt(id, workOrderBody, verifyErrors string, maxErrLines int) string {
	return FormatRepairPromptWithHistory(id, workOrderBody, verifyErrors, nil, maxErrLines)
}

// IsSemanticFailure detects whether verification errors indicate a logic, assertion, or runtime bug
// (rather than a simple mechanical typo/import fix).
func IsSemanticFailure(errors string) bool {
	lower := strings.ToLower(errors)
	keywords := []string{
		"fail:", "assertionerror", "assert", "panic:", "runtime error",
		"nil pointer", "segmentation fault", "sigsegv", "expected", "want", "got",
		"timed out", "timeout", "index out of range", "fatal error", "tests failed",
	}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// AttemptRecord holds details of a prior execution or repair attempt for a chunk.
type AttemptRecord struct {
	Attempt  int
	Tier     string
	Failures string

	// Handoff fields (zero values mean "legacy record").
	Model        string
	Outcome      string // verify_failed, no_changes, adapter_error, limit_hit
	ChangedFiles []FileChange
	FailedChecks []string
}

// FormatRepairPromptWithHistory formats the repair prompt including cross-model review notes
// from previous attempts (Attempt 1 and Attempt 2).
func FormatRepairPromptWithHistory(id, workOrderBody, verifyErrors string, attempts []AttemptRecord, maxErrLines int) string {
	return FormatRepairPromptWithHandoff(id, workOrderBody, verifyErrors, attempts, maxErrLines, HandoffOptions{})
}

// FormatRepairPromptWithHandoff is FormatRepairPromptWithHistory plus the structured attempt
// record and optional worker note. With opts.Enabled false the output is the legacy prompt.
func FormatRepairPromptWithHandoff(id, workOrderBody, verifyErrors string, attempts []AttemptRecord, maxErrLines int, opts HandoffOptions) string {
	errLines := strings.Split(strings.TrimSpace(verifyErrors), "\n")
	if maxErrLines > 0 && len(errLines) > maxErrLines {
		errLines = errLines[:maxErrLines]
	}
	errorExcerpt := strings.Join(errLines, "\n")

	var historySection strings.Builder
	if opts.Enabled && (len(attempts) > 0 || strings.TrimSpace(opts.Note) != "") {
		historySection.WriteString(renderHistory(attempts, opts))
	} else if len(attempts) > 0 {
		historySection.WriteString("## Cross-Model Diagnostic History & Review Notes\n")
		historySection.WriteString("Earlier model attempts encountered the following failures:\n")
		for _, a := range attempts {
			firstLine := "verification failed"
			lines := strings.Split(strings.TrimSpace(a.Failures), "\n")
			if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
				firstLine = strings.TrimSpace(lines[0])
			}
			historySection.WriteString(fmt.Sprintf("- **Attempt %d (Tier: %s):** %s\n", a.Attempt, a.Tier, firstLine))
		}
		historySection.WriteString("\nCarefully analyze why prior attempts failed so you do not repeat the same mistakes. Address the fundamental root cause.\n\n")
	}

	return fmt.Sprintf("You are repairing chunk %s.\n\n"+
		"## Original Work Order\n%s\n\n"+
		"%s"+
		"## Current Verification Failures\n%s\n\n"+
		"## Instructions\n"+
		"Fix the verification failures above. Make small, precise edits. Touch only the files declared in the work order.\n",
		id, strings.TrimSpace(workOrderBody), historySection.String(), errorExcerpt)
}

// EscalateTier selects a higher-capability model tier when semantic failures occur.
func EscalateTier(currentTier string, isSemantic bool, chain []string, fallbackStrong string) string {
	if !isSemantic {
		return currentTier
	}

	if fallbackStrong == "" {
		fallbackStrong = "strong"
	}

	// If current tier is already strong/frontier, keep it
	if currentTier == fallbackStrong || strings.EqualFold(currentTier, "frontier") || strings.EqualFold(currentTier, "opus") {
		return currentTier
	}

	// Try advancing along the chain
	for i, spec := range chain {
		tier := strings.Split(spec, ":")[0]
		if tier == currentTier && i+1 < len(chain) {
			nextTier := strings.Split(chain[i+1], ":")[0]
			return nextTier
		}
	}

	return fallbackStrong
}

