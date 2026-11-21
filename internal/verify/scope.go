package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/erinwnash/Shepherd/internal/scope"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

// ScopeResult captures the outcome of scope and protected path checking.
type ScopeResult struct {
	Passed       bool
	Errors       string
	Violations   []string
	Tampered        bool
	ProtectedHit    []string
	ReadOnlyTestHit []string
}

// CheckScope verifies that a chunk changed only the files it declared, that no protected
// paths were modified, and that the work order itself was not tampered with.
func CheckScope(wo *workorder.WorkOrder, currentWorkOrderBytes []byte, changedPaths []string, protected scope.Protected) ScopeResult {
	res := ScopeResult{Passed: true}
	var errSections []string

	// 1. Work order tamper check (Threat Model T1)
	if currentWorkOrderBytes != nil {
		norm := strings.ReplaceAll(string(bytes.TrimPrefix(currentWorkOrderBytes, []byte("\xef\xbb\xbf"))), "\r\n", "\n")
		sum := sha256.Sum256([]byte(norm))
		h := hex.EncodeToString(sum[:])
		if h != wo.Hash {
			res.Passed = false
			res.Tampered = true
			errSections = append(errSections, "## Work order modified\nThe work order on disk differs from its queue-time copy (hash mismatch). A model may not edit its own work order or widen its scope.\n")
		}
	}

	woPathLower := strings.ToLower(wo.Path)
	for _, p := range changedPaths {
		if strings.ToLower(p) == woPathLower {
			res.Passed = false
			res.Tampered = true
			errSections = append(errSections, fmt.Sprintf("## Work order modified\nThe work order %q is in changed files. A model may not edit its own work order or widen its scope.\n", wo.Path))
			break
		}
	}

	// 2. Protected paths check (Threat Model T2)
	for _, p := range changedPaths {
		if protected.Is(p) {
			res.Passed = false
			res.ProtectedHit = append(res.ProtectedHit, p)
		}
	}
	if len(res.ProtectedHit) > 0 {
		errSections = append(errSections, "## Protected paths modified\nThe following protected files were changed and may never be modified by any chunk:\n"+
			strings.Join(res.ProtectedHit, "\n")+"\nRevert these files immediately.\n")
	}

	// 3. Declared files and tests check (Threat Model T1, T3)
	for _, p := range changedPaths {
		// Ignore if it's already reported as protected
		if protected.Is(p) {
			continue
		}

		if wo.ReadOnlyTests {
			isTest := false
			for _, t := range wo.Tests {
				if scope.Match(t, p) {
					isTest = true
					break
				}
			}
			if isTest {
				res.ReadOnlyTestHit = append(res.ReadOnlyTestHit, p)
				continue
			}
		}

		covered := false
		for _, f := range wo.Files {
			if scope.Match(f.Path, p) {
				covered = true
				break
			}
		}
		if !covered && !wo.ReadOnlyTests {
			for _, t := range wo.Tests {
				if scope.Match(t, p) {
					covered = true
					break
				}
			}
		}
		if !covered {
			res.Violations = append(res.Violations, p)
		}
	}

	if len(res.ReadOnlyTestHit) > 0 {
		res.Passed = false
		errSections = append(errSections, "## Read-only test files modified (TDD anti-cheating)\nThe following test files are marked read-only and may not be modified or deleted:\n"+
			strings.Join(res.ReadOnlyTestHit, "\n")+"\nYou must satisfy the test assertions by fixing the implementation code, not by altering test assertions.\n")
	}

	if len(res.Violations) > 0 {
		res.Passed = false
		errSections = append(errSections, FormatScopeErrors(res.Violations))
	}

	res.Errors = strings.Join(errSections, "\n")
	return res
}

// FormatScopeErrors formats scope violations into the model prompt format.
func FormatScopeErrors(violations []string) string {
	if len(violations) == 0 {
		return ""
	}
	return "## Files changed that the work order does not list (only the files under 'files' and 'tests' may change)\n" +
		strings.Join(violations, "\n") +
		"\nRevert every one of these files to its original content (for a new file, delete it). Do not touch anything else.\n"
}
