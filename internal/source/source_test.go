package source_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/source"
)

func TestSource_MarkdownDir(t *testing.T) {
	dir := t.TempDir()
	chunkDir := filepath.Join(dir, "docs", "plan", "S01")
	_ = os.MkdirAll(chunkDir, 0755)

	content := "---\nid: 01A\ntitle: Markdown Feature\nsprint: S01\nfiles:\n  - src/feature.go\nno_tests_reason: ok\n---\n## Goal\nTest\n"
	_ = os.WriteFile(filepath.Join(chunkDir, "01A.md"), []byte(content), 0644)
	_ = os.WriteFile(filepath.Join(chunkDir, "README.md"), []byte("# Notes"), 0644)

	src := &source.MarkdownDirSource{Root: filepath.Join(dir, "docs", "plan")}
	orders, err := src.FetchWorkOrders(context.Background())
	if err != nil {
		t.Fatalf("FetchWorkOrders: %v", err)
	}

	if len(orders) != 1 {
		t.Fatalf("expected 1 work order, got %d", len(orders))
	}
	if orders[0].ID != "01A" {
		t.Errorf("ID = %s, want 01A", orders[0].ID)
	}
}

func TestSource_JSONFile(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "queue.json")
	content := `[
		{
			"id": "J01",
			"title": "JSON Work Order",
			"status": "pending",
			"files": [{"path": "main.go"}]
		}
	]`
	if err := os.WriteFile(jsonPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	src := &source.JSONFileSource{Path: jsonPath}
	orders, err := src.FetchWorkOrders(context.Background())
	if err != nil {
		t.Fatalf("FetchWorkOrders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if orders[0].ID != "J01" {
		t.Errorf("ID = %s, want J01", orders[0].ID)
	}
}

func TestSource_GitHubIssues(t *testing.T) {
	fakeJSON := `[
		{
			"number": 42,
			"title": "Add OAuth2 integration",
			"body": "---\nid: 42A\ntitle: OAuth2\nfiles:\n  - src/oauth.go\nno_tests_reason: ok\n---\n## Goal\nImplement OAuth\n"
		},
		{
			"number": 43,
			"title": "Fix memory leak in parser",
			"body": "Plain issue description without frontmatter"
		}
	]`

	src := &source.GitHubIssuesSource{
		JSONData: []byte(fakeJSON),
	}

	orders, err := src.FetchWorkOrders(context.Background())
	if err != nil {
		t.Fatalf("FetchWorkOrders: %v", err)
	}

	if len(orders) != 2 {
		t.Fatalf("expected 2 work orders, got %d", len(orders))
	}

	if orders[0].ID != "42A" {
		t.Errorf("order[0] ID = %s, want 42A", orders[0].ID)
	}
	if orders[1].ID != "GH-43" {
		t.Errorf("order[1] ID = %s, want GH-43", orders[1].ID)
	}
}

func TestSource_MarkdownDir_SkipsDoneDir(t *testing.T) {
	dir := t.TempDir()
	planDir := filepath.Join(dir, "docs", "plan")
	s01Dir := filepath.Join(planDir, "S01")
	doneDir := filepath.Join(planDir, "_done")
	_ = os.MkdirAll(s01Dir, 0755)
	_ = os.MkdirAll(doneDir, 0755)

	pending := "---\nid: 01A\ntitle: Pending\nfiles:\n  - a.go\nno_tests_reason: ok\n---\n"
	done := "---\nid: 01B\ntitle: Done\nstatus: done\nfiles:\n  - b.go\nno_tests_reason: ok\n---\n"

	_ = os.WriteFile(filepath.Join(s01Dir, "01A.md"), []byte(pending), 0644)
	_ = os.WriteFile(filepath.Join(doneDir, "01B.md"), []byte(done), 0644)

	src := &source.MarkdownDirSource{Root: planDir}
	orders, err := src.FetchWorkOrders(context.Background())
	if err != nil {
		t.Fatalf("FetchWorkOrders: %v", err)
	}

	if len(orders) != 1 {
		t.Fatalf("expected 1 work order (skipping _done), got %d", len(orders))
	}
	if orders[0].ID != "01A" {
		t.Errorf("expected 01A, got %s", orders[0].ID)
	}
}

func TestMarkDone(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "01A.md")
	content := "---\nid: 01A\ntitle: Work\nstatus: in-progress\n---\n# Work\n"
	_ = os.WriteFile(filePath, []byte(content), 0644)

	if err := source.MarkDone(filePath, "01A"); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}

	data, _ := os.ReadFile(filePath)
	if !os.FileMode(0).IsDir() && !strings.Contains(string(data), "status: done") {
		t.Errorf("expected status: done, got:\n%s", string(data))
	}
}

