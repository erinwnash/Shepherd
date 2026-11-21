package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTree(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.go", "package a\n")
	write(t, root, "docs/adr/0003-x.md", "# x\n")
	write(t, root, "docs/plan/EPIC-x/epic.md", "# epic, not a chunk\n")
	write(t, root, "docs/plan/EPIC-x/S1_one/sprint.md", "# sprint, not a chunk\n")
	write(t, root, "docs/plan/EPIC-x/S1_one/1A_a.md",
		"---\nid: 1A\ntitle: a\nsprint: S1\nfiles: [src/a.go]\nno_tests_reason: x\nadr: [ADR-0003]\n---\n## Goal\ng\n## Out of scope\nx\n## Acceptance\ny\n")
	write(t, root, "docs/plan/EPIC-x/S2_two/2A_b.md",
		"---\nid: 2A\ntitle: b\nsprint: S2\nfiles: [new:src/b.go]\nno_tests_reason: x\ndepends_on: [1A]\n---\n## Goal\ng\n## Out of scope\nx\n## Acceptance\ny\n")
	write(t, root, "docs/plan/EPIC-x/S2_two/2B_broken.md", "no frontmatter\n")

	orders, bad, err := Load(root, "docs/plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 2 || orders[0].ID != "1A" || orders[1].ID != "2A" {
		t.Fatalf("orders = %v", orders)
	}
	if len(bad) != 1 || bad[0].Code != "schema" || !strings.HasSuffix(bad[0].Chunk, "2B_broken.md") {
		t.Errorf("bad = %+v", bad)
	}
	if got := strings.Join(SprintOrder(orders), ","); got != "S1,S2" {
		t.Errorf("sprint order = %s", got)
	}
	fs := Lint(orders, Options{SprintOrder: SprintOrder(orders)}, DirRepo{Root: root, ADRDir: "docs/adr"})
	if len(fs) != 0 {
		t.Errorf("findings on a clean tree: %+v", fs)
	}
}

func TestADRExistsRejectsGlobs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "docs/adr/0001-x.md", "x")
	r := DirRepo{Root: root, ADRDir: "docs/adr"}
	if !r.ADRExists("ADR-0001") || !r.ADRExists("0001") {
		t.Error("existing ADR not found")
	}
	for _, id := range []string{"ADR-*", "ADR-000?", "ADR-[0]001", "ADR-../x", "ADR-"} {
		if r.ADRExists(id) {
			t.Errorf("ADRExists(%q) = true", id)
		}
	}
}
