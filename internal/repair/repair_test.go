package repair

import (
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/config"
)

func TestIsMechanicalFix(t *testing.T) {
	cfg := config.MechanicalFixConfig{
		Tier:      "haiku",
		MaxErrors: 3,
		Codes:     []string{"TS2304", "TS2339"},
	}

	// 1. Content loss is never mechanical
	contentLossErr := "## Files that lost most of their content\nsrc/app.ts: 50 lines removed\n"
	if IsMechanicalFix(contentLossErr, cfg) {
		t.Errorf("content loss must never qualify as mechanical fix")
	}

	// 2. Scope violation is never mechanical
	scopeErr := "## Files changed that the work order does not list\nsrc/evil.ts\n"
	if IsMechanicalFix(scopeErr, cfg) {
		t.Errorf("scope violation must never qualify as mechanical fix")
	}

	// 3. Small compiler errors matching allowed codes
	tsErr := "## Gate failed (typecheck, exit 1)\nsrc/a.ts(10,5): error TS2304: Cannot find name 'foo'.\nsrc/b.ts(12,8): error TS2339: Property 'bar' does not exist.\n"
	if !IsMechanicalFix(tsErr, cfg) {
		t.Errorf("expected 2 matching TS errors to qualify as mechanical fix")
	}

	// 4. Too many errors (> MaxErrors)
	tooManyErr := "## Gate failed (typecheck, exit 1)\n" +
		"src/a.ts: error TS2304: Cannot find name 'a'.\n" +
		"src/b.ts: error TS2304: Cannot find name 'b'.\n" +
		"src/c.ts: error TS2304: Cannot find name 'c'.\n" +
		"src/d.ts: error TS2304: Cannot find name 'd'.\n"
	if IsMechanicalFix(tooManyErr, cfg) {
		t.Errorf("expected 4 errors (> max 3) to not qualify as mechanical fix")
	}

	// 5. Unrecognized error code
	otherErr := "## Gate failed (typecheck, exit 1)\nsrc/a.ts: error TS9999: Unknown syntax error.\n"
	if IsMechanicalFix(otherErr, cfg) {
		t.Errorf("expected unlisted error code TS9999 to not qualify as mechanical fix")
	}
}

func TestChainTracker_Progression(t *testing.T) {
	chainSpecs := []string{"haiku:1", "sonnet:2", "opus:1"}
	tracker, err := NewTracker(chainSpecs, "haiku")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}

	// Mechanical fix preferred when requested
	tier, round, ok := tracker.Next(true)
	if !ok || tier != "haiku" || round != 1 {
		t.Fatalf("expected haiku:1, got tier=%q round=%d ok=%v", tier, round, ok)
	}

	// Haiku has used its 1 round; next mechanical request falls back to chain (sonnet)
	tier, round, ok = tracker.Next(true)
	if !ok || tier != "sonnet" || round != 1 {
		t.Fatalf("expected sonnet:1, got tier=%q round=%d ok=%v", tier, round, ok)
	}

	// Next non-mechanical request gets sonnet:2
	tier, round, ok = tracker.Next(false)
	if !ok || tier != "sonnet" || round != 2 {
		t.Fatalf("expected sonnet:2, got tier=%q round=%d ok=%v", tier, round, ok)
	}

	// Next gets opus:1
	tier, round, ok = tracker.Next(false)
	if !ok || tier != "opus" || round != 1 {
		t.Fatalf("expected opus:1, got tier=%q round=%d ok=%v", tier, round, ok)
	}

	// Now all rounds exhausted
	tier, round, ok = tracker.Next(false)
	if ok {
		t.Fatalf("expected chain to be spent, but got tier=%q round=%d", tier, round)
	}
	if !tracker.IsSpent() {
		t.Errorf("tracker.IsSpent() want true, got false")
	}
}

func TestChainTracker_Refund(t *testing.T) {
	chainSpecs := []string{"sonnet:1"}
	tracker, err := NewTracker(chainSpecs, "")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}

	tier, _, ok := tracker.Next(false)
	if !ok || tier != "sonnet" {
		t.Fatalf("expected sonnet")
	}

	// Now spent
	if !tracker.IsSpent() {
		t.Fatalf("expected tracker to be spent")
	}

	// Refund round (e.g. tier capacity error 75)
	tracker.Refund("sonnet")
	if tracker.IsSpent() {
		t.Fatalf("expected tracker to not be spent after refund")
	}

	// Can run sonnet again
	tier2, _, ok2 := tracker.Next(false)
	if !ok2 || tier2 != "sonnet" {
		t.Fatalf("expected sonnet again after refund")
	}
}

func TestFormatRepairPrompt(t *testing.T) {
	woBody := "## Goal\nImplement feature\n## Files\n`src/feat.go`\n"
	errors := "## Gate failed (typecheck, exit 1)\nundefined: foo\n"

	prompt := FormatRepairPrompt("01A", woBody, errors, 25)
	if !strings.Contains(prompt, "01A") {
		t.Errorf("expected prompt to mention chunk id 01A")
	}
	if !strings.Contains(prompt, "undefined: foo") {
		t.Errorf("expected prompt to contain error text")
	}
	if !strings.Contains(prompt, "Fix the verification failures") {
		t.Errorf("expected prompt to contain repair instructions")
	}
}

func TestIsSemanticFailure(t *testing.T) {
	// Semantic failure cases
	semanticErrors := []string{
		"--- FAIL: TestUserAuth (0.05s)",
		"AssertionError: expected status 200, got 500",
		"panic: runtime error: invalid memory address or nil pointer dereference",
		"test timed out after 30s",
	}
	for _, errStr := range semanticErrors {
		if !IsSemanticFailure(errStr) {
			t.Errorf("expected %q to be identified as semantic failure", errStr)
		}
	}

	// Non-semantic / purely formatting errors
	nonSemantic := "go.mod: line 3: syntax error"
	if IsSemanticFailure(nonSemantic) {
		t.Errorf("syntax error should not be flagged as semantic failure")
	}
}

func TestEscalateTier(t *testing.T) {
	chain := []string{"haiku:1", "sonnet:2", "opus:1"}

	// Mechanical failure: stays on current tier
	if got := EscalateTier("haiku", false, chain, "strong"); got != "haiku" {
		t.Errorf("mechanical failure should not escalate, got: %s", got)
	}

	// Semantic failure on haiku: escalates to sonnet
	if got := EscalateTier("haiku", true, chain, "strong"); got != "sonnet" {
		t.Errorf("semantic failure on haiku should escalate to sonnet, got: %s", got)
	}

	// Semantic failure on sonnet: escalates to opus
	if got := EscalateTier("sonnet", true, chain, "strong"); got != "opus" {
		t.Errorf("semantic failure on sonnet should escalate to opus, got: %s", got)
	}

	// Unknown tier with semantic failure: falls back to strong
	if got := EscalateTier("unknown-tier", true, chain, "strong"); got != "strong" {
		t.Errorf("unknown tier with semantic failure should fall back to strong, got: %s", got)
	}
}

func TestFormatRepairPromptWithHistory(t *testing.T) {
	history := []AttemptRecord{
		{Attempt: 1, Tier: "haiku", Failures: "--- FAIL: TestTokenMismatch\nwant token abc, got xyz"},
		{Attempt: 2, Tier: "sonnet", Failures: "panic: nil pointer dereference in auth.go:42"},
	}

	prompt := FormatRepairPromptWithHistory("03B", "## Goal\nAuth\n", "panic: still nil", history, 20)
	if !strings.Contains(prompt, "Cross-Model Diagnostic History") {
		t.Errorf("expected diagnostic history section in prompt")
	}
	if !strings.Contains(prompt, "Attempt 1 (Tier: haiku)") {
		t.Errorf("expected Attempt 1 details in prompt")
	}
	if !strings.Contains(prompt, "Attempt 2 (Tier: sonnet)") {
		t.Errorf("expected Attempt 2 details in prompt")
	}
}

