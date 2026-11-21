package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/erinwnash/Shepherd/internal/tui"
)

func cmdDashboard(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	fl.SetOutput(stderr)
	once := fl.Bool("once", false, "render single snapshot to stdout and exit")
	interval := fl.Duration("interval", 1*time.Second, "polling refresh interval")
	tabName := fl.String("tab", "overview", "dashboard view tab: overview, stack, logs, top, gpu, or nvtop")
	preflight := fl.Bool("preflight", false, "start daemon in paused preflight mode and open stack tab without executing chunks")
	root := fl.String("root", ".", "project root directory")
	planRoot := fl.String("plan", "", "plan directory (defaults to docs/plan)")
	noCloud := fl.Bool("nocloud", false, "only run chunks locally (disable cloud tiers)")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	if !*once {
		if _, err := ensureFirstRunConfig(*root, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "shepherd dashboard: %v\n", err)
			return 1
		}
	}

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	st, err := client.Status(ctx)
	cancel()

	if *preflight {
		if err != nil {
			// Daemon is not running: spawn it in paused mode
			startArgs := []string{"--root", *root, "--paused"}
			if *planRoot != "" {
				startArgs = append(startArgs, "--plan", *planRoot)
			}
			if *noCloud {
				startArgs = append(startArgs, "--nocloud")
			}
			if code := cmdStart(startArgs, stdout, stderr); code != 0 {
				return code
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				pCtx, pCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				st, err = client.Status(pCtx)
				pCancel()
				if err == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		} else {
			// Daemon is running: ensure it is paused
			pCtx, pCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = client.Pause(pCtx)
			pCancel()
		}
		*tabName = "stack"
	} else if err != nil {
		if *once {
			if _, statErr := os.Stat("shepherd.toml"); statErr != nil || (strings.ToLower(*tabName) != "stack" && strings.ToLower(*tabName) != "rules") {
				fmt.Fprintf(stderr, "shepherd dashboard: controller daemon is not running (%v)\n  Fix: start the controller with 'shepherd start'\n", err)
				return 1
			}
			st.State = "offline (config mode)"
		}
	}

	if *once {
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		chunks, _ := client.QueueList(ctx)
		quota, _ := client.QuotaGet(ctx)
		events, _ := client.Audit(ctx, 5)

		var activeTab tui.DashboardTab
		switch strings.ToLower(*tabName) {
		case "stack", "rules", "prio", "priority":
			activeTab = tui.TabStack
		case "logs", "log":
			activeTab = tui.TabLogs
			events, _ = client.Audit(ctx, 30)
		default:
			activeTab = tui.TabOverview
		}

		dashState := tui.DashboardState{
			Status:      st,
			Chunks:      chunks,
			Quota:       quota,
			AuditEvents: events,
			Timestamp:   time.Now(),
			ActiveTab:   activeTab,
		}
		tui.SyncStackFromState(&dashState)
		screen := tui.Render(dashState, 80, 24)

		fmt.Fprint(stdout, screen)
		return 0
	}

	initialTab := tui.TabOverview
	if *preflight || strings.ToLower(*tabName) == "stack" || strings.ToLower(*tabName) == "rules" || strings.ToLower(*tabName) == "priority" {
		initialTab = tui.TabStack
	} else if strings.ToLower(*tabName) == "logs" || strings.ToLower(*tabName) == "log" {
		initialTab = tui.TabLogs
	}

	// Interactive dashboard loop
	stdin := os.Stdin
	returnCode := 0
	if err := tui.RunWithTab(context.Background(), client, stdin, stdout, *interval, initialTab); err != nil {
		fmt.Fprintf(stderr, "shepherd dashboard: %v\n", err)
		returnCode = 1
	}
	return returnCode
}

