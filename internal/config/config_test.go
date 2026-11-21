package config

import (
	"strings"
	"testing"
)

const sampleValidTOML = `
schema_version = 1

[project]
name = "demo"
repo = "."
base_branch = "main"

[queue]
source = "markdown-dir"
root = "docs/plan"
done_marker = "move-to-done"

[verify]
gates = [
  { name = "content-loss", builtin = "content-loss" },
  { name = "scope", builtin = "scope", allow_from = "work-order:files" },
  { name = "typecheck", argv = ["go", "vet", "./..."], hard = true },
  { name = "tests", argv = ["go", "test", "{test_files}"], hard = true },
  { name = "lint", argv = ["golangci-lint", "run", "{touched_files}"], hard = false, glob = "*.go" },
]

[repair]
chain = ["haiku:1", "sonnet:2"]
mechanical_fix = { tier = "haiku", max_errors = 3, codes = ["TS2304"] }

[tiers.haiku]
adapter = "agy"
model = "claude-3-5-haiku"
pool = "claude"

[tiers.sonnet]
adapter = "agy"
model = "claude-3-7-sonnet"
pool = "claude"

[tiers.luna]
adapter = "codex"
model = "gpt-5.6-luna"
pool = "codex"

[quota.pools.claude]
mode = "polled"
usage_cmd = ["agy", "-p", "/usage"]
parser = "agy-usage"
floor_weekly_pct = 25

[quota.pools.codex]
mode = "until-rejected"
backoff_min = [60, 180, 300]

[decide]
backend = "rules"
timeout_ms = 2000

[policy]
max_consecutive_blocks = 3
wait_for_reset_max_min = 45
cloud_first_threshold = 0.5
`

func TestLoad_ValidConfig(t *testing.T) {
	cfg, err := LoadBytes([]byte(sampleValidTOML))
	if err != nil {
		t.Fatalf("LoadBytes failed: %v", err)
	}

	if cfg.SchemaVersion != 1 {
		t.Errorf("got SchemaVersion = %d, want 1", cfg.SchemaVersion)
	}
	if cfg.Project.Name != "demo" {
		t.Errorf("got Project.Name = %q, want demo", cfg.Project.Name)
	}
	if len(cfg.Verify.Gates) != 5 {
		t.Errorf("got %d gates, want 5", len(cfg.Verify.Gates))
	}
	vGates := cfg.VerifierGates()
	if len(vGates) != 5 {
		t.Errorf("got %d verifier gates, want 5", len(vGates))
	}
	if vGates[0].Builtin != "content-loss" {
		t.Errorf("vGates[0].Builtin = %q, want content-loss", vGates[0].Builtin)
	}
	if len(cfg.Tiers) != 3 {
		t.Errorf("got %d tiers, want 3", len(cfg.Tiers))
	}
	if cfg.Quota.Pools["claude"].Mode != "polled" {
		t.Errorf("got claude mode %q, want polled", cfg.Quota.Pools["claude"].Mode)
	}
	if cfg.Quota.Pools["codex"].Mode != "until-rejected" {
		t.Errorf("got codex mode %q, want until-rejected", cfg.Quota.Pools["codex"].Mode)
	}
	if cfg.Queue.DoneDir != "docs/planDONE" {
		t.Errorf("got Queue.DoneDir = %q, want docs/planDONE", cfg.Queue.DoneDir)
	}
	if cfg.Queue.DoneMarker != "move-to-done" {
		t.Errorf("got Queue.DoneMarker = %q, want move-to-done", cfg.Queue.DoneMarker)
	}
}

func TestLoad_CRLFAndBOM(t *testing.T) {
	crlfToml := "\xef\xbb\xbf" + strings.ReplaceAll(sampleValidTOML, "\n", "\r\n")
	cfg, err := LoadBytes([]byte(crlfToml))
	if err != nil {
		t.Fatalf("LoadBytes with CRLF+BOM failed: %v", err)
	}
	if cfg.Project.Name != "demo" {
		t.Errorf("got Project.Name = %q, want demo", cfg.Project.Name)
	}
}

func TestLoad_RejectsUnknownKeys(t *testing.T) {
	cases := []struct {
		name string
		toml string
	}{
		{
			name: "top-level unknown key",
			toml: sampleValidTOML + "\nextra_key = 123\n",
		},
		{
			name: "section unknown key",
			toml: strings.Replace(sampleValidTOML, `name = "demo"`, "name = \"demo\"\nunknown_field = true", 1),
		},
		{
			name: "tier unknown key",
			toml: strings.Replace(sampleValidTOML, `adapter = "agy"`, "adapter = \"agy\"\nrogue_field = 1", 1),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.toml))
			if err == nil {
				t.Fatalf("expected error for unknown key in %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "unknown") && !strings.Contains(err.Error(), "strict") {
				t.Logf("got error: %v", err)
			}
		})
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(string) string
		errSubstr string
	}{
		{
			name: "unsupported schema version",
			mutate: func(s string) string {
				return strings.Replace(s, "schema_version = 1", "schema_version = 2", 1)
			},
			errSubstr: "schema_version",
		},
		{
			name: "missing project name",
			mutate: func(s string) string {
				return strings.Replace(s, `name = "demo"`, `name = ""`, 1)
			},
			errSubstr: "project.name",
		},
		{
			name: "invalid pool mode",
			mutate: func(s string) string {
				return strings.Replace(s, `mode = "polled"`, `mode = "invalid-mode"`, 1)
			},
			errSubstr: "mode",
		},
		{
			name: "polled pool missing usage_cmd",
			mutate: func(s string) string {
				return strings.Replace(s, `usage_cmd = ["agy", "-p", "/usage"]`, `usage_cmd = []`, 1)
			},
			errSubstr: "usage_cmd",
		},
		{
			name: "until-rejected pool missing backoff_min",
			mutate: func(s string) string {
				return strings.Replace(s, `backoff_min = [60, 180, 300]`, `backoff_min = []`, 1)
			},
			errSubstr: "backoff_min",
		},
		{
			name: "disallowed placeholder in gate",
			mutate: func(s string) string {
				return strings.Replace(s, `"{test_files}"`, `"{arbitrary_cmd}"`, 1)
			},
			errSubstr: "placeholder",
		},
		{
			name: "repair chain refers to unknown tier",
			mutate: func(s string) string {
				return strings.Replace(s, `"haiku:1"`, `"nonexistent_tier:1"`, 1)
			},
			errSubstr: "nonexistent_tier",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			badToml := tc.mutate(sampleValidTOML)
			_, err := LoadBytes([]byte(badToml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.errSubstr)) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.errSubstr)
			}
		})
	}
}

func TestLoad_Defaults(t *testing.T) {
	minimalTOML := `
schema_version = 1

[project]
name = "minimal"

[queue]
source = "markdown-dir"
root = "docs/plan"
`
	cfg, err := LoadBytes([]byte(minimalTOML))
	if err != nil {
		t.Fatalf("LoadBytes minimal: %v", err)
	}

	if cfg.Project.Repo != "." {
		t.Errorf("got Project.Repo = %q, want '.'", cfg.Project.Repo)
	}
	if cfg.Project.BaseBranch != "main" {
		t.Errorf("got Project.BaseBranch = %q, want 'main'", cfg.Project.BaseBranch)
	}
	if cfg.Policy.MaxConsecutiveBlocks != 3 {
		t.Errorf("got MaxConsecutiveBlocks = %d, want 3", cfg.Policy.MaxConsecutiveBlocks)
	}
	if cfg.Policy.WaitForResetMaxMin != 45 {
		t.Errorf("got WaitForResetMaxMin = %d, want 45", cfg.Policy.WaitForResetMaxMin)
	}
	if cfg.Policy.CloudFirstThreshold != 0.5 {
		t.Errorf("got CloudFirstThreshold = %f, want 0.5", cfg.Policy.CloudFirstThreshold)
	}
}

func TestLoad_FlakeRetriesAndBaseCommitDelta(t *testing.T) {
	tomlStr := `
schema_version = 1

[project]
name = "quarantine-demo"

[queue]
source = "markdown-dir"
root = "docs/plan"

[verify]
flake_retries = 3
base_commit_delta = true
gates = [
  { name = "tests", argv = ["go", "test", "./..."], hard = true }
]
`
	cfg, err := LoadBytes([]byte(tomlStr))
	if err != nil {
		t.Fatalf("LoadBytes failed: %v", err)
	}
	if cfg.Verify.FlakeRetries != 3 {
		t.Errorf("expected FlakeRetries = 3, got %d", cfg.Verify.FlakeRetries)
	}
	if !cfg.Verify.BaseCommitDelta {
		t.Errorf("expected BaseCommitDelta = true, got %v", cfg.Verify.BaseCommitDelta)
	}
}

func TestLoad_GateOnAndSubchunkArgv(t *testing.T) {
	tomlStr := `
schema_version = 1

[project]
name = "group-gates"

[verify]
gates = [
  { name = "tests-related", argv = ["npx", "vitest", "related", "--run", "{touched_files}"], on = "subchunk", hard = true },
  { name = "tests-full", argv = ["npm", "run", "test:unit"], subchunk_argv = ["npx", "vitest", "related", "--run", "{touched_files}"], on = "group_end", hard = true }
]
`
	cfg, err := LoadBytes([]byte(tomlStr))
	if err != nil {
		t.Fatalf("LoadBytes failed: %v", err)
	}
	vGates := cfg.VerifierGates()
	if len(vGates) != 2 {
		t.Fatalf("expected 2 gates, got %d", len(vGates))
	}
	if vGates[0].On != "subchunk" {
		t.Errorf("expected On='subchunk', got %q", vGates[0].On)
	}
	if vGates[1].On != "group_end" || len(vGates[1].SubchunkArgv) == 0 {
		t.Errorf("expected On='group_end' with SubchunkArgv, got %+v", vGates[1])
	}

	badOn := strings.Replace(tomlStr, `on = "subchunk"`, `on = "invalid"`, 1)
	if _, err := LoadBytes([]byte(badOn)); err == nil {
		t.Errorf("expected error for invalid gate 'on' value")
	}
}

