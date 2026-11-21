package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func cmdUp(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("up", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "project root directory")
	planRoot := fl.String("plan", "", "plan directory (defaults to docs/plan)")
	noCloud := fl.Bool("nocloud", false, "only run chunks locally with no timeout (disable cloud tiers)")
	preflight := fl.Bool("preflight", false, "start daemon paused in preflight mode and open dashboard on stack tab without executing chunks")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	cfgPath := filepath.Join(*root, "shepherd.toml")
	if _, err := os.Stat(cfgPath); err != nil {
		fmt.Fprintf(stderr, "shepherd: no shepherd.toml found in %s\nRun 'shepherd onboard' or 'shepherd init' to set up this project.\n", *root)
		return 1
	}

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	_, err := client.Status(ctx)
	cancel()

	// If daemon is not already running, spawn it in background
	if err != nil {
		startArgs := []string{"--root", *root}
		if *planRoot != "" {
			startArgs = append(startArgs, "--plan", *planRoot)
		}
		if *noCloud {
			startArgs = append(startArgs, "--nocloud")
		}
		if *preflight {
			startArgs = append(startArgs, "--paused")
		}
		code := cmdStart(startArgs, stdout, stderr)
		if code != 0 {
			return code
		}
	}

	// Launch interactive dashboard directly
	dashArgs := []string{}
	if *root != "." {
		dashArgs = append(dashArgs, "--root", *root)
	}
	if *preflight {
		dashArgs = append(dashArgs, "--preflight")
	}
	return cmdDashboard(dashArgs, stdout, stderr)
}
