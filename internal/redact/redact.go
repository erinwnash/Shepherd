// Package redact provides pattern and exact-value secret redaction across all sinks per STRIDE I1.
// Protects API keys, tokens, private keys, and passwords from leaking into transcripts, logs,
// events, notifications, and the terminal dashboard.
package redact

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// RedactionMarker is substituted in place of detected secrets.
const RedactionMarker = "[REDACTED]"

var defaultPatterns = []*regexp.Regexp{
	// OpenAI / Anthropic / Generic API keys
	regexp.MustCompile(`(?i)\b(sk-(?:ant-|proj-)?[a-zA-Z0-9_\-]{20,})\b`),
	// GitHub personal access tokens
	regexp.MustCompile(`\b(gh[pousr]_[a-zA-Z0-9]{36,})\b`),
	regexp.MustCompile(`\b(github_pat_[a-zA-Z0-9_]{40,})\b`),
	// Bearer authorization tokens
	regexp.MustCompile(`(?i)\bBearer\s+([a-zA-Z0-9_\-\.]{20,})\b`),
	// Private key blocks
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	// Generic password or secret assignments in configs/env (e.g. PASSWORD=xyz, SECRET_KEY=xyz)
	regexp.MustCompile(`(?i)(api[_-]?key|secret|password|token|auth)\s*[:=]\s*["']?([a-zA-Z0-9_\-!@#$%^&*]{8,})["']?`),
}

// Redactor manages exact secret values and regex patterns for sanitization.
type Redactor struct {
	mu       sync.RWMutex
	exact    map[string]struct{}
	patterns []*regexp.Regexp
}

// Global default redactor.
var global = New()

// New initializes an empty Redactor loaded with default secret patterns.
func New() *Redactor {
	r := &Redactor{
		exact:    make(map[string]struct{}),
		patterns: append([]*regexp.Regexp{}, defaultPatterns...),
	}
	return r
}

// RegisterSecret adds an exact secret string to be redacted.
func (r *Redactor) RegisterSecret(secret string) {
	if len(strings.TrimSpace(secret)) < 4 {
		return // avoid redacting short trivial strings
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exact[secret] = struct{}{}
}

// Redact replaces any known secret pattern or exact registered secret in input.
func (r *Redactor) Redact(text string) string {
	if text == "" {
		return ""
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// 1. Redact exact secrets
	res := text
	for s := range r.exact {
		if strings.Contains(res, s) {
			res = strings.ReplaceAll(res, s, RedactionMarker)
		}
	}

	// 2. Redact regex patterns
	for _, pat := range r.patterns {
		res = pat.ReplaceAllStringFunc(res, func(match string) string {
			// If it's a key=value match with capture groups, keep the key name
			if strings.Contains(match, "=") || strings.Contains(match, ":") {
				parts := strings.FieldsFunc(match, func(r rune) bool {
					return r == '=' || r == ':'
				})
				if len(parts) >= 2 {
					delim := "="
					if strings.Contains(match, ":") {
						delim = ": "
					}
					return fmt.Sprintf("%s%s%s", parts[0], delim, RedactionMarker)
				}
			}
			if strings.HasPrefix(strings.ToLower(match), "bearer ") {
				return "Bearer " + RedactionMarker
			}
			return RedactionMarker
		})
	}

	return res
}

// RedactBytes redacts a byte slice.
func (r *Redactor) RedactBytes(b []byte) []byte {
	return []byte(r.Redact(string(b)))
}

// RegisterSecret registers a secret with the default global redactor.
func RegisterSecret(s string) {
	global.RegisterSecret(s)
}

// Sanitize cleans text using the default global redactor.
func Sanitize(text string) string {
	return global.Redact(text)
}

// SanitizeBytes cleans byte slices using the default global redactor.
func SanitizeBytes(b []byte) []byte {
	return bytes.TrimSpace(global.RedactBytes(b))
}
