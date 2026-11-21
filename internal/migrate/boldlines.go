// Package migrate converts legacy work orders to the frontmatter schema (ADR-0005). It is a
// one-shot tool: Shepherd's queue reads only frontmatter work orders.
//
// Built from scratch: the input is a project-specific legacy format; there is nothing to source.
package migrate

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/erinwnash/Shepherd/internal/scope"
	"github.com/erinwnash/Shepherd/internal/workorder"
	"go.yaml.in/yaml/v3"
)

// Result is one converted work order.
type Result struct {
	Name  string   // output path relative to the output root, e.g. "S18/18D_x.md"
	Out   []byte   // the converted file
	Notes []string // things a human must check: lines that could not become declared paths
}

var (
	fileName   = regexp.MustCompile(`^(\d\d)([A-Z])_[^/]*\.md$`)
	titleLine  = regexp.MustCompile(`^# (\d\d[A-Z]) -- (.+)$`)
	headerLine = regexp.MustCompile(`^\*\*Effort:\*\* *(\S+) *\| *\*\*Depends on:\*\* *(.*?) *\| *\*\*Review:\*\* *([A-Za-z]+)`)
	legacyID   = regexp.MustCompile(`\b\d\d[A-Z]\b`)
	backticked = regexp.MustCompile("`([^`]*)`")
	fileLine   = regexp.MustCompile("^- (CREATE|EDIT) `")
	runTest    = regexp.MustCompile(`npx vitest run (tests/[A-Za-z0-9_./-]+\.(?:test|spec)\.tsx?)`)
	namedTest  = regexp.MustCompile(`tests/[A-Za-z0-9_./-]+\.(?:test|spec)\.tsx?`)
	spaceInTag = regexp.MustCompile(`<[^<>/]*>`)
	extraScope = regexp.MustCompile(`(?i)in scope in addition to the files list`)
	addVerb    = regexp.MustCompile(`(?i)\badd\b`)
)

// out mirrors workorder's frontmatter keys, in the order they are written.
type out struct {
	ID            string   `yaml:"id"`
	Title         string   `yaml:"title"`
	Sprint        string   `yaml:"sprint"`
	DependsOn     []string `yaml:"depends_on,omitempty,flow"`
	Review        string   `yaml:"review"`
	Effort        string   `yaml:"est_effort"`
	Status        string   `yaml:"status"`
	Files         []string `yaml:"files"`
	Tests         []string `yaml:"tests,omitempty"`
	NoTestsReason string   `yaml:"no_tests_reason,omitempty"`
}

// Options control one conversion.
type Options struct {
	Done bool // the chunk is finished
	// Resolve lists project files whose path ends with "/"+suffix. It expands abbreviated
	// ".../suffix" paths when exactly one file matches. Nil leaves them as notes.
	Resolve func(suffix string) []string
	// Exists reports whether a project file exists. It decides new: for files a work order
	// adds to its scope in prose. Nil treats them as existing.
	Exists func(rel string) bool
}

// BoldLines converts one "bold-lines" work order: a "# NNX -- title" heading, a
// "**Effort:** S | **Depends on:** ... | **Review:** low" line, backticked CREATE/EDIT paths
// under "## Files", and tests named by "npx vitest run tests/...".
func BoldLines(srcPath string, src []byte, opt Options) (Result, error) {
	base := path.Base(strings.ReplaceAll(srcPath, "\\", "/"))
	m := fileName.FindStringSubmatch(base)
	if m == nil {
		return Result{}, fmt.Errorf("%s: not a legacy chunk file name (NNX_name.md)", srcPath)
	}
	id := m[1] + m[2]
	text := strings.ReplaceAll(strings.TrimPrefix(string(src), string(rune(0xFEFF))), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	var problems []string
	o := out{ID: id, Sprint: "S" + m[1], Status: string(workorder.StatusPending)}
	if opt.Done {
		o.Status = string(workorder.StatusDone)
	}

	hdr := -1
	for i, l := range lines {
		if t := titleLine.FindStringSubmatch(l); t != nil && o.Title == "" {
			if t[1] != id {
				problems = append(problems, fmt.Sprintf("heading id %s does not match file name id %s", t[1], id))
			}
			o.Title = strings.TrimSpace(t[2])
		}
		if h := headerLine.FindStringSubmatch(l); h != nil {
			hdr = i
			o.Effort = strings.ToUpper(h[1])
			if o.Effort != "S" && o.Effort != "M" && o.Effort != "L" {
				problems = append(problems, fmt.Sprintf("effort %q is not S, M or L", h[1]))
			}
			if !strings.EqualFold(h[2], "nothing") {
				o.DependsOn = legacyID.FindAllString(h[2], -1)
			}
			o.Review = strings.ToUpper(h[3])
			if o.Review != "LOW" && o.Review != "MEDIUM" && o.Review != "HIGH" {
				problems = append(problems, fmt.Sprintf("review %q is not low, medium or high", h[3]))
			}
			break
		}
	}
	if o.Title == "" {
		problems = append(problems, "no '# NNX -- title' heading")
	}
	if hdr < 0 {
		return Result{}, fmt.Errorf("%s: no '**Effort:** | **Depends on:** | **Review:**' line", srcPath)
	}
	body := strings.TrimLeft(strings.Join(lines[hdr+1:], "\n"), "\n")

	var notes []string
	seen := map[string]bool{}
	add := func(p string, isNew bool) {
		key := strings.ToLower(p)
		if seen[key] {
			return
		}
		seen[key] = true
		if err := scope.ValidatePattern(p); err != nil {
			problems = append(problems, err.Error())
			return
		}
		if isNew {
			p = "new:" + p
		}
		o.Files = append(o.Files, p)
	}
	for _, l := range sectionLines(body, "Files") {
		verb := ""
		if fm := fileLine.FindStringSubmatch(l); fm != nil {
			verb = fm[1]
		} else if strings.HasPrefix(l, "- CREATE") || strings.HasPrefix(l, "- EDIT") {
			notes = append(notes, "no path on: "+l)
			continue
		} else {
			continue // READ lines and prose are context, not scope
		}
		// Paths come before the first parenthesis; backticks after it are commentary.
		head := l
		if i := strings.Index(outsideTicks(l), "("); i >= 0 {
			head = l[:i]
		}
		n, unresolved := 0, false
		for _, t := range backticked.FindAllStringSubmatch(head, -1) {
			p := spaceInTag.ReplaceAllStringFunc(t[1], func(s string) string { return strings.ReplaceAll(s, " ", "_") })
			if strings.HasSuffix(p, "/") || strings.ContainsAny(p, " \t") || !strings.Contains(p, "/") && !strings.Contains(p, ".") {
				continue // a directory, a command or a symbol name
			}
			if suffix, ok := strings.CutPrefix(p, ".../"); ok {
				// An abbreviated path: expand it only when the project makes it unambiguous.
				var m []string
				if opt.Resolve != nil {
					m = opt.Resolve(suffix)
				}
				if len(m) != 1 {
					unresolved = true
					continue
				}
				p = m[0]
			}
			add(p, verb == "CREATE")
			n++
		}
		if n == 0 || unresolved {
			notes = append(notes, "no path on: "+l)
		}
	}
	// A section that says its files are "in scope in addition to the Files list" widens the
	// scope: take the paths on its lines that ask to add something (not references or checks).
	for _, sec := range sectionsMatching(body, extraScope) {
		for _, l := range sec {
			if !addVerb.MatchString(l) {
				continue
			}
			for _, t := range backticked.FindAllStringSubmatch(l, -1) {
				p := t[1]
				if !strings.Contains(p, "/") || strings.HasSuffix(p, "/") || strings.ContainsAny(p, " \t") || strings.HasPrefix(p, ".../") {
					continue
				}
				add(p, opt.Exists != nil && !opt.Exists(p))
			}
		}
	}
	for _, t := range runTest.FindAllStringSubmatch(body, -1) {
		if !contains(o.Tests, t[1]) {
			o.Tests = append(o.Tests, t[1])
		}
	}
	// The legacy scope gate also allowed any test file the work order names.
	for _, t := range namedTest.FindAllString(body, -1) {
		add(t, false)
	}
	if len(o.Tests) == 0 {
		o.NoTestsReason = "migrated: the legacy work order names no test run"
	}
	if len(o.Files) == 0 {
		problems = append(problems, "no declared files")
	}
	if len(problems) > 0 {
		return Result{}, &workorder.ParseError{Path: srcPath, Problems: problems}
	}

	fm, err := yaml.Marshal(&o)
	if err != nil {
		return Result{}, err
	}
	body = renameHeading(body, "Done when", "Acceptance")
	res := Result{
		Name:  o.Sprint + "/" + base,
		Out:   []byte("---\n" + string(fm) + "---\n" + body),
		Notes: notes,
	}
	// The output must be a valid work order; a converter that writes files the queue then
	// rejects is worse than one that stops here.
	if _, err := workorder.Parse(srcPath, res.Out); err != nil {
		return Result{}, err
	}
	return res, nil
}

// sectionLines returns the lines of the "## name" section.
func sectionLines(body, name string) []string {
	var out []string
	on := false
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "## ") {
			on = strings.EqualFold(strings.TrimSpace(l[3:]), name)
			continue
		}
		if on {
			out = append(out, l)
		}
	}
	return out
}

// sectionsMatching returns the lines of every "## " section whose text matches re.
func sectionsMatching(body string, re *regexp.Regexp) [][]string {
	var out [][]string
	var cur []string
	flush := func() {
		if re.MatchString(strings.Join(cur, "\n")) {
			out = append(out, cur)
		}
		cur = nil
	}
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "## ") {
			flush()
			continue
		}
		cur = append(cur, l)
	}
	flush()
	return out
}

// outsideTicks blanks backticked spans so a "(" inside one is not taken as commentary.
func outsideTicks(l string) string {
	b := []byte(l)
	in := false
	for i, c := range b {
		if c == '`' {
			in = !in
		} else if in {
			b[i] = ' '
		}
	}
	return string(b)
}

func renameHeading(body, from, to string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "## ") && strings.EqualFold(strings.TrimSpace(l[3:]), from) {
			lines[i] = "## " + to
		}
	}
	return strings.Join(lines, "\n")
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// IsLegacyName reports whether a file name looks like a legacy chunk work order.
func IsLegacyName(name string) bool { return fileName.MatchString(name) }
