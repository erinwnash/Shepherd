// Package initcmd generates a working shepherd.toml and initial prompt templates by
// autodetecting project build tools, test runners, and available agent CLIs.
package initcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/erinwnash/Shepherd/internal/onboard"
)

// Options configures the init process.
type Options struct {
	Force       bool
	ProjectName string
}

// Run executes shepherd init, generating shepherd.toml and scaffolding .shepherd/prompts/.
func Run(root string, opts Options) (string, error) {
	configPath := filepath.Join(root, "shepherd.toml")
	if _, err := os.Stat(configPath); err == nil && !opts.Force {
		return "", fmt.Errorf("shepherd.toml already exists (use --force to overwrite)")
	}

	projectName := opts.ProjectName
	if projectName == "" {
		projectName = detectProjectName(root)
	}

	// 1. Detect test/build runners
	gatesToml := detectGates(root)

	// 2. Detect agent CLIs on PATH and local opencode config
	tiersToml, poolsToml, repairChain := detectTiersAndPools(root)

	mechTier := "haiku"
	if strings.Contains(tiersToml, "[tiers.flash]") {
		mechTier = "flash"
	}

	// 3. Assemble shepherd.toml
	configContent := fmt.Sprintf(`schema_version = 1

[project]
name = "%s"
repo = "."
base_branch = "main"

[queue]
source = "markdown-dir"
root = "docs/plan"
done_marker = "move-to-done"
done_dir = "docs/planDONE"

[verify]
gates = [
  { name = "content-loss", builtin = "content-loss", hard = true },
  { name = "scope", builtin = "scope", hard = true },
%s
]

[repair]
chain = [%s]
mechanical_fix = { tier = %q, max_errors = 3, codes = ["TS2304", "TS2339"] }

%s

%s

[decide]
backend = "rules"
timeout_ms = 2000

[policy]
max_consecutive_blocks = 3
wait_for_reset_max_min = 45
cloud_first_threshold = 0.5
`, projectName, gatesToml, repairChain, mechTier, tiersToml, poolsToml)

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		return "", fmt.Errorf("write shepherd.toml: %w", err)
	}

	// 4. Scaffold prompt templates and docs/plan/
	if err := scaffoldPrompts(root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(root, "docs", "plan"), 0755); err != nil {
		return "", err
	}

	return fmt.Sprintf("shepherd.toml created for %q with detected gates and tiers.\nScaffolded prompt templates in .shepherd/prompts/", projectName), nil
}

func detectProjectName(root string) string {
	pkgPath := filepath.Join(root, "package.json")
	if data, err := os.ReadFile(pkgPath); err == nil {
		var pkg struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &pkg) == nil && pkg.Name != "" {
			return pkg.Name
		}
	}

	goModPath := filepath.Join(root, "go.mod")
	if data, err := os.ReadFile(goModPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "module ") {
				mod := strings.TrimSpace(strings.TrimPrefix(line, "module "))
				parts := strings.Split(mod, "/")
				return parts[len(parts)-1]
			}
		}
	}

	abs, err := filepath.Abs(root)
	if err == nil {
		return filepath.Base(abs)
	}
	return "project"
}

func detectGates(root string) string {
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		return `  { name = "typecheck", argv = ["go", "vet", "./..."], hard = true },
  { name = "tests", argv = ["go", "test", "{test_files}"], hard = true }`
	}

	if _, err := os.Stat(filepath.Join(root, "package.json")); err == nil {
		return `  { name = "typecheck", argv = ["npx", "tsc", "--noEmit"], hard = true },
  { name = "tests", argv = ["npx", "vitest", "run", "{test_files}"], hard = true },
  { name = "lint", argv = ["npx", "eslint", "{touched_files}"], hard = false }`
	}

	if _, err := os.Stat(filepath.Join(root, "Cargo.toml")); err == nil {
		return `  { name = "typecheck", argv = ["cargo", "check"], hard = true },
  { name = "tests", argv = ["cargo", "test"], hard = true }`
	}

	if _, err := os.Stat(filepath.Join(root, "pubspec.yaml")); err == nil {
		return `  { name = "typecheck", argv = ["flutter", "analyze"], hard = true },
  { name = "tests", argv = ["flutter", "test", "{test_files}"], hard = true }`
	}

	return `  # Add project-specific custom verification gates here:
  # { name = "tests", argv = ["pytest", "{test_files}"], hard = true }`
}

func detectTiersAndPools(root string) (tiersToml, poolsToml, chain string) {
	var tiers []string
	var pools []string
	var chainSteps []string

	agyInfo := onboard.DetectAgyInfo(context.Background())
	hasAgy := agyInfo.Installed
	hasClaude := false
	if _, err := onboard.LookCLIPath("claude"); err == nil {
		hasClaude = true
	}
	hasCodex := false
	if _, err := onboard.LookCLIPath("codex"); err == nil {
		hasCodex = true
	}
	ocInfo := onboard.DetectOpenCodeInfo(root)
	hasOpencode := ocInfo.Installed || ocInfo.ConfigPath != ""

	if hasOpencode {
		localURL := ocInfo.BaseURL
		if localURL == "" {
			localURL = "http://127.0.0.1:8080/v1"
		}
		modelLine := ""
		if ocInfo.DefaultModel != "" {
			modelLine = fmt.Sprintf("\nmodel = %q", ocInfo.DefaultModel)
		}
		tiers = append(tiers, fmt.Sprintf(`[tiers.local]
adapter = "opencode"%s
url = %q
pool = "local"`, modelLine, localURL))
		pools = append(pools, `[quota.pools.local]
mode = "until-rejected"
backoff_min = [10, 30]`)
		chainSteps = append(chainSteps, `"local:3"`)
	}

	if hasAgy {
		activeModel := agyInfo.ActiveModel
		if activeModel == "" {
			activeModel = "gemini-3.8-flash-medium"
		}
		activeLower := strings.ToLower(activeModel)
		isSonnetActive := strings.Contains(activeLower, "claude") || strings.Contains(activeLower, "sonnet")

		proModel := "gemini-4-argon-medium"
		sonnetModel := "claude-sonnet-5-5-medium"
		opusModel := "claude-opus-5-5-medium"
		for _, m := range agyInfo.Models {
			mLow := strings.ToLower(m)
			if (strings.Contains(mLow, "argon") || strings.Contains(mLow, "pro")) && proModel == "gemini-4-argon-medium" {
				proModel = m
			}
			if strings.Contains(mLow, "sonnet") && sonnetModel == "claude-sonnet-5-5-medium" {
				sonnetModel = m
			}
			if strings.Contains(mLow, "opus") && opusModel == "claude-opus-5-5-medium" {
				opusModel = m
			}
		}

		flashModel := activeModel
		if isSonnetActive {
			sonnetModel = activeModel
			flashModel = "gemini-3.8-flash-medium"
		}

		tiers = append(tiers, fmt.Sprintf(`[tiers.flash]
adapter = "agy"
model = %q
pool = "gemini"

[tiers.pro]
adapter = "agy"
model = %q
pool = "gemini"

[tiers.sonnet]
adapter = "agy"
model = %q
pool = "claude"

[tiers.opus]
adapter = "agy"
model = %q
pool = "claude"`, flashModel, proModel, sonnetModel, opusModel))

		pools = append(pools, `[quota.pools.gemini]
mode = "polled"
usage_cmd = ["agy", "-p", "/usage"]
parser = "agy-usage"
floor_weekly_pct = 20

[quota.pools.claude]
mode = "polled"
usage_cmd = ["agy", "-p", "/usage"]
parser = "agy-usage"
floor_weekly_pct = 25`)

		if isSonnetActive {
			chainSteps = append(chainSteps, `"sonnet:2"`, `"opus:1"`)
		} else {
			chainSteps = append(chainSteps, `"flash:2"`, `"pro:1"`)
		}
	}

	if hasClaude {
		tiers = append(tiers, `[tiers.cc]
adapter = "claude-cli"
model = "sonnet"
pool = "cc"

[tiers.haiku]
adapter = "claude-cli"
model = "haiku"
pool = "cc"`)
		pools = append(pools, `[quota.pools.cc]
mode = "polled"
usage_cmd = ["claude", "-p", "/usage"]
parser = "claude-usage"
floor_weekly_pct = 25`)
		if !hasAgy {
			chainSteps = append(chainSteps, `"haiku:1"`, `"cc:2"`)
		}
	}

	if hasCodex {
		tiers = append(tiers, `[tiers.luna]
adapter = "codex"
model = "gpt-5.6-luna"
pool = "codex"

[tiers.terra]
adapter = "codex"
model = "gpt-5.6-terra"
pool = "codex"`)
		pools = append(pools, `[quota.pools.codex]
mode = "until-rejected"
backoff_min = [60, 180, 300]`)
	}

	if len(tiers) == 0 {
		tiers = append(tiers, `[tiers.local]
adapter = "opencode"
url = "http://127.0.0.1:8080/v1"
pool = "local"`)
		pools = append(pools, `[quota.pools.local]
mode = "until-rejected"
backoff_min = [10, 30]`)
		chainSteps = append(chainSteps, `"local:3"`)
	}

	// Determine fallback tier for mechanical syntax fixes
	hasFlash := false
	hasHaiku := false
	for _, t := range tiers {
		if strings.Contains(t, "[tiers.flash]") {
			hasFlash = true
		}
		if strings.Contains(t, "[tiers.haiku]") {
			hasHaiku = true
		}
	}
	if !hasFlash && !hasHaiku {
		tiers = append(tiers, `[tiers.haiku]
adapter = "claude-cli"
model = "haiku"
pool = "cc"`)
	}

	return strings.Join(tiers, "\n\n"), strings.Join(pools, "\n\n"), strings.Join(chainSteps, ", ")
}

func scaffoldPrompts(root string) error {
	promptsDir := filepath.Join(root, ".shepherd", "prompts")
	if err := os.MkdirAll(promptsDir, 0755); err != nil {
		return err
	}

	coderPrompt := `# Terse unattended coding guidelines
- Implement only what the work order requests.
- Make minimal, targeted edits to the declared files. Never rewrite intact files from memory.
- Avoid unnecessary whitespace changes.
- Ensure all tests pass.
`
	repairPrompt := `# Targeted repair guidelines
- Focus strictly on fixing the reported verification failures.
- Do not introduce unrelated modifications.
- Preserve unchanged sections of code.
`

	if err := os.WriteFile(filepath.Join(promptsDir, "coder.md"), []byte(coderPrompt), 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(promptsDir, "repair.md"), []byte(repairPrompt), 0644)
}
