// Package doctor implements system and project preflight checks with actionable remediation advice.
package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/onboard"
)

// CheckResult records the outcome of one preflight diagnostic check.
type CheckResult struct {
	Name   string
	Passed bool
	Detail string
	Fix    string
}

// Check evaluates project configuration, git status, installed agent CLIs, and sandbox tools.
func Check(root string) ([]CheckResult, bool) {
	var results []CheckResult
	allOk := true

	add := func(name string, passed bool, detail, fix string) {
		if !passed {
			allOk = false
		}
		results = append(results, CheckResult{
			Name:   name,
			Passed: passed,
			Detail: detail,
			Fix:    fix,
		})
	}

	// 1. shepherd.toml check
	configPath := filepath.Join(root, "shepherd.toml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		add("shepherd.toml", false, "configuration file not found", "run 'shepherd init' to autodetect tools and create shepherd.toml")
	} else {
		_, err := config.LoadFile(configPath)
		if err != nil {
			add("shepherd.toml", false, fmt.Sprintf("invalid configuration: %v", err), "correct the errors in shepherd.toml per ADR-0004")
		} else {
			add("shepherd.toml", true, "valid configuration schema", "")
		}
	}

	// 2. Git repository check
	gitDir := filepath.Join(root, ".git")
	if stat, err := os.Stat(gitDir); err == nil && (stat.IsDir() || stat.Mode().IsRegular()) {
		add("git repository", true, "repository root detected", "")
	} else {
		add("git repository", false, "not a git repository", "run 'git init' to initialize version control")
	}

	// 3. Agent CLIs on PATH
	type cliInfo struct {
		bin string
		pkg string
	}
	clis := []cliInfo{
		{bin: "claude", pkg: "npm i -g @anthropic-ai/claude-code"},
		{bin: "agy", pkg: "install Antigravity CLI"},
		{bin: "codex", pkg: "install Codex CLI and run 'codex login'"},
		{bin: "opencode", pkg: "install opencode for local model execution"},
	}

	foundAnyAgent := false
	for _, cli := range clis {
		p, err := onboard.LookCLIPath(cli.bin)
		if err == nil {
			foundAnyAgent = true
			detail := fmt.Sprintf("found at %s", p)
			if cli.bin == "opencode" {
				ocInfo := onboard.DetectOpenCodeInfo(root)
				if ocSummary := ocInfo.Summary(); ocSummary != "" {
					detail = fmt.Sprintf("found at %s — %s", p, ocSummary)
				}
			}
			add(cli.bin, true, detail, "")
		}
	}
	if !foundAnyAgent {
		add("agent CLIs", false, "no supported agent CLIs (claude, agy, codex, opencode) found on PATH", "install at least one agent CLI (e.g. npm i -g @anthropic-ai/claude-code)")
	}

	// 4. Sandboxing tool on Linux (ADR-0006)
	if runtime.GOOS == "linux" {
		bwrapPath, err := exec.LookPath("bwrap")
		if err == nil {
			add("sandbox (bwrap)", true, fmt.Sprintf("found at %s", bwrapPath), "")
		} else {
			add("sandbox (bwrap)", false, "bubblewrap ('bwrap') not found on PATH", "install bubblewrap (e.g. 'pacman -S bubblewrap' or 'apt install bubblewrap')")
		}
	}

	return results, allOk
}
