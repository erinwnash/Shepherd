package plan

import (
	"fmt"
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/workorder"
)

// fakeRepo answers Exists from a fixed set and knows ADR-0001..0009.
type fakeRepo map[string]bool

func (r fakeRepo) Exists(p string) bool { return r[p] }
func (r fakeRepo) ADRExists(id string) bool {
	var n int
	_, err := fmt.Sscanf(id, "ADR-%04d", &n)
	return err == nil && n >= 1 && n <= 9
}

var repo = fakeRepo{"src/a.go": true, "src/b.go": true, "src/c.go": true, "a_test.go": true}

// chunk builds a parsed work order; fields are given as frontmatter lines.
func chunk(t *testing.T, id, sprint string, extra ...string) *workorder.WorkOrder {
	t.Helper()
	fm := []string{"id: " + id, "title: t " + id, "sprint: " + sprint}
	hasFiles, hasTests := false, false
	for _, e := range extra {
		hasFiles = hasFiles || strings.HasPrefix(e, "files:")
		hasTests = hasTests || strings.HasPrefix(e, "tests:") || strings.HasPrefix(e, "no_tests_reason:")
	}
	if !hasFiles {
		fm = append(fm, "files: [src/"+strings.ToLower(id)+".go]")
	}
	if !hasTests {
		fm = append(fm, "tests: [a_test.go]")
	}
	fm = append(fm, extra...)
	src := "---\n" + strings.Join(fm, "\n") + "\n---\n## Goal\ng\n## Out of scope\n- x\n## Acceptance\n- y\n"
	w, err := workorder.Parse(id+".md", []byte(src))
	if err != nil {
		t.Fatalf("chunk %s: %v", id, err)
	}
	return w
}

func codes(fs []Finding, sev Severity) string {
	var out []string
	for _, f := range fs {
		if f.Severity == sev {
			out = append(out, f.Chunk+":"+f.Code)
		}
	}
	return strings.Join(out, " ")
}

func lint(t *testing.T, ws ...*workorder.WorkOrder) []Finding {
	t.Helper()
	r2 := fakeRepo{}
	for k, v := range repo {
		r2[k] = v
	}
	for _, w := range ws {
		for _, f := range w.Files {
			if !f.New && !strings.Contains(f.Path, "<") {
				r2[f.Path] = true
			}
		}
	}
	return Lint(ws, Options{MaxFiles: 5, SprintOrder: []string{"S1", "S2"}}, r2)
}

func TestCleanPlan(t *testing.T) {
	fs := lint(t,
		chunk(t, "A", "S1"),
		chunk(t, "B", "S1", "depends_on: [A]"),
		chunk(t, "C", "S2", "depends_on: [B]", "adr: [ADR-0003]"),
	)
	if len(fs) != 0 {
		t.Errorf("clean plan produced findings: %+v", fs)
	}
}

func TestDependencyCycle(t *testing.T) {
	fs := lint(t,
		chunk(t, "A", "S1", "depends_on: [C]"),
		chunk(t, "B", "S1", "depends_on: [A]"),
		chunk(t, "C", "S1", "depends_on: [B]"),
		chunk(t, "D", "S1"),
	)
	got := codes(fs, Error)
	if strings.Count(got, ":cycle") != 1 {
		t.Fatalf("want exactly one cycle finding, got %q", got)
	}
	for _, f := range fs {
		if f.Code == "cycle" && !(strings.Contains(f.Message, "A") && strings.Contains(f.Message, "B") && strings.Contains(f.Message, "C")) {
			t.Errorf("cycle message should name the members: %q", f.Message)
		}
	}
}

func TestDanglingAndDuplicate(t *testing.T) {
	fs := lint(t,
		chunk(t, "A", "S1", "depends_on: [ZZ]"),
		chunk(t, "B", "S1"),
		chunk(t, "B", "S1"),
	)
	got := codes(fs, Error)
	if !strings.Contains(got, "A:dangling-dep") || !strings.Contains(got, "B:duplicate-id") {
		t.Errorf("got %q", got)
	}
}

func TestOversizeChunk(t *testing.T) {
	fs := lint(t, chunk(t, "A", "S1", "files: [src/a.go, src/b.go, src/c.go, new:src/d.go, new:src/e.go, new:src/f.go]"))
	if !strings.Contains(codes(fs, Error), "A:oversize") {
		t.Errorf("got %+v", fs)
	}
}

// Planning-quality rules judge work still to do; a finished chunk cannot be split or rewritten.
// Safety rules (protected paths) still apply to it.
func TestDoneChunkSkipsPlanningRules(t *testing.T) {
	w := chunk(t, "A", "S1", "status: done", "tests: []",
		"files: [src/a.go, src/b.go, src/c.go, src/d.go, src/e.go, src/f.go, .github/workflows/ci.yml]")
	w.Sections = map[string]string{}
	got := codes(lint(t, w), Error) + " " + codes(lint(t, w), Warning)
	if strings.Contains(got, "oversize") || strings.Contains(got, "no-tests") || strings.Contains(got, "missing-section") {
		t.Errorf("planning rule fired on a done chunk: %s", got)
	}
	if !strings.Contains(got, "A:protected-path") {
		t.Errorf("protected-path must still fire on a done chunk: %s", got)
	}
}

func TestNoTestsNoReason(t *testing.T) {
	fs := lint(t, chunk(t, "A", "S1", "tests: []"))
	if !strings.Contains(codes(fs, Error), "A:no-tests") {
		t.Errorf("got %+v", fs)
	}
	fs = lint(t, chunk(t, "B", "S1", "no_tests_reason: docs only"))
	if strings.Contains(codes(fs, Error), "no-tests") {
		t.Errorf("reason given but still flagged: %+v", fs)
	}
}

func TestOverlapWithoutOrdering(t *testing.T) {
	// A and B both touch src/a.go and neither depends on the other: their result depends on
	// run order, so the plan is rejected.
	fs := lint(t,
		chunk(t, "A", "S1", "files: [src/a.go]"),
		chunk(t, "B", "S1", "files: [src/a.go, src/b.go]"),
	)
	if !strings.Contains(codes(fs, Error), "unordered-overlap") {
		t.Errorf("got %+v", fs)
	}
	// ordered through a transitive dependency: fine
	fs = lint(t,
		chunk(t, "A", "S1", "files: [src/a.go]"),
		chunk(t, "M", "S1", "depends_on: [A]"),
		chunk(t, "B", "S1", "files: [src/a.go]", "depends_on: [M]"),
	)
	if strings.Contains(codes(fs, Error), "unordered-overlap") {
		t.Errorf("transitively ordered chunks flagged: %+v", fs)
	}
}

func TestOverlapIgnoresDoneChunks(t *testing.T) {
	fs := lint(t,
		chunk(t, "A", "S1", "files: [src/a.go]", "status: done"),
		chunk(t, "B", "S1", "files: [src/a.go]"),
	)
	if strings.Contains(codes(fs, Error), "unordered-overlap") {
		t.Errorf("done chunk counted for overlap: %+v", fs)
	}
}

func TestFilesMustExistUnlessNew(t *testing.T) {
	ws := []*workorder.WorkOrder{chunk(t, "A", "S1", "files: [src/missing.go, new:src/a.go]", "tests: [a_test.go]")}
	fs := Lint(ws, Options{MaxFiles: 5}, repo)
	got := codes(fs, Error)
	if !strings.Contains(got, "A:file-missing") {
		t.Errorf("errors %q", got)
	}
	if !strings.Contains(codes(fs, Warning), "A:new-file-exists") {
		t.Errorf("warnings %+v", fs)
	}
}

func TestTestFileMustExistOrBeNew(t *testing.T) {
	ws := []*workorder.WorkOrder{chunk(t, "A", "S1", "files: [src/a.go]", "tests: [t/new_test.go]")}
	if !strings.Contains(codes(Lint(ws, Options{MaxFiles: 5}, repo), Error), "A:test-missing") {
		t.Error("missing, undeclared test file not flagged")
	}
	ws = []*workorder.WorkOrder{chunk(t, "B", "S1", "files: [src/a.go, new:t/new_test.go]", "tests: [t/new_test.go]")}
	if strings.Contains(codes(Lint(ws, Options{MaxFiles: 5}, repo), Error), "test-missing") {
		t.Error("test declared as new still flagged")
	}
}

func TestProtectedPathDeclared(t *testing.T) {
	fs := lint(t, chunk(t, "A", "S1", "files: [shepherd.toml, src/a.go]"))
	if !strings.Contains(codes(fs, Error), "A:protected-path") {
		t.Errorf("got %+v", fs)
	}
}

func TestADRMustExist(t *testing.T) {
	fs := lint(t, chunk(t, "A", "S1", "adr: [ADR-0042]"))
	if !strings.Contains(codes(fs, Error), "A:adr-missing") {
		t.Errorf("got %+v", fs)
	}
}

func TestDependsOnLaterSprint(t *testing.T) {
	fs := lint(t,
		chunk(t, "A", "S1", "depends_on: [B]"),
		chunk(t, "B", "S2"),
	)
	if !strings.Contains(codes(fs, Error), "A:later-sprint-dep") {
		t.Errorf("got %+v", fs)
	}
}

func TestMissingSectionsWarn(t *testing.T) {
	w, err := workorder.Parse("x.md", []byte("---\nid: A\ntitle: t\nfiles: [src/a.go]\ntests: [a_test.go]\n---\n## Goal\ng\n"))
	if err != nil {
		t.Fatal(err)
	}
	fs := Lint([]*workorder.WorkOrder{w}, Options{MaxFiles: 5}, repo)
	got := codes(fs, Warning)
	if !strings.Contains(got, "A:missing-section") || len(codes(fs, Error)) != 0 {
		t.Errorf("got %+v", fs)
	}
}

func TestFindingsAreSortedAndStable(t *testing.T) {
	ws := []*workorder.WorkOrder{
		chunk(t, "B", "S1", "depends_on: [Q]"),
		chunk(t, "A", "S1", "depends_on: [Z]"),
	}
	a := lint(t, ws...)
	b := lint(t, ws[1], ws[0])
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("order depends on input order:\n%v\n%v", a, b)
	}
	if a[0].Chunk != "A" {
		t.Errorf("not sorted by chunk: %v", a)
	}
}
