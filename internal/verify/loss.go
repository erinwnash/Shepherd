// Package verify implements verification gates for chunk changes.
// Built from scratch: core verification logic (content-loss, scope enforcement, argv-only gates).
package verify

import (
	"bytes"
	"fmt"
	"strings"
)

// NumstatEntry holds git diff numstat information for one file.
type NumstatEntry struct {
	Path    string
	Added   int
	Deleted int
	Binary  bool
}

// CheckNumstatLoss detects files that lost a large share of their lines (e.g. a partial file
// written over the original). Formula: deleted >= 25 && deleted > added * 2 + 10.
func CheckNumstatLoss(entries []NumstatEntry) []string {
	var out []string
	for _, e := range entries {
		if e.Binary {
			continue
		}
		if e.Deleted >= 25 && e.Deleted > e.Added*2+10 {
			out = append(out, fmt.Sprintf("%s: %d lines removed, only %d added", e.Path, e.Deleted, e.Added))
		}
	}
	return out
}

// FormatContentLossErrors formats content loss warnings for model repair prompts.
func FormatContentLossErrors(losses []string) string {
	if len(losses) == 0 {
		return ""
	}
	return "## Files that lost most of their content (a partial file was written over the original)\n" +
		strings.Join(losses, "\n") +
		"\nRestore every removed line that the work order did not ask you to remove. Re-apply your change as small edits that touch only the lines the work order names.\n"
}

// CompareNormalizedLines compares two byte slices line-by-line with CRLF and UTF-8 BOM
// normalized. Returns line additions and deletions.
func CompareNormalizedLines(base, current []byte) (added, deleted int) {
	normBase := normalizeText(base)
	normCurrent := normalizeText(current)

	if normBase == normCurrent {
		return 0, 0
	}

	baseLines := splitLines(normBase)
	currLines := splitLines(normCurrent)

	// Standard LCS (longest common subsequence) for line comparison
	n := len(baseLines)
	m := len(currLines)

	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}

	for i := 0; i < n; i++ {
		for j := 0; j < m; j++ {
			if baseLines[i] == currLines[j] {
				dp[i+1][j+1] = dp[i][j] + 1
			} else if dp[i+1][j] > dp[i][j+1] {
				dp[i+1][j+1] = dp[i+1][j]
			} else {
				dp[i+1][j+1] = dp[i][j+1]
			}
		}
	}

	lcs := dp[n][m]
	deleted = n - lcs
	added = m - lcs
	return added, deleted
}

func normalizeText(b []byte) string {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return s
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
