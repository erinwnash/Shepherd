package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdOnboard_DryRun(t *testing.T) {
	tmp := t.TempDir()
	goMod := "module github.com/test/demo\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"onboard", "--dir", tmp, "--cli", "claude", "--goal", "Test Goal", "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Shepherd Onboarding: demo") {
		t.Errorf("stdout missing project header: %s", out)
	}
	if !strings.Contains(out, "Dry-run preview") {
		t.Errorf("stdout missing dry-run notice: %s", out)
	}
	if !strings.Contains(out, "claude") {
		t.Errorf("stdout missing target CLI claude: %s", out)
	}
}

func TestCmdWizard_Alias(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"wizard", "--dir", tmp, "--cli", "agy", "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("wizard alias expected code 0, got %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Shepherd Onboarding") {
		t.Errorf("stdout missing onboarding title: %s", stdout.String())
	}
}

func TestCmdOnboard_RealScaffold(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"onboard", "--dir", tmp, "--name", "real-proj", "--cli", "codex", "--goal", "Build auth service"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0, got %d, stderr: %s", code, stderr.String())
	}

	expectedFiles := []string{
		filepath.Join(tmp, "shepherd.toml"),
		filepath.Join(tmp, "launch.md"),
		filepath.Join(tmp, "docs/plan/epic.md"),
		filepath.Join(tmp, "docs/plan/S01/sprint.md"),
		filepath.Join(tmp, "docs/adr/0001-record-architecture-decisions.md"),
		filepath.Join(tmp, ".shepherd/prompts/coder.md"),
		filepath.Join(tmp, ".shepherd/prompts/repair.md"),
	}

	for _, f := range expectedFiles {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("expected generated file %s not found", f)
		}
	}
}
