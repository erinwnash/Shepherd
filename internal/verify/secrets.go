package verify

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/erinwnash/Shepherd/internal/redact"
)

// SecretFinding describes one leaked credential or private key detected in a changed file.
type SecretFinding struct {
	File    string
	Line    int
	Rule    string
	Preview string
}

type secretRule struct {
	name    string
	pattern *regexp.Regexp
}

var secretRules = []secretRule{
	{
		name:    "private-key",
		pattern: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	},
	{
		name:    "api-key-sk",
		pattern: regexp.MustCompile(`\bsk-(?:ant-|proj-)?[a-zA-Z0-9_\-]{20,}\b`),
	},
	{
		name:    "github-pat",
		pattern: regexp.MustCompile(`\b(?:gh[pousr]_[a-zA-Z0-9]{36,}|github_pat_[a-zA-Z0-9_]{40,})\b`),
	},
	{
		name:    "aws-access-key",
		pattern: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	},
	{
		name:    "gcp-api-key",
		pattern: regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35}\b`),
	},
	{
		name:    "slack-webhook-or-token",
		pattern: regexp.MustCompile(`\bxox[baprs]-[0-9a-zA-Z\-]{20,}\b`),
	},
}

// CheckSecrets scans changed files in worktreeRoot for accidentally committed credentials or keys.
func CheckSecrets(worktreeRoot string, changedPaths []string) []SecretFinding {
	var findings []SecretFinding

	for _, rel := range changedPaths {
		full := filepath.Join(worktreeRoot, filepath.FromSlash(rel))
		f, err := os.Open(full)
		if err != nil {
			continue // deleted or unreadable file
		}

		scanner := bufio.NewScanner(f)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			for _, rule := range secretRules {
				if rule.pattern.MatchString(line) {
					findings = append(findings, SecretFinding{
						File:    rel,
						Line:    lineNo,
						Rule:    rule.name,
						Preview: redact.Sanitize(strings.TrimSpace(line)),
					})
					break
				}
			}
		}
		_ = f.Close()
	}

	return findings
}

// FormatSecretErrors produces a concise error report for secret scanner gate failures.
func FormatSecretErrors(findings []SecretFinding) string {
	var sb strings.Builder
	sb.WriteString("## Gate failed (secrets: credential/key leak detected)\n")
	for _, f := range findings {
		fmt.Fprintf(&sb, "- %s:%d [%s]: %s\n", f.File, f.Line, f.Rule, f.Preview)
	}
	return sb.String()
}
