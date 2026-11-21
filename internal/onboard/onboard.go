package onboard

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/slots"
)

// SupportedAgentCLIs defines known coding agent CLIs in order of recommendation preference.
var SupportedAgentCLIs = []string{
	"agy",
	"claude",
	"codex",
	"opencode",
	"aider",
	"gemini",
	"hermes",
	"pi",
}

// OpenCodeInfo describes a detected local OpenCode configuration and provider endpoint.
type OpenCodeInfo struct {
	Installed    bool     `json:"installed"`
	BinaryPath   string   `json:"binary_path,omitempty"`
	ConfigPath   string   `json:"config_path,omitempty"`
	ProviderID   string   `json:"provider_id,omitempty"`
	ProviderName string   `json:"provider_name,omitempty"`
	BaseURL      string   `json:"base_url,omitempty"`
	DefaultModel string   `json:"default_model,omitempty"`
	Models       []string `json:"models,omitempty"`
}

// Summary returns a concise human-readable description of the local OpenCode instance.
func (o OpenCodeInfo) Summary() string {
	if !o.Installed && o.ConfigPath == "" {
		return ""
	}
	url := o.BaseURL
	if url == "" {
		url = "http://127.0.0.1:8080/v1"
	}
	provider := o.ProviderName
	if provider == "" {
		provider = o.ProviderID
	}
	if provider == "" {
		provider = "local"
	}
	if o.DefaultModel != "" {
		return fmt.Sprintf("opencode (%s @ %s, model: %s)", provider, url, o.DefaultModel)
	}
	return fmt.Sprintf("opencode (%s @ %s)", provider, url)
}

// LookCLIPath locates a CLI binary on PATH or in standard user-local installation directories (mise, ~/.local/bin, etc.).
func LookCLIPath(bin string) (string, error) {
	if p, err := exec.LookPath(bin); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("%s not found on PATH", bin)
	}
	candidates := []string{
		filepath.Join(home, ".local", "share", "mise", "installs", bin, "latest", bin),
		filepath.Join(home, ".local", "share", "mise", "installs", bin, "latest", "bin", bin),
		filepath.Join(home, ".local", "share", "mise", "shims", bin),
		filepath.Join(home, ".local", "bin", bin),
		filepath.Join(home, ".opencode", "bin", bin),
		filepath.Join(home, ".gemini", "antigravity-cli", "bin", bin),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH or standard user directories", bin)
}

// DetectOpenCodeInfo inspects PATH and opencode.json files (project-local and ~/.config/opencode/opencode.json).
func DetectOpenCodeInfo(root string) OpenCodeInfo {
	info := OpenCodeInfo{
		BaseURL: "http://127.0.0.1:8080/v1",
	}
	if p, err := LookCLIPath("opencode"); err == nil {
		info.Installed = true
		info.BinaryPath = p
	}

	var configPaths []string
	if root != "" {
		configPaths = append(configPaths,
			filepath.Join(root, "opencode.json"),
			filepath.Join(root, ".opencode", "opencode.json"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		configPaths = append(configPaths,
			filepath.Join(home, ".config", "opencode", "opencode.json"),
		)
	}

	type rawProvider struct {
		Name    string `json:"name"`
		Options struct {
			BaseURL string `json:"baseURL"`
		} `json:"options"`
		Models map[string]struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	type rawConfig struct {
		Model    string                 `json:"model"`
		Provider map[string]rawProvider `json:"provider"`
	}

	baseURLSet := false
	for _, cp := range configPaths {
		data, err := os.ReadFile(cp)
		if err != nil {
			continue
		}
		var rc rawConfig
		if err := json.Unmarshal(data, &rc); err != nil {
			continue
		}
		if info.ConfigPath == "" {
			info.ConfigPath = cp
		}
		if info.DefaultModel == "" && rc.Model != "" {
			info.DefaultModel = rc.Model
		}
		var provIDs []string
		for provID := range rc.Provider {
			provIDs = append(provIDs, provID)
		}
		sort.Strings(provIDs)

		for _, provID := range provIDs {
			prov := rc.Provider[provID]
			if info.ProviderID == "" {
				info.ProviderID = provID
			}
			if info.ProviderName == "" && prov.Name != "" {
				info.ProviderName = prov.Name
			}
			// First configured provider wins (it also supplies the default model); later
			// providers must not override the endpoint or the URL and model would mismatch.
			if prov.Options.BaseURL != "" && !baseURLSet {
				info.BaseURL = prov.Options.BaseURL
				baseURLSet = true
			}
			var mIDs []string
			for mID := range prov.Models {
				mIDs = append(mIDs, mID)
			}
			sort.Strings(mIDs)
			for _, mID := range mIDs {
				fullModel := mID
				if provID != "" && !strings.Contains(mID, "/") {
					fullModel = provID + "/" + mID
				}
				already := false
				for _, existing := range info.Models {
					if existing == fullModel {
						already = true
						break
					}
				}
				if !already {
					info.Models = append(info.Models, fullModel)
				}
			}
		}
	}

	if info.DefaultModel == "" && len(info.Models) > 0 {
		info.DefaultModel = info.Models[0]
	}

	return info
}

// Environment encapsulates detected repository metadata, language toolchains, and host hardware.
type Environment struct {
	ProjectRoot          string
	ProjectName          string
	Language             string
	BuildCommand         []string
	TestCommand          []string
	TypecheckCommand     []string
	LintCommand          []string
	GatesToml            string
	InstalledCLIs        []string
	RecommendedCLI       string
	OpenCode             OpenCodeInfo
	Agy                  AgyInfo
	Providers            []DiscoveredProvider
	AvailableRAMGB       float64
	NumGPUs              int
	RecommendedSlots     int
	GitInitialized       bool
	CurrentBranch        string
	ExistingPlan         bool
	ExistingShepherdTOML bool
}

// Config specifies inputs for the onboarding wizard and launch.md generation.
type Config struct {
	ProjectRoot            string
	ProjectName            string
	Goal                   string
	TargetCLI              string
	OutDir                 string
	DryRun                 bool
	AutoLaunch             bool
	Interactive            bool
	Providers              []DiscoveredProvider
	CloudFirstThreshold    float64
	MaxChunkCostUSD        float64
	MaxSprintCostUSD       float64
	RequireHighReviewHuman bool
}

// Result describes the outcome of scaffolding.
type Result struct {
	Environment      *Environment
	FilesCreated     []string
	LaunchMDPath     string
	ShepherdTOMLPath string
	LaunchCommand    string
	LaunchArgs       []string
	LaunchStdin      string
	TargetCLI        string
}

// DetectEnvironment inspects the given project directory for language stacks,
// installed agent CLIs, compliant AI routers, git status, and hardware constraints.
func DetectEnvironment(root string) *Environment {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}

	env := &Environment{
		ProjectRoot: absRoot,
		ProjectName: detectProjectName(absRoot),
	}

	// 1. Language and build/test gates
	detectStack(env)

	// 2. Installed agent CLIs, local OpenCode instance, Antigravity CLI info, and compliant AI routers
	env.OpenCode = DetectOpenCodeInfo(absRoot)
	env.Agy = DetectAgyInfo(context.Background())
	env.Providers = DiscoverProviders(context.Background(), absRoot)
	for _, cli := range SupportedAgentCLIs {
		lookupName := cli
		if _, err := LookCLIPath(lookupName); err == nil {
			env.InstalledCLIs = append(env.InstalledCLIs, cli)
		} else if cli == "opencode" && (env.OpenCode.Installed || env.OpenCode.ConfigPath != "") {
			env.InstalledCLIs = append(env.InstalledCLIs, "opencode")
		}
	}
	if len(env.InstalledCLIs) > 0 {
		env.RecommendedCLI = env.InstalledCLIs[0]
	} else {
		env.RecommendedCLI = "agy" // fallback default
	}

	// 3. Hardware & Slot safeguards
	env.NumGPUs = slots.DetectNumGPUs()
	ram, err := slots.GetAvailableRAMGB()
	if err == nil {
		env.AvailableRAMGB = ram
	}

	// Suggest slot concurrency bounded by hardware
	if env.NumGPUs > 0 {
		env.RecommendedSlots = env.NumGPUs
		if env.RecommendedSlots > 4 {
			env.RecommendedSlots = 4
		}
	} else {
		cpus := runtime.NumCPU()
		env.RecommendedSlots = cpus / 2
		if env.RecommendedSlots < 1 {
			env.RecommendedSlots = 1
		} else if env.RecommendedSlots > 3 {
			env.RecommendedSlots = 3
		}
	}

	// 4. Git status
	if _, err := os.Stat(filepath.Join(absRoot, ".git")); err == nil {
		env.GitInitialized = true
		cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
		cmd.Dir = absRoot
		if out, err := cmd.Output(); err == nil {
			env.CurrentBranch = strings.TrimSpace(string(out))
		}
	}
	if env.CurrentBranch == "" {
		env.CurrentBranch = "main"
	}

	// 5. Existing files
	if _, err := os.Stat(filepath.Join(absRoot, "shepherd.toml")); err == nil {
		env.ExistingShepherdTOML = true
	}
	if _, err := os.Stat(filepath.Join(absRoot, "docs", "plan")); err == nil {
		env.ExistingPlan = true
	}

	return env
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

	cargoPath := filepath.Join(root, "Cargo.toml")
	if data, err := os.ReadFile(cargoPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "name =") {
				parts := strings.SplitN(trimmed, "=", 2)
				if len(parts) == 2 {
					name := strings.Trim(strings.TrimSpace(parts[1]), "\"")
					if name != "" {
						return name
					}
				}
			}
		}
	}

	pyprojectPath := filepath.Join(root, "pyproject.toml")
	if data, err := os.ReadFile(pyprojectPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "name =") {
				parts := strings.SplitN(trimmed, "=", 2)
				if len(parts) == 2 {
					name := strings.Trim(strings.TrimSpace(parts[1]), "\"")
					if name != "" {
						return name
					}
				}
			}
		}
	}

	pubspecPath := filepath.Join(root, "pubspec.yaml")
	if data, err := os.ReadFile(pubspecPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "name:") {
				name := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "name:")), "\"'")
				if name != "" {
					return name
				}
			}
		}
	}

	return filepath.Base(root)
}

func detectStack(env *Environment) {
	root := env.ProjectRoot

	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		env.Language = "Go"
		env.TypecheckCommand = []string{"go", "vet", "./..."}
		env.TestCommand = []string{"go", "test", "{test_files}"}
		env.GatesToml = `  { name = "typecheck", argv = ["go", "vet", "./..."], hard = true },
  { name = "tests", argv = ["go", "test", "{test_files}"], hard = true }`
		return
	}

	if _, err := os.Stat(filepath.Join(root, "package.json")); err == nil {
		env.Language = "TypeScript/Node"
		env.TypecheckCommand = []string{"npx", "tsc", "--noEmit"}
		env.TestCommand = []string{"npm", "test"}
		env.LintCommand = []string{"npx", "eslint", "{touched_files}"}
		env.GatesToml = `  { name = "typecheck", argv = ["npx", "tsc", "--noEmit"], hard = true },
  { name = "tests", argv = ["npm", "test"], hard = true },
  { name = "lint", argv = ["npx", "eslint", "{touched_files}"], hard = false }`
		return
	}

	if _, err := os.Stat(filepath.Join(root, "Cargo.toml")); err == nil {
		env.Language = "Rust"
		env.TypecheckCommand = []string{"cargo", "check"}
		env.TestCommand = []string{"cargo", "test"}
		env.GatesToml = `  { name = "typecheck", argv = ["cargo", "check"], hard = true },
  { name = "tests", argv = ["cargo", "test"], hard = true }`
		return
	}

	if _, err := os.Stat(filepath.Join(root, "pubspec.yaml")); err == nil {
		env.Language = "Flutter/Dart"
		env.TypecheckCommand = []string{"flutter", "analyze"}
		env.TestCommand = []string{"flutter", "test", "{test_files}"}
		env.GatesToml = `  { name = "typecheck", argv = ["flutter", "analyze"], hard = true },
  { name = "tests", argv = ["flutter", "test", "{test_files}"], hard = true }`
		return
	}

	if _, err := os.Stat(filepath.Join(root, "pyproject.toml")); err == nil ||
		fileExists(filepath.Join(root, "requirements.txt")) ||
		fileExists(filepath.Join(root, "setup.py")) {
		env.Language = "Python"
		env.TestCommand = []string{"pytest", "{test_files}"}
		env.GatesToml = `  { name = "tests", argv = ["pytest", "{test_files}"], hard = true }`
		return
	}

	env.Language = "Generic"
	env.GatesToml = `  # Define project-specific verification gates here:
  # { name = "tests", argv = ["test-runner", "{test_files}"], hard = true }`
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// GenerateLaunchMD crafts a comprehensive onboarding directive prompt for the chosen agent CLI.
func GenerateLaunchMD(cfg Config, env *Environment) string {
	goal := cfg.Goal
	if goal == "" {
		goal = "Analyze repository architecture, record Architecture Decision Records (ADRs), create epic and sprint plans, and generate initial atomic chunk work orders."
	}

	cli := cfg.TargetCLI
	if cli == "" {
		cli = env.RecommendedCLI
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# Shepherd AI Onboarding Blueprint: %s\n\n", cfg.ProjectName)
	sb.WriteString("## Mission Directive\n")
	fmt.Fprintf(&sb, "You are acting as the Chief Systems Architect and Principal Engineer onboarding **%s** into the Shepherd AI Autonomous Coding Orchestration System.\n\n", cfg.ProjectName)
	sb.WriteString("Your objective is to:\n")
	sb.WriteString("1. Thoroughly inspect the existing repository code, conventions, structure, and dependencies.\n")
	sb.WriteString("2. Establish foundational Architectural Decision Records (ADRs) under `docs/adr/`.\n")
	sb.WriteString("3. Establish the overarching Epic roadmap under `docs/plan/epic.md` and Sprint 1 scope under `docs/plan/S01/sprint.md`.\n")
	sb.WriteString("4. Decompose the project goal into small, atomic, independently verifiable chunk work orders under `docs/plan/S01/`.\n")
	sb.WriteString("5. Run `shepherd plan lint` and ensure all created work orders pass validation with **ZERO errors**.\n\n")
	sb.WriteString("---\n\n")

	sb.WriteString("## Project Context\n")
	fmt.Fprintf(&sb, "- **Project Name:** %s\n", cfg.ProjectName)
	fmt.Fprintf(&sb, "- **Primary Language / Stack:** %s\n", env.Language)
	fmt.Fprintf(&sb, "- **Base Branch:** %s\n", env.CurrentBranch)
	fmt.Fprintf(&sb, "- **Onboarding Target CLI:** %s\n", cli)
	if len(env.InstalledCLIs) > 0 {
		fmt.Fprintf(&sb, "- **Detected Agent Stack:** %s\n", strings.Join(env.InstalledCLIs, ", "))
	}
	if ocSummary := env.OpenCode.Summary(); ocSummary != "" {
		fmt.Fprintf(&sb, "- **Local OpenCode Instance:** %s\n", ocSummary)
	}
	sb.WriteString("- **High-Level Objective:**\n")
	fmt.Fprintf(&sb, "> %s\n\n", goal)
	sb.WriteString("---\n\n")

	sb.WriteString("## Shepherd Work Order Specification\n\n")
	sb.WriteString("Shepherd coordinates autonomous coding agents across isolated git worktrees using Markdown work order files.\n")
	sb.WriteString("Each chunk file under `docs/plan/S01/` must follow the format `01A_<name>.md`, `01B_<name>.md`, etc.\n\n")

	sb.WriteString("### Strict YAML Frontmatter Schema\n")
	sb.WriteString("Every chunk work order MUST start with YAML frontmatter bounded by `---`:\n\n")
	sb.WriteString("```yaml\n")
	sb.WriteString("---\n")
	sb.WriteString("id: 01A_chunk_name               # Alphanumeric with '.' '_' '-', starting with letter/digit\n")
	sb.WriteString("title: \"Human Readable Title\"    # Clear description of the chunk's purpose\n")
	sb.WriteString("epic: \"Core\"                     # Epic identifier\n")
	sb.WriteString("sprint: \"S01\"                    # Sprint folder identifier (matches folder name)\n")
	sb.WriteString("depends_on: []                   # List of prerequisite chunk IDs (no circular deps!)\n")
	sb.WriteString("review: \"MEDIUM\"                 # Review level: LOW, MEDIUM, or HIGH\n")
	sb.WriteString("est_effort: \"M\"                  # Estimated effort: S, M, or L\n")
	sb.WriteString("files:                           # ALL files this chunk may create or modify\n")
	sb.WriteString("  - \"new:path/to/new_file.ext\"   # CRITICAL: files created anew MUST have the 'new:' prefix!\n")
	sb.WriteString("  - \"path/to/existing_file.ext\"  # Existing files omit the prefix\n")
	sb.WriteString("tests:                           # Verification test paths to run\n")
	sb.WriteString("  - \"path/to/test_file.ext\"      # If new test file, also list in files with new:\n")
	sb.WriteString("# no_tests_reason: \"reason\"      # Required ONLY if tests array is empty\n")
	sb.WriteString("adr:                             # Referenced ADRs (must exist in docs/adr/)\n")
	sb.WriteString("  - \"ADR-0001\"\n")
	sb.WriteString("status: pending                  # Always start as pending\n")
	sb.WriteString("---\n")
	sb.WriteString("```\n\n")

	sb.WriteString("### Required Markdown Body Sections\n")
	sb.WriteString("Immediately following the YAML frontmatter, the chunk body MUST contain these specific `## ` headings:\n\n")
	sb.WriteString("1. `## Goal`: A crisp, 1-3 sentence summary of what this chunk achieves.\n")
	sb.WriteString("2. `## Out of scope`: Explicit constraints on what this chunk must NOT touch or implement.\n")
	sb.WriteString("3. `## Acceptance`: Concrete, testable conditions indicating completion (e.g., specific test cases passing, zero regressions, contracts satisfied).\n\n")

	sb.WriteString("### Chunk Sizing Rules (STRIDE & Modularity Guardrails)\n")
	sb.WriteString("- **Maximum 5 files per chunk:** Never declare more than 5 files in `files:`.\n")
	sb.WriteString("- **Atomic & Reversible:** Each chunk should do one cohesive task.\n")
	sb.WriteString("- **DAG Acyclicity:** Work orders must form a Directed Acyclic Graph (DAG). Sprint dependencies cannot point forward.\n")
	sb.WriteString("- **Test-First or Coupled:** Every code modification must either be accompanied by tests or have a legitimate `no_tests_reason`.\n\n")
	sb.WriteString("---\n\n")

	sb.WriteString("## Action Plan for the Agent CLI\n\n")
	sb.WriteString("Execute the following steps in sequence:\n\n")
	sb.WriteString("### Step 1: Codebase Survey\n")
	sb.WriteString("- Read directory layout, root configuration files, and package manifests.\n")
	sb.WriteString("- Understand the existing testing frameworks and compilation targets.\n\n")

	sb.WriteString("### Step 2: Establish Architecture Decision Records (ADRs)\n")
	sb.WriteString("- Examine `docs/adr/0001-record-architecture-decisions.md`.\n")
	sb.WriteString("- Create subsequent numbered ADRs for major choices, e.g. `docs/adr/0002-data-storage.md` or `docs/adr/0002-api-contracts.md`.\n")
	sb.WriteString("- Numbering convention: `NNNN-kebab-case-title.md` (4-digit padded number).\n\n")

	sb.WriteString("### Step 3: Define Epic & Sprint Roadmap\n")
	sb.WriteString("- In `docs/plan/epic.md`, document the strategic goals, architectural pillars, and milestones.\n")
	sb.WriteString("- In `docs/plan/S01/sprint.md`, define the Sprint 1 deliverables, prerequisites, and chunk roadmap.\n\n")

	sb.WriteString("### Step 4: Author Atomic Chunk Work Orders\n")
	sb.WriteString("- Scaffold chunk work orders: `docs/plan/S01/01A_<name>.md`, `01B_<name>.md`, etc.\n")
	sb.WriteString("- Ensure all created files use `new:path` under `files:`.\n")
	sb.WriteString("- Ensure all referenced ADRs exist under `docs/adr/`.\n\n")

	sb.WriteString("### Step 5: Verification with Shepherd Linter\n")
	sb.WriteString("- Run:\n")
	sb.WriteString("  ```bash\n")
	sb.WriteString("  shepherd plan lint\n")
	sb.WriteString("  ```\n")
	sb.WriteString("- If any findings (errors or warnings) are reported, update the chunk files until `shepherd plan lint` passes cleanly with code 0!\n")

	return sb.String()
}

// GenerateShepherdTOML generates a fully configured shepherd.toml matching the project environment.
func GenerateShepherdTOML(cfg Config, env *Environment) (string, error) {
	name := cfg.ProjectName
	if name == "" {
		name = env.ProjectName
	}
	targetCLI := cfg.TargetCLI
	if targetCLI == "" {
		targetCLI = env.RecommendedCLI
	}

	maxSlots := env.RecommendedSlots
	if maxSlots <= 0 {
		maxSlots = 2
	}
	maxGPUs := env.NumGPUs
	if maxGPUs <= 0 {
		maxGPUs = 1
	}

	// Build active CLI set from targetCLI + all detected InstalledCLIs
	hasCLI := make(map[string]bool)
	if targetCLI != "" {
		hasCLI[strings.ToLower(targetCLI)] = true
	}
	for _, c := range env.InstalledCLIs {
		hasCLI[strings.ToLower(c)] = true
	}
	if env.OpenCode.Installed || env.OpenCode.ConfigPath != "" {
		hasCLI["opencode"] = true
	}

	var tiers []string
	var pools []string
	var chain []string

	if len(cfg.Providers) > 0 {
		sorted := make([]DiscoveredProvider, len(cfg.Providers))
		copy(sorted, cfg.Providers)
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[i].Priority < sorted[j].Priority
		})

		seenPools := make(map[string]bool)
		for idx, p := range sorted {
			p.Priority = idx + 1
			attempts := p.Attempts
			if attempts <= 0 {
				attempts = 2
			}
			capPct := p.UsageCapPct
			if capPct <= 0 {
				capPct = 100
			}
			poolName := p.Pool
			if poolName == "" {
				poolName = p.ID
			}
			var tLines []string
			tLines = append(tLines, fmt.Sprintf("[tiers.%s]", p.ID))
			tLines = append(tLines, fmt.Sprintf("adapter = %q", p.Adapter))
			if p.DefaultModel != "" {
				tLines = append(tLines, fmt.Sprintf("model = %q", p.DefaultModel))
			}
			if p.Endpoint != "" {
				tLines = append(tLines, fmt.Sprintf("url = %q", p.Endpoint))
			}
			tLines = append(tLines, fmt.Sprintf("pool = %q", poolName))
			tLines = append(tLines, fmt.Sprintf("enabled = %v", p.Enabled))
			tLines = append(tLines, fmt.Sprintf("priority = %d", p.Priority))
			tLines = append(tLines, fmt.Sprintf("usage_cap_pct = %d", capPct))
			tiers = append(tiers, strings.Join(tLines, "\n"))

			if p.Enabled {
				chain = append(chain, fmt.Sprintf("%q", fmt.Sprintf("%s:%d", p.ID, attempts)))
			}

			if !seenPools[poolName] {
				seenPools[poolName] = true
				if p.PoolMode == "polled" {
					usageBin := "agy"
					parser := "agy-usage"
					if p.Adapter == "claude-cli" {
						usageBin = "claude"
						parser = "claude-usage"
					}
					floor := p.FloorPct
					if floor <= 0 {
						floor = 25
					}
					pools = append(pools, fmt.Sprintf(`[quota.pools.%s]
mode = "polled"
usage_cmd = [%q, "-p", "/usage"]
parser = %q
floor_weekly_pct = %d
usage_cap_pct = %d`, poolName, usageBin, parser, floor, capPct))
				} else {
					pools = append(pools, fmt.Sprintf(`[quota.pools.%s]
mode = "until-rejected"
backoff_min = [10, 30]
usage_cap_pct = %d`, poolName, capPct))
				}
			}
		}
		if len(chain) == 0 && len(sorted) > 0 {
			chain = append(chain, fmt.Sprintf("%q", fmt.Sprintf("%s:2", sorted[0].ID)))
		}
	} else {
		seenPools := make(map[string]bool)
		// 1. Local OpenCode tier first in ladder if installed/configured (per brief §2: local model first)
		if hasCLI["opencode"] || hasCLI["openai-compat"] {
			localURL := env.OpenCode.BaseURL
			if localURL == "" {
				localURL = "http://127.0.0.1:8080/v1"
			}
			modelLine := ""
			if env.OpenCode.DefaultModel != "" {
				modelLine = fmt.Sprintf("\nmodel = %q", env.OpenCode.DefaultModel)
			}
			tiers = append(tiers, fmt.Sprintf(`[tiers.local]
adapter = "opencode"%s
url = %q
pool = "local"`, modelLine, localURL))
			if !seenPools["local"] {
				seenPools["local"] = true
				pools = append(pools, `[quota.pools.local]
mode = "until-rejected"
backoff_min = [10, 30]`)
			}
			chain = append(chain, `"local:3"`)
		}

		if hasCLI["agy"] {
			activeModel := env.Agy.ActiveModel
			if activeModel == "" {
				activeModel = "gemini-3.8-flash-medium"
			}
			activeLower := strings.ToLower(activeModel)
			isSonnetActive := strings.Contains(activeLower, "claude") || strings.Contains(activeLower, "sonnet")

			proModel := "gemini-4-argon-medium"
			sonnetModel := "claude-sonnet-5-5-medium"
			opusModel := "claude-opus-5-5-medium"
			for _, m := range env.Agy.Models {
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

			if !seenPools["gemini"] {
				seenPools["gemini"] = true
				pools = append(pools, `[quota.pools.gemini]
mode = "polled"
usage_cmd = ["agy", "-p", "/usage"]
parser = "agy-usage"
floor_weekly_pct = 20`)
			}
			if !seenPools["claude"] {
				seenPools["claude"] = true
				pools = append(pools, `[quota.pools.claude]
mode = "polled"
usage_cmd = ["agy", "-p", "/usage"]
parser = "agy-usage"
floor_weekly_pct = 25`)
			}

			if isSonnetActive {
				chain = append(chain, `"sonnet:2"`, `"opus:1"`)
			} else {
				chain = append(chain, `"flash:2"`, `"pro:1"`)
			}
		}

		if hasCLI["claude"] || hasCLI["claude-cli"] {
			tiers = append(tiers, `[tiers.cc]
adapter = "claude-cli"
model = "sonnet"
pool = "cc"

[tiers.haiku]
adapter = "claude-cli"
model = "haiku"
pool = "cc"`)
			if !seenPools["cc"] {
				seenPools["cc"] = true
				pools = append(pools, `[quota.pools.cc]
mode = "polled"
usage_cmd = ["claude", "-p", "/usage"]
parser = "claude-usage"
floor_weekly_pct = 25`)
			}
			if !hasCLI["agy"] {
				chain = append(chain, `"haiku:1"`, `"cc:2"`)
			}
		}

		if hasCLI["codex"] {
			tiers = append(tiers, `[tiers.luna]
adapter = "codex"
model = "gpt-5.6-luna"
pool = "codex"

[tiers.terra]
adapter = "codex"
model = "gpt-5.6-terra"
pool = "codex"`)
			if !seenPools["codex"] {
				seenPools["codex"] = true
				pools = append(pools, `[quota.pools.codex]
mode = "until-rejected"
backoff_min = [60, 180, 300]`)
			}
			if len(chain) == 0 || targetCLI == "codex" {
				chain = append(chain, `"luna:2"`, `"terra:1"`)
			}
		}

		if hasCLI["aider"] {
			tiers = append(tiers, `[tiers.aider]
adapter = "aider"
model = "sonnet"
pool = "aider"`)
			if !seenPools["aider"] {
				seenPools["aider"] = true
				pools = append(pools, `[quota.pools.aider]
mode = "until-rejected"
backoff_min = [30, 60]`)
			}
			if len(chain) == 0 || targetCLI == "aider" {
				chain = append(chain, `"aider:2"`)
			}
		}

		if hasCLI["gemini"] || hasCLI["gemini-cli"] {
			poolName := "gemini"
			if seenPools["gemini"] {
				poolName = "gemini-cli"
			}
			tiers = append(tiers, fmt.Sprintf(`[tiers.gemini]
adapter = "gemini-cli"
model = "gemini-2.5-pro"
pool = %q`, poolName))
			if !seenPools[poolName] {
				seenPools[poolName] = true
				pools = append(pools, fmt.Sprintf(`[quota.pools.%s]
mode = "until-rejected"
backoff_min = [30, 60]`, poolName))
			}
			if len(chain) == 0 || targetCLI == "gemini" {
				chain = append(chain, `"gemini:2"`)
			}
		}
	}

	// Determine tier for mechanical syntax fixes
	mechTier := "haiku"
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
	if hasFlash && !hasHaiku {
		mechTier = "flash"
	} else if !hasHaiku {
		tiers = append(tiers, `[tiers.haiku]
adapter = "claude-cli"
model = "haiku"
pool = "cc"`)
	}

	cloudFirst := cfg.CloudFirstThreshold
	if cloudFirst <= 0 {
		cloudFirst = 0.5
	}
	var extraPolicy string
	if cfg.MaxChunkCostUSD > 0 {
		extraPolicy += fmt.Sprintf("\nmax_chunk_cost_usd = %.2f", cfg.MaxChunkCostUSD)
	}
	if cfg.MaxSprintCostUSD > 0 {
		extraPolicy += fmt.Sprintf("\nmax_sprint_cost_usd = %.2f", cfg.MaxSprintCostUSD)
	}
	if cfg.RequireHighReviewHuman {
		extraPolicy += "\nrequire_high_review_human = true"
	}

	tomlContent := fmt.Sprintf(`schema_version = 1

[project]
name = "%s"
repo = "."
base_branch = "%s"

[queue]
source = "markdown-dir"
root = "docs/plan"
done_marker = "move-to-done"
done_dir = "docs/planDONE"

[slots]
max_slots = %d
min_free_ram_gb = 4.0
max_gpu_workers = %d
desktop_priority = true

[verify]
gates = [
  { name = "content-loss", builtin = "content-loss", hard = true },
  { name = "scope", builtin = "scope", hard = true },
%s
]
flake_retries = 2
base_commit_delta = true

[repair]
chain = [%s]
mechanical_fix = { tier = %q, max_errors = 3, codes = ["TS2304", "TS2339", "undeclared-name"] }

%s

%s

[decide]
backend = "rules"
timeout_ms = 2000

[policy]
max_consecutive_blocks = 3
wait_for_reset_max_min = 45
cloud_first_threshold = %.2f%s
`, name, env.CurrentBranch, maxSlots, maxGPUs, env.GatesToml, strings.Join(chain, ", "), mechTier, strings.Join(tiers, "\n\n"), strings.Join(pools, "\n\n"), cloudFirst, extraPolicy)

	// Validate against strict configuration parser
	if _, err := config.LoadBytes([]byte(tomlContent)); err != nil {
		return "", fmt.Errorf("generated invalid shepherd.toml: %w", err)
	}

	return tomlContent, nil
}

// Scaffold orchestrates the creation of shepherd.toml, launch.md, docs/plan/S01, and docs/adr.
func Scaffold(cfg Config) (*Result, error) {
	if cfg.ProjectRoot == "" {
		cfg.ProjectRoot = "."
	}
	absRoot, err := filepath.Abs(cfg.ProjectRoot)
	if err != nil {
		absRoot = cfg.ProjectRoot
	}
	cfg.ProjectRoot = absRoot

	outDir := cfg.OutDir
	if outDir == "" {
		outDir = cfg.ProjectRoot
	} else {
		absOut, err := filepath.Abs(outDir)
		if err == nil {
			outDir = absOut
		}
	}

	env := DetectEnvironment(cfg.ProjectRoot)
	if cfg.ProjectName == "" {
		cfg.ProjectName = env.ProjectName
	}
	if cfg.TargetCLI == "" {
		cfg.TargetCLI = env.RecommendedCLI
	}

	res := &Result{
		Environment: env,
		TargetCLI:   cfg.TargetCLI,
	}

	// 1. Generate shepherd.toml
	shepherdTOMLPath := filepath.Join(outDir, "shepherd.toml")
	res.ShepherdTOMLPath = shepherdTOMLPath
	tomlContent, err := GenerateShepherdTOML(cfg, env)
	if err != nil {
		return nil, err
	}

	// 2. Generate launch.md
	launchMDPath := filepath.Join(outDir, "launch.md")
	res.LaunchMDPath = launchMDPath
	launchContent := GenerateLaunchMD(cfg, env)

	// 3. Build launch command
	cmdName, args, stdinStr, err := BuildLaunchCommand(cfg.TargetCLI, launchMDPath)
	if err != nil {
		return nil, err
	}
	res.LaunchCommand = cmdName
	res.LaunchArgs = args
	res.LaunchStdin = stdinStr

	if cfg.DryRun {
		res.FilesCreated = []string{
			shepherdTOMLPath,
			launchMDPath,
			filepath.Join(outDir, "docs/plan/epic.md"),
			filepath.Join(outDir, "docs/plan/S01/sprint.md"),
			filepath.Join(outDir, "docs/adr/0001-record-architecture-decisions.md"),
			filepath.Join(outDir, ".shepherd/prompts/coder.md"),
			filepath.Join(outDir, ".shepherd/prompts/repair.md"),
		}
		return res, nil
	}

	// Create directories
	dirs := []string{
		filepath.Join(outDir, "docs", "plan", "S01"),
		filepath.Join(outDir, "docs", "adr"),
		filepath.Join(outDir, ".shepherd", "prompts"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", d, err)
		}
	}

	// Write shepherd.toml
	if err := os.WriteFile(shepherdTOMLPath, []byte(tomlContent), 0644); err != nil {
		return nil, fmt.Errorf("write shepherd.toml: %w", err)
	}
	res.FilesCreated = append(res.FilesCreated, shepherdTOMLPath)

	// Write launch.md
	if err := os.WriteFile(launchMDPath, []byte(launchContent), 0644); err != nil {
		return nil, fmt.Errorf("write launch.md: %w", err)
	}
	res.FilesCreated = append(res.FilesCreated, launchMDPath)

	// Write docs/plan/epic.md
	epicPath := filepath.Join(outDir, "docs", "plan", "epic.md")
	epicContent := fmt.Sprintf(`# Epic: %s Core Architecture & Implementation

## Vision
%s

## Strategic Goals
1. Establish modular, clean architecture with explicit boundary contracts.
2. Ensure rigorous test coverage and automated verification gates.
3. Deliver atomic, reviewable work orders coordinated by Shepherd AI.

## Roadmap & Sprints
- **S01:** Initial foundation, core interfaces, domain entities, and baseline tests.
`, cfg.ProjectName, cfg.Goal)
	if err := os.WriteFile(epicPath, []byte(epicContent), 0644); err != nil {
		return nil, err
	}
	res.FilesCreated = append(res.FilesCreated, epicPath)

	// Write docs/plan/S01/sprint.md
	sprintPath := filepath.Join(outDir, "docs", "plan", "S01", "sprint.md")
	sprintContent := fmt.Sprintf(`# Sprint S01: Bootstrap & Foundation

## Objective
%s

## Chunk Inventory
- [ ] `+"`01A_foundation`"+`: Baseline types and configuration.
- [ ] `+"`01B_core_logic`"+`: Core domain behaviors and verification test suite.

## Acceptance Criteria
- All tests pass with zero regressions.
- `+"`shepherd plan lint`"+` passes with 0 errors.
`, cfg.Goal)
	if err := os.WriteFile(sprintPath, []byte(sprintContent), 0644); err != nil {
		return nil, err
	}
	res.FilesCreated = append(res.FilesCreated, sprintPath)

	// Write docs/adr/0001-record-architecture-decisions.md
	adrPath := filepath.Join(outDir, "docs", "adr", "0001-record-architecture-decisions.md")
	adrContent := `# 1. Record Architecture Decisions

Date: 2026-10-17

## Status
Accepted

## Context
We need to record architectural, structural, and behavioral decisions made during the design and implementation of this system so that autonomous AI agents and human contributors share full context.

## Decision
We will use Architecture Decision Records (ADRs) as documented by Michael Nygard. ADRs will be stored under ` + "`docs/adr/NNNN-title.md`" + ` and referenced in chunk work orders under the ` + "`adr:`" + ` frontmatter field.

## Consequences
- Every major structural decision has an immutable historical record.
- Autonomous agent work orders can cite specific ADRs for architectural invariants.
`
	if err := os.WriteFile(adrPath, []byte(adrContent), 0644); err != nil {
		return nil, err
	}
	res.FilesCreated = append(res.FilesCreated, adrPath)

	// Write .shepherd/prompts
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
	coderPath := filepath.Join(outDir, ".shepherd", "prompts", "coder.md")
	repairPath := filepath.Join(outDir, ".shepherd", "prompts", "repair.md")
	if err := os.WriteFile(coderPath, []byte(coderPrompt), 0644); err != nil {
		return nil, err
	}
	res.FilesCreated = append(res.FilesCreated, coderPath)
	if err := os.WriteFile(repairPath, []byte(repairPrompt), 0644); err != nil {
		return nil, err
	}
	res.FilesCreated = append(res.FilesCreated, repairPath)

	return res, nil
}

// BuildLaunchCommand determines the executable and arguments to launch the target CLI.
func BuildLaunchCommand(targetCLI, launchMDPath string) (string, []string, string, error) {
	prompt := fmt.Sprintf("Please read %s and execute the onboarding instructions to survey the codebase, record ADRs, and create chunk work orders.", filepath.Base(launchMDPath))

	switch strings.ToLower(targetCLI) {
	case "agy":
		return "agy", []string{"-p", prompt, "--dangerously-skip-permissions"}, "", nil
	case "claude", "claude-cli":
		return "claude", []string{"-p", prompt, "--dangerously-skip-permissions"}, "", nil
	case "codex":
		return "codex", []string{"exec", "-c", `model_reasoning_effort="medium"`, "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "-"}, prompt, nil
	case "opencode", "openai-compat":
		return "opencode", []string{"run", "--auto", prompt}, "", nil
	case "aider":
		return "aider", []string{"--yes", "--no-auto-commits", "--message", prompt}, "", nil
	case "gemini", "gemini-cli":
		return "gemini", []string{"-p", prompt}, "", nil
	case "hermes", "hermes-agent":
		return "hermes", []string{"chat", "--oneshot", "-q", prompt}, "", nil
	case "pi", "pi-coding-agent":
		return "pi", []string{"-p", prompt}, "", nil
	default:
		return "", nil, "", fmt.Errorf("unsupported agent CLI %q; supported: %s", targetCLI, strings.Join(SupportedAgentCLIs, ", "))
	}
}

// LaunchCLI spawns the requested agent CLI in the project directory, streaming standard I/O.
func LaunchCLI(ctx context.Context, cfg Config, res *Result, stdin io.Reader, stdout, stderr io.Writer) error {
	cmdName := res.LaunchCommand
	args := res.LaunchArgs

	cmd := exec.CommandContext(ctx, cmdName, args...)
	cmd.Dir = cfg.ProjectRoot

	if res.LaunchStdin != "" {
		cmd.Stdin = strings.NewReader(res.LaunchStdin)
	} else if stdin != nil {
		cmd.Stdin = stdin
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	return cmd.Run()
}

// RunInteractive prompts the user interactively in the terminal for configuration settings.
func RunInteractive(r io.Reader, w io.Writer, env *Environment, defaultCfg Config) (Config, error) {
	scanner := bufio.NewScanner(r)
	cfg := defaultCfg

	fmt.Fprintf(w, "Shepherd Onboarding Wizard\n")
	fmt.Fprintf(w, "--------------------------\n")
	fmt.Fprintf(w, "Detected Stack: %s\n", env.Language)
	fmt.Fprintf(w, "Available CLIs: %s\n", strings.Join(env.InstalledCLIs, ", "))
	if ocSummary := env.OpenCode.Summary(); ocSummary != "" {
		fmt.Fprintf(w, "Local OpenCode: %s\n", ocSummary)
	}
	fmt.Fprintf(w, "Hardware:       %.1f GB Free RAM | %d GPUs -> Recommending %d slots\n\n", env.AvailableRAMGB, env.NumGPUs, env.RecommendedSlots)

	// 1. Project Name
	defaultName := cfg.ProjectName
	if defaultName == "" {
		defaultName = env.ProjectName
	}
	fmt.Fprintf(w, "Project Name [%s]: ", defaultName)
	if scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text != "" {
			cfg.ProjectName = text
		} else {
			cfg.ProjectName = defaultName
		}
	}

	// 2. Goal
	defaultGoal := cfg.Goal
	if defaultGoal == "" {
		defaultGoal = "Onboard architecture, ADRs, and Sprint 1 chunk plan"
	}
	fmt.Fprintf(w, "Sprint 1 / Project Goal [%s]: ", defaultGoal)
	if scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text != "" {
			cfg.Goal = text
		} else {
			cfg.Goal = defaultGoal
		}
	}

	// 3. Target CLI
	defaultCLI := cfg.TargetCLI
	if defaultCLI == "" {
		defaultCLI = env.RecommendedCLI
	}
	fmt.Fprintf(w, "Agent CLI (%s) [%s]: ", strings.Join(SupportedAgentCLIs, "/"), defaultCLI)
	if scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text != "" {
			cfg.TargetCLI = text
		} else {
			cfg.TargetCLI = defaultCLI
		}
	}

	// 4. Launch immediately?
	fmt.Fprintf(w, "Immediately launch %s with launch.md? [y/N]: ", cfg.TargetCLI)
	if scanner.Scan() {
		text := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if text == "y" || text == "yes" {
			cfg.AutoLaunch = true
		}
	}

	return cfg, nil
}
