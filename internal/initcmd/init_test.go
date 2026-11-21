package initcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/config"
)

func TestInit_GoProject(t *testing.T) {
	tempDir := t.TempDir()

	// Create fake go.mod
	if err := os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(tempDir, Options{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if !strings.Contains(report, "shepherd.toml created") {
		t.Errorf("expected report to mention shepherd.toml created, got: %s", report)
	}

	// Verify shepherd.toml was created and passes strict validation
	cfgBytes, err := os.ReadFile(filepath.Join(tempDir, "shepherd.toml"))
	if err != nil {
		t.Fatalf("ReadFile shepherd.toml: %v", err)
	}
	cfg, err := config.LoadBytes(cfgBytes)
	if err != nil {
		t.Fatalf("generated shepherd.toml failed validation: %v", err)
	}
	if cfg.Project.Name == "" {
		t.Errorf("expected non-empty project name")
	}

	// Verify prompt templates were scaffolded
	promptFile := filepath.Join(tempDir, ".shepherd", "prompts", "coder.md")
	if _, err := os.Stat(promptFile); os.IsNotExist(err) {
		t.Errorf("expected .shepherd/prompts/coder.md to exist")
	}

	// Second run without force should fail
	_, err2 := Run(tempDir, Options{})
	if err2 == nil {
		t.Errorf("expected error when shepherd.toml exists without force")
	}

	// Second run with force should succeed
	_, err3 := Run(tempDir, Options{Force: true})
	if err3 != nil {
		t.Errorf("expected force init to succeed: %v", err3)
	}
}

func TestInit_NodeProject(t *testing.T) {
	tempDir := t.TempDir()

	// Create fake package.json
	pkgJSON := `{"name": "my-node-app", "scripts": {"test": "vitest"}}`
	if err := os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(pkgJSON), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Run(tempDir, Options{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	cfgBytes, err := os.ReadFile(filepath.Join(tempDir, "shepherd.toml"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	cfg, err := config.LoadBytes(cfgBytes)
	if err != nil {
		t.Fatalf("config validation: %v", err)
	}
	if cfg.Project.Name != "my-node-app" {
		t.Errorf("project name = %q, want 'my-node-app'", cfg.Project.Name)
	}
}
