package onboard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/config"
)

func TestDetectEnvironment_Go(t *testing.T) {
	tmp := t.TempDir()
	goMod := "module github.com/example/my-go-service\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	env := DetectEnvironment(tmp)
	if env.ProjectName != "my-go-service" {
		t.Errorf("got ProjectName %q, want %q", env.ProjectName, "my-go-service")
	}
	if env.Language != "Go" {
		t.Errorf("got Language %q, want %q", env.Language, "Go")
	}
	if len(env.TestCommand) == 0 || env.TestCommand[0] != "go" {
		t.Errorf("expected go test command, got %v", env.TestCommand)
	}
}

func TestDetectEnvironment_Node(t *testing.T) {
	tmp := t.TempDir()
	pkgJSON := `{"name": "web-frontend", "version": "1.0.0"}`
	if err := os.WriteFile(filepath.Join(tmp, "package.json"), []byte(pkgJSON), 0644); err != nil {
		t.Fatal(err)
	}

	env := DetectEnvironment(tmp)
	if env.ProjectName != "web-frontend" {
		t.Errorf("got ProjectName %q, want %q", env.ProjectName, "web-frontend")
	}
	if env.Language != "TypeScript/Node" {
		t.Errorf("got Language %q, want %q", env.Language, "TypeScript/Node")
	}
}

func TestDetectEnvironment_Rust(t *testing.T) {
	tmp := t.TempDir()
	cargoToml := `[package]
name = "rust-engine"
version = "0.1.0"
`
	if err := os.WriteFile(filepath.Join(tmp, "Cargo.toml"), []byte(cargoToml), 0644); err != nil {
		t.Fatal(err)
	}

	env := DetectEnvironment(tmp)
	if env.ProjectName != "rust-engine" {
		t.Errorf("got ProjectName %q, want %q", env.ProjectName, "rust-engine")
	}
	if env.Language != "Rust" {
		t.Errorf("got Language %q, want %q", env.Language, "Rust")
	}
}

func TestDetectEnvironment_Python(t *testing.T) {
	tmp := t.TempDir()
	pyproject := `[project]
name = "ml-service"
version = "0.1.0"
`
	if err := os.WriteFile(filepath.Join(tmp, "pyproject.toml"), []byte(pyproject), 0644); err != nil {
		t.Fatal(err)
	}

	env := DetectEnvironment(tmp)
	if env.ProjectName != "ml-service" {
		t.Errorf("got ProjectName %q, want %q", env.ProjectName, "ml-service")
	}
	if env.Language != "Python" {
		t.Errorf("got Language %q, want %q", env.Language, "Python")
	}
}

func TestGenerateShepherdTOML_ValidAllCLIs(t *testing.T) {
	clis := []string{"agy", "claude", "codex", "opencode", "aider", "gemini"}

	for _, cli := range clis {
		t.Run(cli, func(t *testing.T) {
			env := &Environment{
				ProjectName:      "test-app",
				Language:         "Go",
				CurrentBranch:    "main",
				RecommendedCLI:   cli,
				GatesToml:        `  { name = "tests", argv = ["go", "test", "{test_files}"], hard = true }`,
				RecommendedSlots: 2,
				NumGPUs:          1,
			}
			cfg := Config{
				ProjectName: "test-app",
				TargetCLI:   cli,
			}
			tomlStr, err := GenerateShepherdTOML(cfg, env)
			if err != nil {
				t.Fatalf("GenerateShepherdTOML failed for %s: %v", cli, err)
			}

			// Must parse cleanly with strict config.LoadBytes
			parsed, err := config.LoadBytes([]byte(tomlStr))
			if err != nil {
				t.Fatalf("config.LoadBytes failed for %s TOML: %v\nContent:\n%s", cli, err, tomlStr)
			}
			if parsed.Project.Name != "test-app" {
				t.Errorf("got project name %q, want test-app", parsed.Project.Name)
			}
			if parsed.Slots.MaxSlots != 2 {
				t.Errorf("got max slots %d, want 2", parsed.Slots.MaxSlots)
			}
		})
	}
}

func TestGenerateLaunchMD(t *testing.T) {
	env := &Environment{
		ProjectName:    "payment-gateway",
		Language:       "Go",
		CurrentBranch:  "main",
		RecommendedCLI: "claude",
	}
	cfg := Config{
		ProjectName: "payment-gateway",
		Goal:        "Implement Stripe webhooks and idempotent event storage",
		TargetCLI:   "claude",
	}

	md := GenerateLaunchMD(cfg, env)

	// Check essential sections and rules in launch.md
	checks := []string{
		"Shepherd AI Onboarding Blueprint: payment-gateway",
		"payment-gateway",
		"Stripe webhooks and idempotent event storage",
		"docs/plan/S01/",
		"docs/adr/",
		"new:path/to/new_file.ext",
		"## Goal",
		"## Out of scope",
		"## Acceptance",
		"shepherd plan lint",
	}
	for _, c := range checks {
		if !strings.Contains(md, c) {
			t.Errorf("launch.md missing expected substring: %q", c)
		}
	}
}

func TestBuildLaunchCommand(t *testing.T) {
	tests := []struct {
		cli     string
		wantCmd string
	}{
		{"agy", "agy"},
		{"claude", "claude"},
		{"codex", "codex"},
		{"opencode", "opencode"},
		{"aider", "aider"},
		{"gemini", "gemini"},
	}

	for _, tt := range tests {
		t.Run(tt.cli, func(t *testing.T) {
			cmd, args, stdin, err := BuildLaunchCommand(tt.cli, "launch.md")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cmd != tt.wantCmd {
				t.Errorf("got cmd %q, want %q", cmd, tt.wantCmd)
			}
			if len(args) == 0 {
				t.Errorf("expected non-empty args for %s", tt.cli)
			}
			if tt.cli == "codex" && stdin == "" {
				t.Errorf("expected non-empty stdin for codex")
			}
		})
	}

	_, _, _, err := BuildLaunchCommand("unknown-cli", "launch.md")
	if err == nil {
		t.Errorf("expected error for unknown CLI")
	}
}

func TestScaffold(t *testing.T) {
	tmp := t.TempDir()
	goMod := "module github.com/example/inventory\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		ProjectRoot: tmp,
		Goal:        "Bootstrap inventory stock reconciliation",
		TargetCLI:   "agy",
	}

	res, err := Scaffold(cfg)
	if err != nil {
		t.Fatalf("Scaffold failed: %v", err)
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

	for _, ef := range expectedFiles {
		if _, err := os.Stat(ef); err != nil {
			t.Errorf("expected file %s does not exist", ef)
		}
	}

	if res.LaunchCommand != "agy" {
		t.Errorf("got LaunchCommand %q, want agy", res.LaunchCommand)
	}
}

func TestScaffold_DryRun(t *testing.T) {
	tmp := t.TempDir()

	cfg := Config{
		ProjectRoot: tmp,
		ProjectName: "dry-run-demo",
		TargetCLI:   "claude",
		DryRun:      true,
	}

	res, err := Scaffold(cfg)
	if err != nil {
		t.Fatalf("Scaffold failed: %v", err)
	}

	if len(res.FilesCreated) == 0 {
		t.Errorf("expected preview of files created")
	}

	// Verify nothing was actually written to disk
	if _, err := os.Stat(filepath.Join(tmp, "shepherd.toml")); err == nil {
		t.Errorf("shepherd.toml should not be written during dry-run")
	}
}

func TestRunInteractive(t *testing.T) {
	env := &Environment{
		ProjectName:      "auto-proj",
		Language:         "Go",
		RecommendedCLI:   "agy",
		InstalledCLIs:    []string{"agy", "claude"},
		AvailableRAMGB:   16.0,
		NumGPUs:          1,
		RecommendedSlots: 1,
	}

	input := "custom-name\nCustom Goal\nclaude\ny\n"
	in := bytes.NewBufferString(input)
	var out bytes.Buffer

	cfg, err := RunInteractive(in, &out, env, Config{ProjectRoot: "."})
	if err != nil {
		t.Fatalf("RunInteractive failed: %v", err)
	}

	if cfg.ProjectName != "custom-name" {
		t.Errorf("got ProjectName %q, want custom-name", cfg.ProjectName)
	}
	if cfg.Goal != "Custom Goal" {
		t.Errorf("got Goal %q, want 'Custom Goal'", cfg.Goal)
	}
	if cfg.TargetCLI != "claude" {
		t.Errorf("got TargetCLI %q, want claude", cfg.TargetCLI)
	}
	if !cfg.AutoLaunch {
		t.Errorf("expected AutoLaunch = true")
	}
}

func TestGenerateShepherdTOML_IncludesLocalOpenCodeInStack(t *testing.T) {
	env := &Environment{
		ProjectName:      "multi-stack-app",
		Language:         "Go",
		CurrentBranch:    "main",
		RecommendedCLI:   "agy",
		InstalledCLIs:    []string{"agy", "claude", "opencode"},
		GatesToml:        `  { name = "tests", argv = ["go", "test", "{test_files}"], hard = true }`,
		RecommendedSlots: 2,
		NumGPUs:          1,
		OpenCode: OpenCodeInfo{
			Installed:    true,
			ProviderID:   "llamacpp",
			ProviderName: "llama.cpp (local)",
			BaseURL:      "http://127.0.0.1:8080/v1",
			DefaultModel: "llamacpp/gemma-4-12B-it-Q4_K_M.gguf",
		},
	}

	tomlStr, err := GenerateShepherdTOML(Config{
		ProjectName: "multi-stack-app",
		TargetCLI:   "agy",
	}, env)
	if err != nil {
		t.Fatalf("GenerateShepherdTOML failed: %v", err)
	}

	parsed, err := config.LoadBytes([]byte(tomlStr))
	if err != nil {
		t.Fatalf("LoadBytes failed: %v\nTOML:\n%s", err, tomlStr)
	}

	localTier, ok := parsed.Tiers["local"]
	if !ok {
		t.Fatalf("expected [tiers.local] in generated shepherd.toml, got tiers: %+v", parsed.Tiers)
	}
	if localTier.Adapter != "opencode" {
		t.Errorf("got local tier adapter %q, want opencode", localTier.Adapter)
	}
	if localTier.Model != "llamacpp/gemma-4-12B-it-Q4_K_M.gguf" {
		t.Errorf("got local tier model %q, want llamacpp/gemma-4-12B-it-Q4_K_M.gguf", localTier.Model)
	}
	if _, ok := parsed.Quota.Pools["local"]; !ok {
		t.Errorf("expected [quota.pools.local] in generated shepherd.toml")
	}
	if len(parsed.Repair.Chain) == 0 || parsed.Repair.Chain[0] != "local:3" {
		t.Errorf("expected local:3 at start of repair chain, got %v", parsed.Repair.Chain)
	}
}

func TestConfigWizard_ToggleReorderCapAndSave(t *testing.T) {
	env := &Environment{
		ProjectName:      "wizard-app",
		Language:         "Go",
		CurrentBranch:    "main",
		RecommendedCLI:   "agy",
		GatesToml:        `  { name = "tests", argv = ["go", "test", "{test_files}"], hard = true }`,
		RecommendedSlots: 2,
		NumGPUs:          1,
		Providers: []DiscoveredProvider{
			{
				ID: "local", Name: "Local Router", Kind: "router", Adapter: "opencode",
				Endpoint: "http://127.0.0.1:8080/v1", DefaultModel: "model-a",
				Models: []string{"model-a", "model-b"}, Pool: "local", PoolMode: "until-rejected",
				UsageCapPct: 100, Attempts: 3, Enabled: true, Priority: 1,
			},
			{
				ID: "sonnet", Name: "Antigravity Sonnet", Kind: "cli", Adapter: "agy",
				DefaultModel: "sonnet", Models: []string{"sonnet", "opus"},
				Pool: "claude", PoolMode: "polled", FloorPct: 25,
				UsageCapPct: 100, Attempts: 2, Enabled: true, Priority: 2,
			},
			{
				ID: "codex", Name: "Codex CLI", Kind: "cli", Adapter: "codex",
				DefaultModel: "gpt-5.6-luna", Pool: "codex", PoolMode: "until-rejected",
				UsageCapPct: 100, Attempts: 2, Enabled: true, Priority: 3,
			},
		},
	}

	// Keys:
	// 'm' -> cycle model on #1 (local: model-a -> model-b)
	// 'c' -> cycle usage cap on #1 (100% -> 80%)
	// 'j' -> select #2 (sonnet)
	// 'u' -> move sonnet UP to priority #1
	// 'j' -> select #2 (local)
	// 'j' -> select #3 (codex)
	// ' ' -> toggle codex OFF
	// 'r' -> cycle routing rule (0.50 -> 0.75 Local-First)
	// 'b' -> cycle sprint budget cap ($0 -> $5.00)
	// 's' -> save & exit
	keys := bytes.NewBufferString("mcjujj rbs")
	var out bytes.Buffer

	cfg, err := RunConfigWizard(t.Context(), keys, &out, env, Config{ProjectRoot: "."})
	if err != nil {
		t.Fatalf("RunConfigWizard failed: %v", err)
	}

	if len(cfg.Providers) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(cfg.Providers))
	}
	if cfg.Providers[0].ID != "sonnet" || cfg.Providers[0].Priority != 1 {
		t.Errorf("expected sonnet moved to priority #1, got %+v", cfg.Providers[0])
	}
	if cfg.Providers[1].ID != "local" || cfg.Providers[1].DefaultModel != "model-b" || cfg.Providers[1].UsageCapPct != 80 {
		t.Errorf("expected local at #2 with model-b and 80%% cap, got %+v", cfg.Providers[1])
	}
	if cfg.Providers[2].ID != "codex" || cfg.Providers[2].Enabled {
		t.Errorf("expected codex deactivated at #3, got %+v", cfg.Providers[2])
	}
	if cfg.CloudFirstThreshold != 0.75 {
		t.Errorf("expected CloudFirstThreshold 0.75, got %v", cfg.CloudFirstThreshold)
	}
	if cfg.MaxSprintCostUSD != 5.0 {
		t.Errorf("expected MaxSprintCostUSD 5.0, got %v", cfg.MaxSprintCostUSD)
	}

	tomlStr, err := GenerateShepherdTOML(cfg, env)
	if err != nil {
		t.Fatalf("GenerateShepherdTOML failed: %v", err)
	}
	parsed, err := config.LoadBytes([]byte(tomlStr))
	if err != nil {
		t.Fatalf("LoadBytes failed: %v\n%s", err, tomlStr)
	}
	// Chain should only include enabled providers in priority order: sonnet:2, local:3
	if len(parsed.Repair.Chain) != 2 || parsed.Repair.Chain[0] != "sonnet:2" || parsed.Repair.Chain[1] != "local:3" {
		t.Errorf("unexpected repair chain: %v", parsed.Repair.Chain)
	}
	if parsed.Tiers["codex"].IsEnabled() {
		t.Errorf("expected codex tier to be disabled in shepherd.toml")
	}
}

func TestDetectAgyInfo_And_GeminiFlashDiscovery(t *testing.T) {
	info := DetectAgyInfo(t.Context())
	if info.Installed {
		if len(info.Models) == 0 {
			t.Errorf("expected agy models to be populated")
		}
		if info.ActiveModel == "" {
			t.Errorf("expected active model to be detected")
		}
	}
}

func TestGenerateShepherdTOML_AgyGeminiFlash(t *testing.T) {
	env := &Environment{
		ProjectName:      "flash-app",
		Language:         "Go",
		CurrentBranch:    "main",
		RecommendedCLI:   "agy",
		GatesToml:        `  { name = "tests", argv = ["go", "test", "{test_files}"], hard = true }`,
		RecommendedSlots: 2,
		NumGPUs:          1,
		Agy: AgyInfo{
			Installed:   true,
			ActiveModel: "gemini-3.8-flash-medium",
			ActiveTitle: "Gemini 3.8 Flash (Medium)",
			Models:      []string{"gemini-3.8-flash-medium", "gemini-4-argon-medium"},
		},
	}
	cfg := Config{
		ProjectName: "flash-app",
		TargetCLI:   "agy",
	}
	tomlStr, err := GenerateShepherdTOML(cfg, env)
	if err != nil {
		t.Fatalf("GenerateShepherdTOML failed: %v", err)
	}

	parsed, err := config.LoadBytes([]byte(tomlStr))
	if err != nil {
		t.Fatalf("LoadBytes failed: %v\n%s", err, tomlStr)
	}

	flashTier, ok := parsed.Tiers["flash"]
	if !ok {
		t.Fatalf("expected tiers.flash to exist")
	}
	if flashTier.Model != "gemini-3.8-flash-medium" {
		t.Errorf("expected flash model gemini-3.8-flash-medium, got %q", flashTier.Model)
	}
	if flashTier.Pool != "gemini" {
		t.Errorf("expected flash pool gemini, got %q", flashTier.Pool)
	}

	geminiPool, ok := parsed.Quota.Pools["gemini"]
	if !ok {
		t.Fatalf("expected quota.pools.gemini to exist")
	}
	if geminiPool.Mode != "polled" {
		t.Errorf("expected gemini pool mode polled, got %q", geminiPool.Mode)
	}

	if parsed.Repair.MechanicalFix.Tier != "flash" {
		t.Errorf("expected mechanical_fix.tier flash, got %q", parsed.Repair.MechanicalFix.Tier)
	}
}



