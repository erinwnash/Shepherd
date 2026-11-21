package migrate

import (
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/workorder"
)

const legacy = `# 18D -- Rule builder: due trigger

**Effort:** M | **Depends on:** 18A, 18B | **Review:** HIGH (touches the DB)

## Goal
Add a due-date trigger.

## Files
- CREATE ` + "`supabase/migrations/<next number>_due.sql`" + `
- CREATE ` + "`tests/unit/dueTrigger.test.ts`" + `
- EDIT ` + "`src/lib/events/admit.ts`" + ` (add ` + "`admitBatch`" + `)
- EDIT ` + "`.../projects/[projectId]/page.tsx`" + `
- EDIT ` + "`src/app/[id]/page.tsx`" + `
- EDIT ` + "`src/a.ts`" + ` and/or ` + "`src/b.ts`" + ` (only if needed)
- EDIT the personal menu that links to ` + "`me/security`" + ` (find with ` + "`grep -rn x src`" + `)
- EDIT ` + "`tests/unit/`" + ` file that tests the worker
- READ ` + "`src/lib/read/only.ts`" + `

## Verify
- ` + "`npx vitest run tests/unit/dueTrigger.test.ts`" + ` passes
- ` + "`npx vitest run tests/unit/other.test.ts`" + ` passes

## Done when
- It works.
`

func convert(t *testing.T, src string, done bool) (*workorder.WorkOrder, Result) {
	t.Helper()
	r, err := BoldLines("docs/parity/18_x/18D_rule-builder-due-trigger.md", []byte(src), Options{Done: done})
	if err != nil {
		t.Fatal(err)
	}
	w, err := workorder.Parse(r.Name, r.Out)
	if err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, r.Out)
	}
	return w, r
}

func TestBoldLinesConvertsMetadata(t *testing.T) {
	w, r := convert(t, legacy, false)
	if w.ID != "18D" || w.Title != "Rule builder: due trigger" || w.Sprint != "S18" {
		t.Errorf("id/title/sprint = %q %q %q", w.ID, w.Title, w.Sprint)
	}
	if strings.Join(w.DependsOn, ",") != "18A,18B" {
		t.Errorf("depends_on = %v", w.DependsOn)
	}
	if w.Review != workorder.ReviewHigh || w.Effort != workorder.EffortM || w.Status != workorder.StatusPending {
		t.Errorf("review/effort/status = %s %s %s", w.Review, w.Effort, w.Status)
	}
	if r.Name != "S18/18D_rule-builder-due-trigger.md" {
		t.Errorf("name = %s", r.Name)
	}
}

func TestBoldLinesFiles(t *testing.T) {
	w, r := convert(t, legacy, false)
	var got []string
	for _, f := range w.Files {
		p := f.Path
		if f.New {
			p = "new:" + p
		}
		got = append(got, p)
	}
	want := "new:supabase/migrations/<next_number>_due.sql,new:tests/unit/dueTrigger.test.ts," +
		"src/lib/events/admit.ts,src/app/[id]/page.tsx,src/a.ts,src/b.ts,tests/unit/other.test.ts"
	if strings.Join(got, ",") != want {
		t.Errorf("files =\n %s\nwant\n %s", strings.Join(got, ","), want)
	}
	if strings.Join(w.Tests, ",") != "tests/unit/dueTrigger.test.ts,tests/unit/other.test.ts" {
		t.Errorf("tests = %v", w.Tests)
	}
	// The vague EDIT lines cannot become paths; they must be reported, not dropped silently.
	if len(r.Notes) != 3 || !strings.Contains(r.Notes[0], ".../projects") ||
		!strings.Contains(r.Notes[1], "personal menu") || !strings.Contains(r.Notes[2], "tests/unit/") {
		t.Errorf("notes = %q", r.Notes)
	}
}

func TestBoldLinesBody(t *testing.T) {
	w, _ := convert(t, legacy, true)
	if w.Status != workorder.StatusDone {
		t.Errorf("status = %s", w.Status)
	}
	if !w.HasSection("Goal") || !w.HasSection("Acceptance") || w.HasSection("Done when") {
		t.Errorf("sections = %v", w.Sections)
	}
	if strings.Contains(w.Body, "**Effort:**") || strings.Contains(w.Body, "# 18D --") {
		t.Errorf("legacy header left in body:\n%s", w.Body)
	}
}

func TestBoldLinesNoDepsNoTests(t *testing.T) {
	src := strings.Replace(legacy, "18A, 18B | **Review:** HIGH (touches the DB)", "nothing | **Review:** low", 1)
	src = strings.ReplaceAll(src, "npx vitest run", "npm run")
	w, _ := convert(t, src, false)
	if len(w.DependsOn) != 0 || w.Review != workorder.ReviewLow {
		t.Errorf("deps/review = %v %s", w.DependsOn, w.Review)
	}
	if len(w.Tests) != 0 || w.NoTestsReason == "" {
		t.Errorf("tests/no_tests_reason = %v %q", w.Tests, w.NoTestsReason)
	}
}

func TestBoldLinesCRLF(t *testing.T) {
	a, _ := convert(t, legacy, false)
	b, _ := convert(t, strings.ReplaceAll(legacy, "\n", "\r\n"), false)
	if a.Hash != b.Hash {
		t.Error("CRLF source converts differently")
	}
}

// Abuse cases: the legacy text is untrusted input. It must not be able to smuggle extra
// frontmatter keys, protected or escaping paths, or a malformed id into the output.
func TestBoldLinesRejectsHostileInput(t *testing.T) {
	cases := map[string]struct{ name, src string }{
		"bad file name":    {"docs/x/notachunk.md", legacy},
		"no header":        {"docs/x/18D_x.md", strings.Replace(legacy, "**Effort:**", "Effort:", 1)},
		"id mismatch":      {"docs/x/18E_x.md", legacy},
		"bad effort":       {"docs/x/18D_x.md", strings.Replace(legacy, "**Effort:** M", "**Effort:** XL", 1)},
		"bad review":       {"docs/x/18D_x.md", strings.Replace(legacy, "**Review:** HIGH", "**Review:** urgent", 1)},
		"escaping path":    {"docs/x/18D_x.md", strings.Replace(legacy, "src/a.ts", "../../etc/passwd", 1)},
		"absolute path":    {"docs/x/18D_x.md", strings.Replace(legacy, "src/a.ts", "/etc/passwd", 1)},
		"no declared file": {"docs/x/18D_x.md", legacy[:strings.Index(legacy, "## Files")]},
	}
	for name, c := range cases {
		if _, err := BoldLines(c.name, []byte(c.src), Options{}); err == nil {
			t.Errorf("%s: converted without error", name)
		}
	}
}

func TestBoldLinesTitleCannotInjectYAML(t *testing.T) {
	src := strings.Replace(legacy, "Rule builder: due trigger", "x\nstatus: done\n---\nid: evil", 1)
	// A newline in the title line ends the title at the first line, so the rest is body text.
	w, _ := convert(t, src, false)
	if w.Status != workorder.StatusPending || w.ID != "18D" || w.Title != "x" {
		t.Errorf("injected: id=%q title=%q status=%s", w.ID, w.Title, w.Status)
	}
	src = strings.Replace(legacy, "Rule builder: due trigger", `a" ]: {status: done} #`, 1)
	w, _ = convert(t, src, false)
	if w.Status != workorder.StatusPending || w.Title != `a" ]: {status: done} #` {
		t.Errorf("injected: title=%q status=%s", w.Title, w.Status)
	}
}

func TestBoldLinesResolvesAbbreviatedPaths(t *testing.T) {
	repo := map[string][]string{
		"projects/[projectId]/page.tsx": {"src/app/w/[workspaceId]/projects/[projectId]/page.tsx"},
		"x/y.ts":                        {"a/x/y.ts", "b/x/y.ts"},
	}
	src := strings.Replace(legacy, "- EDIT `src/a.ts`", "- EDIT `.../x/y.ts`\n- EDIT `src/a.ts`", 1)
	r, err := BoldLines("docs/x/18D_x.md", []byte(src), Options{Resolve: func(suffix string) []string { return repo[suffix] }})
	if err != nil {
		t.Fatal(err)
	}
	w, err := workorder.Parse(r.Name, r.Out)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range w.Files {
		got = append(got, f.Path)
	}
	if !contains(got, "src/app/w/[workspaceId]/projects/[projectId]/page.tsx") {
		t.Errorf("unique suffix not resolved: %v", got)
	}
	// Two candidates: ambiguous, so it stays a note and is not guessed.
	if contains(got, "a/x/y.ts") || contains(got, "b/x/y.ts") || !strings.Contains(strings.Join(r.Notes, "|"), ".../x/y.ts") {
		t.Errorf("ambiguous suffix guessed: files=%v notes=%v", got, r.Notes)
	}
}

// Some work orders widen their scope in prose ("in scope in addition to the Files list").
// Dropping that would make the scope gate reject work the order asked for.
func TestBoldLinesExtraScopeSection(t *testing.T) {
	extra := "\n## Agent tools\n" +
		"Contract: `docs/agents/contract.md`.\n" +
		"- Add or extend a tool in `src/tools/publish.ts` (create it if missing) and add it to `src/tools/index.ts`.\n" +
		"- Add test cases to `tests/unit/tools.publish.test.ts`.\n" +
		"- These files are in scope in addition to the Files list. Verify also with `npx vitest run tests/unit/tools.publish.test.ts`.\n" +
		"- If `src/tools/registry.ts` does not exist, STOP.\n"
	existing := map[string]bool{"src/tools/index.ts": true}
	r, err := BoldLines("docs/x/18D_x.md", []byte(legacy+extra), Options{Exists: func(p string) bool { return existing[p] }})
	if err != nil {
		t.Fatal(err)
	}
	w, err := workorder.Parse(r.Name, r.Out)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range w.Files {
		p := f.Path
		if f.New {
			p = "new:" + p
		}
		got[p] = true
	}
	for _, want := range []string{"new:src/tools/publish.ts", "src/tools/index.ts", "new:tests/unit/tools.publish.test.ts"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	// Referenced but not to be changed: a contract document and a precondition check.
	for _, not := range []string{"docs/agents/contract.md", "src/tools/registry.ts", "new:docs/agents/contract.md", "new:src/tools/registry.ts"} {
		if got[not] {
			t.Errorf("%s must not be in scope", not)
		}
	}
}
