package workorder

import (
	"strings"
	"testing"
)

const good = `---
id: 16C
title: Note commands on the REST API
epic: parity
sprint: S16
depends_on: [16A, 16B]
review: low
est_effort: S
files:
  - src/api/notes.ts
  - new:tests/api/notes.test.ts
tests:
  - tests/api/notes.test.ts
adr: [ADR-0017]
status: pending
---
## Goal
One outcome.

## Out of scope
- Nothing else.
`

func TestParseGood(t *testing.T) {
	w, err := Parse("docs/plan/S16/16C_notes.md", []byte(good))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if w.ID != "16C" || w.Sprint != "S16" || w.Epic != "parity" {
		t.Errorf("ids: %+v", w)
	}
	if w.Review != ReviewLow {
		t.Errorf("review normalised to %q, want LOW", w.Review)
	}
	if strings.Join(w.DependsOn, ",") != "16A,16B" {
		t.Errorf("depends_on = %v", w.DependsOn)
	}
	if len(w.Files) != 2 || w.Files[0] != (File{Path: "src/api/notes.ts"}) || w.Files[1] != (File{Path: "tests/api/notes.test.ts", New: true}) {
		t.Errorf("files = %+v", w.Files)
	}
	if !w.HasSection("goal") || !w.HasSection("Out of scope") || w.HasSection("Acceptance") {
		t.Errorf("sections = %v", w.Sections)
	}
	if len(w.Hash) != 64 {
		t.Errorf("hash = %q", w.Hash)
	}
}

func TestParseDefaults(t *testing.T) {
	w, err := Parse("x.md", []byte("---\nid: A1\ntitle: t\nfiles: [a.go]\nno_tests_reason: docs only\n---\nbody\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if w.Review != ReviewLow || w.Effort != EffortS || w.Status != StatusPending {
		t.Errorf("defaults: review=%q effort=%q status=%q", w.Review, w.Effort, w.Status)
	}
}

func TestParseCRLFAndBOM(t *testing.T) {
	src := "\xef\xbb\xbf" + strings.ReplaceAll(good, "\n", "\r\n")
	w, err := Parse("x.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse CRLF: %v", err)
	}
	if w.ID != "16C" || !w.HasSection("goal") {
		t.Errorf("CRLF parse lost data: %+v", w)
	}
	// the hash identifies content, not line endings
	lf, _ := Parse("x.md", []byte(good))
	if lf.Hash != w.Hash {
		t.Error("hash differs between CRLF and LF copies of the same work order")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"no frontmatter":     "# 16C -- title\n",
		"unterminated":       "---\nid: A\n",
		"unknown key":        "---\nid: A\ntitle: t\nfiles: [a]\nscope_extra: [b]\n---\n",
		"anchor/alias":       "---\nid: A\ntitle: t\nfiles: &f [a]\ntests: *f\n---\n",
		"anchor only":        "---\nid: A\ntitle: t\nfiles: &f [a]\n---\n",
		"merge key":          "---\nid: A\ntitle: t\nfiles: [a]\nx: &b {q: 1}\n<<: *b\n---\n",
		"explicit tag":       "---\nid: A\ntitle: !!binary aGVsbG8=\nfiles: [a]\n---\n",
		"custom tag":         "---\nid: A\ntitle: !evil t\nfiles: [a]\n---\n",
		"missing id":         "---\ntitle: t\nfiles: [a]\n---\n",
		"bad id":             "---\nid: \"../x\"\ntitle: t\nfiles: [a]\n---\n",
		"missing title":      "---\nid: A\nfiles: [a]\n---\n",
		"no files":           "---\nid: A\ntitle: t\n---\n",
		"bad review":         "---\nid: A\ntitle: t\nfiles: [a]\nreview: URGENT\n---\n",
		"bad effort":         "---\nid: A\ntitle: t\nfiles: [a]\nest_effort: XL\n---\n",
		"bad status":         "---\nid: A\ntitle: t\nfiles: [a]\nstatus: maybe\n---\n",
		"path traversal":     "---\nid: A\ntitle: t\nfiles: [../a]\n---\n",
		"option injection":   "---\nid: A\ntitle: t\nfiles: [a]\ntests: [--exec=x]\n---\n",
		"self dependency":    "---\nid: A\ntitle: t\nfiles: [a]\ndepends_on: [A]\n---\n",
		"bad dependency id":  "---\nid: A\ntitle: t\nfiles: [a]\ndepends_on: [\"a b\"]\n---\n",
		"duplicate key":      "---\nid: A\nid: B\ntitle: t\nfiles: [a]\n---\n",
		"wrong type":         "---\nid: A\ntitle: t\nfiles: a\n---\n",
		"new: on test entry": "---\nid: A\ntitle: t\nfiles: [a]\ntests: [\"new:t\"]\n---\n",
	}
	for name, src := range cases {
		if _, err := Parse("x.md", []byte(src)); err == nil {
			t.Errorf("%s: Parse accepted %q", name, src)
		}
	}
}

func TestParseSizeCap(t *testing.T) {
	big := "---\nid: A\ntitle: " + strings.Repeat("x", MaxFrontmatterBytes) + "\nfiles: [a]\n---\n"
	if _, err := Parse("x.md", []byte(big)); err == nil {
		t.Error("oversized frontmatter accepted")
	}
}

func TestParseErrorListsEveryProblem(t *testing.T) {
	_, err := Parse("x.md", []byte("---\nid: A\nreview: URGENT\n---\n"))
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	for _, want := range []string{"title", "files", "review"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestParseReadOnlyTests(t *testing.T) {
	src := `---
id: 01A.impl
title: Implementation
files: [pkg/foo.go]
tests: [pkg/foo_test.go]
read_only_tests: true
---
## Goal
Implement foo.
`
	w, err := Parse("test.md", []byte(src))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if !w.ReadOnlyTests {
		t.Errorf("expected ReadOnlyTests to be true, got %v", w.ReadOnlyTests)
	}
}

