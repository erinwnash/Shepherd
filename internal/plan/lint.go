// Package plan validates a set of work orders before anything is queued (brief section 9.5,
// idea #1). Deterministic, no model calls.
//
// Built from scratch: plan lint is core Shepherd logic.
package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/erinwnash/Shepherd/internal/scope"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

type Severity int

const (
	Error Severity = iota
	Warning
)

func (s Severity) String() string {
	if s == Error {
		return "error"
	}
	return "warning"
}

// Finding is one lint result. Code is a stable kebab-case identifier for tests and filtering.
type Finding struct {
	Severity Severity
	Chunk    string
	Code     string
	Message  string
}

func (f Finding) String() string {
	return fmt.Sprintf("%-7s %-8s %-18s %s", f.Severity, f.Chunk, f.Code, f.Message)
}

// Repo answers questions about the project on disk.
type Repo interface {
	Exists(relPath string) bool
	ADRExists(id string) bool
}

type Options struct {
	MaxFiles       int      // declared files per chunk; 0 means the default (5)
	SprintOrder    []string // sprint ids in execution order; unknown sprints are not compared
	ProtectedExtra []string // project-specific protected paths
}

// RequiredSections must appear in every work order body; missing ones are warnings.
var RequiredSections = []string{"Goal", "Out of scope", "Acceptance"}

// Lint checks the whole plan and returns findings sorted by chunk, severity and code.
func Lint(orders []*workorder.WorkOrder, opt Options, repo Repo) []Finding {
	if opt.MaxFiles <= 0 {
		opt.MaxFiles = 5
	}
	var out []Finding
	add := func(sev Severity, chunk, code, f string, a ...any) {
		out = append(out, Finding{sev, chunk, code, fmt.Sprintf(f, a...)})
	}
	protected := scope.NewProtected(opt.ProtectedExtra)
	sprintIdx := map[string]int{}
	for i, s := range opt.SprintOrder {
		sprintIdx[s] = i
	}

	byID := map[string]*workorder.WorkOrder{}
	for _, w := range orders {
		if prev, dup := byID[w.ID]; dup {
			add(Error, w.ID, "duplicate-id", "id also used by %s", prev.Path)
			continue
		}
		byID[w.ID] = w
	}

	for _, w := range orders {
		for _, d := range w.DependsOn {
			dep, ok := byID[d]
			if !ok {
				add(Error, w.ID, "dangling-dep", "depends on %s, which does not exist", d)
				continue
			}
			wi, wok := sprintIdx[w.Sprint]
			di, dok := sprintIdx[dep.Sprint]
			if wok && dok && di > wi && dep.Status != workorder.StatusDone {
				add(Error, w.ID, "later-sprint-dep", "depends on %s in later sprint %s", d, dep.Sprint)
			}
		}
		// Planning-quality rules judge work still to do; history cannot be split or rewritten.
		todo := w.Status != workorder.StatusDone
		if todo && len(w.Files) > opt.MaxFiles {
			add(Error, w.ID, "oversize", "declares %d files (max %d); split the chunk", len(w.Files), opt.MaxFiles)
		}
		if todo && len(w.Tests) == 0 && w.NoTestsReason == "" {
			add(Error, w.ID, "no-tests", "no tests listed and no no_tests_reason given")
		}
		newFiles := map[string]bool{}
		for _, f := range w.Files {
			if protected.Covers(f.Path) {
				add(Error, w.ID, "protected-path", "%s is protected; no chunk may change it", f.Path)
				continue
			}
			if scope.HasPlaceholder(f.Path) {
				continue
			}
			exists := repo.Exists(f.Path)
			switch {
			case f.New:
				newFiles[f.Path] = true
				if exists && w.Status != workorder.StatusDone {
					add(Warning, w.ID, "new-file-exists", "%s is marked new: but already exists", f.Path)
				}
			case !exists && w.Status != workorder.StatusDone:
				add(Error, w.ID, "file-missing", "%s does not exist; mark it new: if the chunk creates it", f.Path)
			}
		}
		for _, t := range w.Tests {
			if protected.Is(t) {
				add(Error, w.ID, "protected-path", "test %s is a protected path", t)
			} else if !repo.Exists(t) && !newFiles[t] && w.Status != workorder.StatusDone {
				add(Error, w.ID, "test-missing", "test %s does not exist and is not declared as new: under files", t)
			}
		}
		for _, a := range w.ADR {
			if !repo.ADRExists(a) {
				add(Error, w.ID, "adr-missing", "references %s, which does not exist", a)
			}
		}
		var missing []string
		for _, s := range RequiredSections {
			if !w.HasSection(s) {
				missing = append(missing, s)
			}
		}
		if todo && len(missing) > 0 {
			add(Warning, w.ID, "missing-section", "body has no %s section", strings.Join(missing, ", "))
		}
	}

	for _, c := range cycles(orders, byID) {
		add(Error, c[0], "cycle", "dependency cycle: %s", strings.Join(append(c, c[0]), " -> "))
	}
	out = append(out, overlaps(orders, byID)...)

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Chunk != b.Chunk {
			return a.Chunk < b.Chunk
		}
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
	return out
}

// cycles returns each dependency cycle once, rotated to start at its smallest id.
func cycles(orders []*workorder.WorkOrder, byID map[string]*workorder.WorkOrder) [][]string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	seen := map[string]bool{}
	var found [][]string
	var visit func(id string)
	visit = func(id string) {
		color[id] = grey
		stack = append(stack, id)
		deps := append([]string{}, byID[id].DependsOn...)
		sort.Strings(deps)
		for _, d := range deps {
			if _, ok := byID[d]; !ok {
				continue
			}
			switch color[d] {
			case white:
				visit(d)
			case grey:
				i := len(stack) - 1
				for stack[i] != d {
					i--
				}
				c := rotate(append([]string{}, stack[i:]...))
				if key := strings.Join(c, ">"); !seen[key] {
					seen[key] = true
					found = append(found, c)
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if color[id] == white {
			visit(id)
		}
	}
	return found
}

func rotate(c []string) []string {
	min := 0
	for i := range c {
		if c[i] < c[min] {
			min = i
		}
	}
	return append(c[min:], c[:min]...)
}

// overlaps flags pairs of unfinished chunks that declare the same file while neither depends
// (transitively) on the other: the outcome would depend on run order, and the scheduler could
// not tell which one to serialize first.
func overlaps(orders []*workorder.WorkOrder, byID map[string]*workorder.WorkOrder) []Finding {
	reach := map[string]map[string]bool{}
	var deps func(id string, seen map[string]bool)
	deps = func(id string, seen map[string]bool) {
		for _, d := range byID[id].DependsOn {
			if _, ok := byID[d]; ok && !seen[d] {
				seen[d] = true
				deps(d, seen)
			}
		}
	}
	var live []*workorder.WorkOrder
	for id, w := range byID {
		if w.Status == workorder.StatusDone || w.Status == workorder.StatusSkipped {
			continue
		}
		reach[id] = map[string]bool{}
		deps(id, reach[id])
		live = append(live, w)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })

	var out []Finding
	for i, a := range live {
		for _, b := range live[i+1:] {
			if reach[a.ID][b.ID] || reach[b.ID][a.ID] {
				continue
			}
			var shared []string
			for _, fa := range a.Files {
				for _, fb := range b.Files {
					if scope.Overlap(fa.Path, fb.Path) {
						shared = append(shared, fa.Path)
					}
				}
			}
			if len(shared) > 0 {
				out = append(out, Finding{Error, a.ID, "unordered-overlap",
					fmt.Sprintf("shares %s with %s but neither depends on the other; add a depends_on", strings.Join(shared, ", "), b.ID)})
			}
		}
	}
	return out
}
