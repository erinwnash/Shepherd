package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/erinwnash/Shepherd/internal/onboard"
	"golang.org/x/term"
)

// ensureFirstRunConfig checks if shepherd.toml exists in root. If missing and running in an
// interactive terminal, it automatically launches the First-Run Config Wizard.
func ensureFirstRunConfig(root string, stdout, stderr io.Writer) (bool, error) {
	tomlPath := filepath.Join(root, "shepherd.toml")
	if _, err := os.Stat(tomlPath); err == nil {
		return false, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, nil
	}

	fmt.Fprintf(stdout, "No shepherd.toml found in %s — launching First-Run Configuration Wizard...\n\n", root)
	env := onboard.DetectEnvironment(root)
	cfg := onboard.Config{
		ProjectRoot: root,
		ProjectName: env.ProjectName,
		Interactive: true,
	}
	cfg, err := onboard.RunConfigWizard(context.Background(), os.Stdin, stdout, env, cfg)
	if err != nil {
		return false, err
	}
	res, err := onboard.Scaffold(cfg)
	if err != nil {
		return false, err
	}
	fmt.Fprintf(stdout, "\nFirst-run configuration saved to %s\n", res.ShepherdTOMLPath)
	return true, nil
}

func cmdOnboard(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("onboard", flag.ContinueOnError)
	fl.SetOutput(stderr)

	dir := fl.String("dir", ".", "project root directory to onboard")
	name := fl.String("name", "", "project name (default auto-detected)")
	goal := fl.String("goal", "", "high-level project/sprint objective prompt")
	targetCLI := fl.String("cli", "", "target agent CLI (agy, claude, codex, opencode, aider, gemini)")
	outDir := fl.String("out", "", "output directory for orchestration files (default same as --dir)")
	interactive := fl.Bool("interactive", false, "run interactive setup wizard")
	fl.BoolVar(interactive, "i", false, "shorthand for --interactive")
	wizardMode := fl.Bool("wizard", false, "launch full-screen AI router & CLI configuration wizard")
	fl.BoolVar(wizardMode, "w", false, "shorthand for --wizard")
	nonInteractive := fl.Bool("non-interactive", false, "run non-interactively using flags and auto-detected defaults")
	autoLaunch := fl.Bool("launch", false, "immediately launch the target CLI with launch.md after scaffolding")
	fl.BoolVar(autoLaunch, "l", false, "shorthand for --launch")
	dryRun := fl.Bool("dry-run", false, "preview generated files and launch command without writing to disk")
	refresh := fl.Bool("refresh", false, "append newly discovered providers to an existing shepherd.toml without changing existing settings")

	if err := fl.Parse(args); err != nil {
		return 2
	}

	if *refresh {
		res, err := onboard.RefreshProviders(context.Background(), *dir, *dryRun)
		if err != nil {
			fmt.Fprintf(stderr, "shepherd onboard --refresh: %v\n", err)
			return 1
		}
		verb := "Appended"
		if *dryRun {
			verb = "Would append"
		}
		fmt.Fprintf(stdout, "Refreshing %s (existing settings untouched)\n", res.TOMLPath)
		fmt.Fprintf(stdout, "  already configured: %d provider(s)\n", res.Unchanged)
		if len(res.AddedTiers) == 0 {
			fmt.Fprintf(stdout, "  no new providers found\n")
		} else {
			fmt.Fprintf(stdout, "  %s tiers (disabled): %s\n", verb, strings.Join(res.AddedTiers, ", "))
			if len(res.AddedPools) > 0 {
				fmt.Fprintf(stdout, "  %s pools: %s\n", verb, strings.Join(res.AddedPools, ", "))
			}
			fmt.Fprintf(stdout, "  enable them in 'shepherd dashboard' (stack tab)\n")
		}
		for _, st := range res.Stale {
			fmt.Fprintf(stdout, "  warning: stale model for tier %s\n", st)
		}
		return 0
	}

	if _, err := os.Stat(filepath.Join(*dir, "shepherd.toml")); err == nil && !*dryRun {
		fmt.Fprintf(stderr, "note: shepherd.toml already exists and will be OVERWRITTEN; use 'shepherd onboard --refresh' to append new providers instead\n")
	}

	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	if !*nonInteractive && isTTY && !*dryRun && *name == "" && *goal == "" && *targetCLI == "" {
		*wizardMode = true
	}
	if *nonInteractive {
		*interactive = false
		*wizardMode = false
	}

	cfg := onboard.Config{
		ProjectRoot: *dir,
		ProjectName: *name,
		Goal:        *goal,
		TargetCLI:   *targetCLI,
		OutDir:      *outDir,
		DryRun:      *dryRun,
		AutoLaunch:  *autoLaunch,
		Interactive: *interactive || *wizardMode,
	}

	env := onboard.DetectEnvironment(cfg.ProjectRoot)

	// Run full interactive Router & CLI Config Wizard when in wizard mode or interactive TTY
	if *wizardMode || (*interactive && isTTY) {
		var err error
		cfg, err = onboard.RunConfigWizard(context.Background(), os.Stdin, stdout, env, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "shepherd onboard: %v\n", err)
			return 1
		}
	} else if cfg.Interactive {
		var err error
		cfg, err = onboard.RunInteractive(os.Stdin, stdout, env, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "shepherd onboard: %v\n", err)
			return 1
		}
	}

	res, err := onboard.Scaffold(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd onboard: %v\n", err)
		return 1
	}

	// Output summary of what was generated
	modeStr := "Created orchestration files"
	if cfg.DryRun {
		modeStr = "Dry-run preview (no files written)"
	}
	fmt.Fprintf(stdout, "\n=== Shepherd Onboarding: %s ===\n", res.Environment.ProjectName)
	fmt.Fprintf(stdout, "Status:         %s\n", modeStr)
	fmt.Fprintf(stdout, "Target Stack:   %s\n", res.Environment.Language)
	if len(res.Environment.InstalledCLIs) > 0 {
		fmt.Fprintf(stdout, "Agent Stack:    %s\n", strings.Join(res.Environment.InstalledCLIs, ", "))
	}
	if ocSummary := res.Environment.OpenCode.Summary(); ocSummary != "" {
		fmt.Fprintf(stdout, "Local OpenCode: %s\n", ocSummary)
	}
	if res.Environment.Agy.Installed {
		activeStr := res.Environment.Agy.ActiveModel
		if res.Environment.Agy.ActiveTitle != "" {
			activeStr = fmt.Sprintf("%s (%s)", res.Environment.Agy.ActiveTitle, res.Environment.Agy.ActiveModel)
		}
		fmt.Fprintf(stdout, "Antigravity CLI: %s [polled %d models; run 'agy models' to list all]\n", activeStr, len(res.Environment.Agy.Models))
	}
	if len(res.Environment.Providers) > 0 {
		var pSummaries []string
		for _, p := range res.Environment.Providers {
			state := "ON"
			if !p.Enabled {
				state = "OFF"
			}
			pSummaries = append(pSummaries, fmt.Sprintf("#%d %s [%s]", p.Priority, p.ID, state))
		}
		fmt.Fprintf(stdout, "AI Providers:   %s\n", strings.Join(pSummaries, " -> "))
	}
	fmt.Fprintf(stdout, "Target CLI:     %s\n", res.TargetCLI)
	fmt.Fprintf(stdout, "Launch Prompt:  %s\n", res.LaunchMDPath)
	fmt.Fprintf(stdout, "\nScaffolded Files:\n")
	for _, f := range res.FilesCreated {
		fmt.Fprintf(stdout, "  + %s\n", f)
	}

	launchCmdStr := res.LaunchCommand + " " + strings.Join(res.LaunchArgs, " ")
	fmt.Fprintf(stdout, "\nAgent CLI Launch Command:\n  %s\n", launchCmdStr)

	// Auto-launch if requested
	if cfg.AutoLaunch && !cfg.DryRun {
		fmt.Fprintf(stdout, "\nLaunching %s...\n", res.TargetCLI)
		ctx := context.Background()
		if err := onboard.LaunchCLI(ctx, cfg, res, os.Stdin, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "shepherd onboard: error running %s: %v\n", res.TargetCLI, err)
			return 1
		}
	} else if !cfg.DryRun {
		fmt.Fprintf(stdout, "\nNext Steps:\n")
		fmt.Fprintf(stdout, "  1. Open interactive Stack & Priority TUI: 'shepherd dashboard'\n")
		fmt.Fprintf(stdout, "  2. Verify work orders: 'shepherd plan lint'\n")
		fmt.Fprintf(stdout, "  3. Start execution: 'shepherd start'\n")
	}

	return 0
}

