package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctor_MissingConfig(t *testing.T) {
	tempDir := t.TempDir()

	checks, allOk := Check(tempDir)
	if allOk {
		t.Errorf("expected doctor check to fail in empty directory")
	}

	foundConfigCheck := false
	for _, c := range checks {
		if c.Name == "shepherd.toml" {
			foundConfigCheck = true
			if c.Passed {
				t.Errorf("expected shepherd.toml check to fail when missing")
			}
			if !strings.Contains(c.Fix, "shepherd init") {
				t.Errorf("expected fix recommendation to mention 'shepherd init', got: %s", c.Fix)
			}
		}
	}
	if !foundConfigCheck {
		t.Errorf("shepherd.toml check missing")
	}
}

func TestDoctor_ValidConfigAndGit(t *testing.T) {
	tempDir := t.TempDir()

	// Create fake .git directory
	if err := os.MkdirAll(filepath.Join(tempDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create valid shepherd.toml
	cfgContent := `
schema_version = 1
[project]
name = "doctor-test"
[queue]
source = "markdown-dir"
root = "docs/plan"
`
	if err := os.WriteFile(filepath.Join(tempDir, "shepherd.toml"), []byte(cfgContent), 0644); err != nil {
		t.Fatal(err)
	}

	checks, _ := Check(tempDir)
	for _, c := range checks {
		if c.Name == "shepherd.toml" && !c.Passed {
			t.Errorf("shepherd.toml check failed: %s", c.Detail)
		}
		if c.Name == "git repository" && !c.Passed {
			t.Errorf("git repository check failed: %s", c.Detail)
		}
	}
}
