// Package scope holds the path rules shared by plan lint and the scope gate: what a declared
// path may look like, how placeholders match, and which paths no chunk may ever touch.
//
// Built from scratch: this is core security logic (brief section 7), small and specific.
package scope

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// ValidatePattern checks one declared path from a work order. Paths are repo-relative, use
// forward slashes, and are canonical (no ".", "..", empty segments or trailing slash). A
// segment may contain <name> placeholders, each matching one or more characters within that
// segment. Anything that could act as a command-line option (a segment starting with "-") or
// escape the worktree is rejected.
func ValidatePattern(p string) error {
	switch {
	case p == "":
		return errors.New("empty path")
	case strings.TrimSpace(p) != p:
		return fmt.Errorf("%q: leading or trailing whitespace", p)
	case strings.ContainsRune(p, '\\'):
		return fmt.Errorf("%q: use forward slashes", p)
	case strings.HasPrefix(p, "/"), strings.HasPrefix(p, "~"), len(p) >= 2 && p[1] == ':':
		return fmt.Errorf("%q: must be relative to the repo root", p)
	case strings.ContainsAny(p, "*?"):
		// "[" and "]" stay legal: paths are never globbed, and frameworks name route folders "[id]".
		return fmt.Errorf("%q: '*' and '?' are not allowed; use a <name> placeholder", p)
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return fmt.Errorf("%q: control character", p)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "":
			return fmt.Errorf("%q: empty path segment", p)
		case seg == "." || seg == "..":
			return fmt.Errorf("%q: %q segments are not allowed", p, seg)
		case strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " "):
			// Windows drops trailing dots and spaces, so "x." would alias "x" past the scope gate.
			return fmt.Errorf("%q: a segment may not end with '.' or a space", p)
		case strings.HasPrefix(seg, "-"):
			return fmt.Errorf("%q: a segment may not start with '-'", p)
		}
		if err := checkPlaceholders(seg); err != nil {
			return fmt.Errorf("%q: %w", p, err)
		}
	}
	return nil
}

func checkPlaceholders(seg string) error {
	depth := 0
	for _, r := range seg {
		switch r {
		case '<':
			if depth > 0 {
				return errors.New("nested '<'")
			}
			depth++
		case '>':
			if depth == 0 {
				return errors.New("'>' without '<'")
			}
			depth--
		case '/':
			return errors.New("placeholder spans segments")
		}
	}
	if depth != 0 {
		return errors.New("unclosed '<'")
	}
	return nil
}

var placeholder = regexp.MustCompile(`<[^<>/]*>`)

// HasPlaceholder reports whether a declared path contains a <name> placeholder.
func HasPlaceholder(p string) bool { return placeholder.MatchString(p) }

func compile(pat string, foldCase bool) *regexp.Regexp {
	parts := placeholder.Split(pat, -1)
	for i, s := range parts {
		parts[i] = regexp.QuoteMeta(s)
	}
	expr := "^" + strings.Join(parts, "[^/]+") + "$"
	if foldCase {
		expr = "(?i)" + expr
	}
	return regexp.MustCompile(expr)
}

// Match reports whether a changed path is covered by a declared path (case-sensitive).
func Match(pat, path string) bool {
	if !HasPlaceholder(pat) {
		return pat == path
	}
	return compile(pat, false).MatchString(path)
}

// Overlap reports whether two declared paths could name the same file. It is conservative:
// case is ignored (macOS and Windows file systems are case-insensitive by default), and two
// placeholder patterns overlap when they are equal with their placeholders blanked.
func Overlap(a, b string) bool {
	ha, hb := HasPlaceholder(a), HasPlaceholder(b)
	switch {
	case !ha && !hb:
		return strings.EqualFold(a, b)
	case ha && !hb:
		return compile(a, true).MatchString(b)
	case !ha && hb:
		return compile(b, true).MatchString(a)
	default:
		blank := func(s string) string { return strings.ToLower(placeholder.ReplaceAllString(s, "<>")) }
		return blank(a) == blank(b)
	}
}

// DefaultProtected are paths no chunk may modify, whatever its work order says: Shepherd's own
// config and prompts, git internals and hook managers, and CI definitions. Entries ending in
// "/" are directory prefixes.
var DefaultProtected = []string{
	"shepherd.toml",
	".shepherd/",
	".git/",
	".git",
	".gitmodules",
	".githooks/",
	".husky/",
	".github/",
	".gitlab-ci.yml",
	".circleci/",
	".buildkite/",
	"azure-pipelines.yml",
	"Jenkinsfile",
}

// Protected matches paths against the protected list, ignoring case.
type Protected struct{ entries []string }

// NewProtected returns the default protected list plus project-specific extras.
func NewProtected(extra []string) Protected {
	all := make([]string, 0, len(DefaultProtected)+len(extra))
	for _, e := range append(append([]string{}, DefaultProtected...), extra...) {
		all = append(all, strings.ToLower(e))
	}
	return Protected{entries: all}
}

// Is reports whether a repo-relative path is protected.
func (p Protected) Is(path string) bool {
	l := strings.ToLower(path)
	for _, e := range p.entries {
		if strings.HasSuffix(e, "/") {
			if strings.HasPrefix(l, e) || l == strings.TrimSuffix(e, "/") {
				return true
			}
		} else if l == e {
			return true
		}
	}
	return false
}

// Covers reports whether a declared path (possibly with placeholders) could name a protected
// file.
func (p Protected) Covers(pat string) bool {
	if p.Is(pat) {
		return true
	}
	if !HasPlaceholder(pat) {
		return false
	}
	segs := strings.Split(pat, "/")
	for _, e := range p.entries {
		dir := strings.HasSuffix(e, "/")
		eSegs := strings.Split(strings.TrimSuffix(e, "/"), "/")
		n := len(eSegs)
		if len(segs) < n || (!dir && len(segs) != n) {
			continue
		}
		// a directory entry also protects the directory path itself, so compare prefixes
		if compile(strings.Join(segs[:n], "/"), true).MatchString(strings.Join(eSegs, "/")) {
			return true
		}
	}
	return false
}
