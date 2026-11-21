package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/controller"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/notify"
	"github.com/erinwnash/Shepherd/internal/plan"
	"github.com/erinwnash/Shepherd/internal/remote"
)

func getClient() *control.Client {
	sock := os.Getenv("SHEPHERD_SOCKET")
	if sock == "" {
		sock = control.DefaultSocketPath()
	}
	token := os.Getenv("SHEPHERD_TOKEN")
	return control.NewClient(sock, token)
}

func cmdStart(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("start", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "project root directory")
	planRoot := fl.String("plan", "docs/plan", "plan directory")
	foreground := fl.Bool("f", false, "run controller daemon in foreground")
	noCloud := fl.Bool("nocloud", false, "only run chunks locally with no timeout (disable cloud tiers)")
	paused := fl.Bool("paused", false, "start daemon in paused mode without launching chunks")
	preflight := fl.Bool("preflight", false, "alias for --paused")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 1. If daemon is already running, instruct it to start or pause processing
	st, err := client.Status(ctx)
	if err == nil {
		if *paused || *preflight {
			if err := client.Pause(context.Background()); err != nil {
				fmt.Fprintf(stderr, "shepherd start: %v\n", err)
				return 1
			}
			fmt.Fprintf(stdout, "shepherd daemon (state: %s) paused for preflight\n", st.State)
			return 0
		}
		if err := client.Start(context.Background()); err != nil {
			fmt.Fprintf(stderr, "shepherd start: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "shepherd daemon (state: %s) started processing queue\n", st.State)
		return 0
	}

	// 2. Validate config before spawning or starting
	cfgPath := filepath.Join(*root, "shepherd.toml")
	cfg, err := config.LoadFile(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd start: configuration error: %v\n", err)
		return 1
	}

	if (*planRoot == "docs/plan" || *planRoot == "") && cfg.Queue.Root != "" {
		*planRoot = cfg.Queue.Root
	}

	stateDir := filepath.Join(*root, ".shepherd")
	_ = os.MkdirAll(stateDir, 0700)

	// If not running in foreground and not inside a go test binary, spawn detached background daemon
	if !*foreground {
		if exe, err := os.Executable(); err == nil && !strings.HasSuffix(exe, ".test") {
			logPath := filepath.Join(stateDir, "daemon.log")
			logFile, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			bgArgs := []string{"start", "-f", "--root", *root, "--plan", *planRoot}
			if *noCloud {
				bgArgs = append(bgArgs, "--nocloud")
			}
			if *paused || *preflight {
				bgArgs = append(bgArgs, "--paused")
			}
			bgCmd := exec.Command(exe, bgArgs...)
			if logFile != nil {
				bgCmd.Stdout = logFile
				bgCmd.Stderr = logFile
			}
			if err := bgCmd.Start(); err != nil {
				if logFile != nil {
					_ = logFile.Close()
				}
				fmt.Fprintf(stderr, "shepherd start: failed to spawn background daemon: %v\n", err)
				return 1
			}
			if logFile != nil {
				_ = logFile.Close()
			}
			pid := bgCmd.Process.Pid
			_ = bgCmd.Process.Release()

			// Wait up to 2s for control socket readiness
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				pCtx, pCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				_, sErr := client.Status(pCtx)
				pCancel()
				if sErr == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			fmt.Fprintf(stdout, "shepherd daemon started in background (pid %d, log: %s)\n", pid, logPath)
			return 0
		}
	}

	dbPath := filepath.Join(stateDir, "state.db")
	store, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd start: open database %s: %v\n", dbPath, err)
		return 1
	}
	defer store.Close()

	ctrl, err := controller.New(controller.Options{
		ProjectRoot: *root,
		PlanRoot:    *planRoot,
		Config:      cfg,
		Store:       store,
		NoCloud:     *noCloud,
	})
	if err != nil {
		fmt.Fprintf(stderr, "shepherd start: %v\n", err)
		return 1
	}

	// Sync work orders from docs/plan
	orders, bad, loadErr := plan.Load(*root, *planRoot)
	if loadErr != nil {
		fmt.Fprintf(stderr, "shepherd start: error reading plan tree %s: %v\n", *planRoot, loadErr)
		ctrl.NotifyEvent(context.Background(), notify.EventPlanError, "Plan Tree Error", fmt.Sprintf("Failed to read plan tree %s: %v", *planRoot, loadErr))
	}
	if len(bad) > 0 {
		fmt.Fprintf(stderr, "shepherd start: found %d plan schema error(s) in %s:\n", len(bad), *planRoot)
		var errLines []string
		for _, f := range bad {
			fmt.Fprintf(stderr, "  [%s] %s: %s\n", f.Severity, f.Chunk, f.Message)
			errLines = append(errLines, fmt.Sprintf("• %s: %s", filepath.Base(f.Chunk), f.Message))
		}
		limit := 5
		if len(errLines) < limit {
			limit = len(errLines)
		}
		summaryMsg := fmt.Sprintf("Found %d plan schema error(s) in %s (%d valid chunks loaded):\n%s",
			len(bad), *planRoot, len(orders), strings.Join(errLines[:limit], "\n"))
		if len(bad) > limit {
			summaryMsg += fmt.Sprintf("\n...and %d more errors", len(bad)-limit)
		}
		ctrl.NotifyEvent(context.Background(), notify.EventPlanError, "Plan Validation Failed", summaryMsg)
	}
	for _, wo := range orders {
		_ = ctrl.RegisterWorkOrder(wo)
	}
	if reset, err := ctrl.ReconcileOrphanedRuns(context.Background()); err != nil {
		fmt.Fprintf(stderr, "shepherd start: reconcile orphaned chunks: %v\n", err)
	} else if len(reset) > 0 {
		fmt.Fprintf(stdout, "requeued %d chunk(s) left running by a previous daemon: %s\n", len(reset), strings.Join(reset, ", "))
	}

	if done := ctrl.ReconcileDone(); len(done) > 0 {
		fmt.Fprintf(stdout, "reconciled %d chunk(s) finished outside Shepherd: %s\n", len(done), strings.Join(done, ", "))
	}

	sockPath := control.DefaultSocketPath()
	srv := control.NewServer(ctrl, sockPath)
	if err := srv.Start(); err != nil {
		fmt.Fprintf(stderr, "shepherd start: failed to start control server on %s: %v\n", sockPath, err)
		return 1
	}
	defer srv.Close()

	listenCtx, stopListening := context.WithCancel(context.Background())
	defer stopListening()
	startNtfyListener(listenCtx, cfg, stateDir, ctrl, store, stdout, stderr)

	if *paused || *preflight {
		if err := ctrl.StartPaused(context.Background()); err != nil {
			fmt.Fprintf(stderr, "shepherd start: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "shepherd daemon running on %s (loaded %d chunks, paused in preflight mode)\n", sockPath, len(orders))
	} else {
		if err := ctrl.Start(context.Background()); err != nil {
			fmt.Fprintf(stderr, "shepherd start: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "shepherd daemon running on %s (loaded %d chunks)\n", sockPath, len(orders))
	}
	if !*foreground {
		return 0
	}

	// Wait for interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	fmt.Fprintln(stdout, "\nstopping shepherd daemon cleanly...")
	_ = ctrl.Stop(context.Background())
	return 0
}

// startNtfyListener watches the ntfy notification topic for operator keywords (obliviate = full
// stop). Commands come from .shepherd/ntfy-commands.toml, written with safe defaults if missing.
func startNtfyListener(ctx context.Context, cfg *config.Config, stateDir string, ctrl *controller.Controller, store *db.Store, stdout, stderr io.Writer) {
	cmds, path, err := remote.EnsureCommandsFile(stateDir)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd start: ntfy commands: %v (falling back to built-in obliviate only)\n", err)
		cmds = remote.DefaultCommands()
	}
	if cmds.TokenEnv == "" {
		cmds.TokenEnv = cfg.Notify.NtfyTokenEnv // one token for publishing and subscribing
	}
	if !cmds.Enabled {
		fmt.Fprintf(stdout, "ntfy command listener disabled (%s: enabled = false)\n", path)
		return
	}
	// The controller's topic already reflects shepherd.toml, env and any live rotation.
	l, err := remote.NewListener(cmds, ctrl.NtfyURL(), ctrl, store.EventLog)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd start: ntfy command listener not started: %v\n", err)
		return
	}
	l.Logf = func(format string, args ...any) { fmt.Fprintf(stdout, format+"\n", args...) }
	if cmds.URL == "" { // a dedicated command topic in ntfy-commands.toml is not rotated
		ctrl.SetNtfyTopicHook(func(topicURL string) {
			if err := l.SetURL(topicURL); err != nil {
				fmt.Fprintf(stderr, "ntfy: listener did not follow topic rotation: %v\n", err)
			}
		})
	}
	go l.Run(ctx)
}

func cmdStop(args []string, stdout, stderr io.Writer) int {
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Stop(ctx); err != nil {
		fmt.Fprintf(stderr, "shepherd stop: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "clean stop scheduled for next chunk boundary")
	return 0
}

func cmdHalt(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("halt", flag.ContinueOnError)
	fl.SetOutput(stderr)
	reason := fl.String("reason", "halted by operator", "reason for aborting chunk")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := client.Halt(ctx, *reason); err != nil {
		fmt.Fprintf(stderr, "shepherd halt: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "current chunk halted, worktree rolled back, and chunk requeued")
	return 0
}

func cmdPause(args []string, stdout, stderr io.Writer) int {
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Pause(ctx); err != nil {
		fmt.Fprintf(stderr, "shepherd pause: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "controller paused")
	return 0
}

func cmdResume(args []string, stdout, stderr io.Writer) int {
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Resume(ctx); err != nil {
		fmt.Fprintf(stderr, "shepherd resume: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "controller resumed")
	return 0
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := client.Status(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd: controller is not running (%v)\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Daemon State:   %s\n", strings.ToUpper(st.State))
	if st.RunningChunkID != "" {
		fmt.Fprintf(stdout, "Running Chunk:  %s (worker: %s)\n", st.RunningChunkID, st.WorkerID)
	}
	if st.ActiveTier != "" {
		fmt.Fprintf(stdout, "Default Tier:   %s\n", st.ActiveTier)
	}
	if len(st.Stack) > 0 {
		fmt.Fprintf(stdout, "Agent Stack:    %s\n", strings.Join(st.Stack, ", "))
	}
	fmt.Fprintf(stdout, "Queue:          %d pending, %d running, %d blocked, %d completed\n",
		st.QueuePending, st.QueueRunning, st.QueueBlocked, st.QueueCompleted)
	if st.TotalCostUSD > 0 || st.TotalTokens > 0 {
		fmt.Fprintf(stdout, "Cost / Tokens:  $%.4f (%d tokens)\n", st.TotalCostUSD, st.TotalTokens)
	}
	fmt.Fprintf(stdout, "Drain Mode:     %v\n", st.DrainMode)
	fmt.Fprintf(stdout, "Lockdown:       %v\n", st.Lockdown)
	if st.NtfyTopic != "" {
		fmt.Fprintf(stdout, "ntfy Topic:     %s\n", st.NtfyTopic)
	} else {
		fmt.Fprintln(stdout, "ntfy Topic:     not configured")
	}
	if len(st.Overrides) > 0 {
		fmt.Fprintf(stdout, "Active Config Overrides (%d):\n", len(st.Overrides))
		for k, v := range st.Overrides {
			fmt.Fprintf(stdout, "  %s = %s\n", k, v)
		}
	}
	return 0
}

func cmdDrain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: shepherd drain <on|off> [--until RFC3339]")
		return 2
	}

	mode := strings.ToLower(args[0])
	enabled := false
	switch mode {
	case "on", "true", "1":
		enabled = true
	case "off", "false", "0":
		enabled = false
	default:
		fmt.Fprintf(stderr, "shepherd drain: unknown mode %q (want 'on' or 'off')\n", args[0])
		return 2
	}

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Drain(ctx, enabled, nil); err != nil {
		fmt.Fprintf(stderr, "shepherd drain: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "drain mode set to %v\n", enabled)
	return 0
}

func cmdLockdown(args []string, stdout, stderr io.Writer) int {
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Lockdown(ctx); err != nil {
		fmt.Fprintf(stderr, "shepherd lockdown: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "lockdown active: all executions halted and runner stopped")
	return 0
}

func cmdUnlock(args []string, stdout, stderr io.Writer) int {
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Unlock(ctx); err != nil {
		fmt.Fprintf(stderr, "shepherd unlock: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "lockdown released")
	return 0
}

func cmdAudit(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("audit", flag.ContinueOnError)
	fl.SetOutput(stderr)
	limit := fl.Int("limit", 50, "number of audit log events to display")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	events, err := client.Audit(ctx, *limit)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd audit: %v\n", err)
		return 1
	}

	if len(events) == 0 {
		fmt.Fprintln(stdout, "no audit events recorded")
		return 0
	}

	fmt.Fprintf(stdout, "%-5s | %-20s | %-16s | %-10s | %s\n", "SEQ", "TIMESTAMP", "TYPE", "ACTOR", "PAYLOAD")
	fmt.Fprintln(stdout, strings.Repeat("-", 80))
	for _, ev := range events {
		ts := ev.Timestamp.Format("2006-01-02 15:04:05")
		fmt.Fprintf(stdout, "%-5d | %-20s | %-16s | %-10s | %s\n", ev.Seq, ts, ev.Type, ev.Actor, ev.Payload)
	}
	return 0
}
