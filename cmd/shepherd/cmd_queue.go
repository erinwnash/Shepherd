package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/erinwnash/Shepherd/internal/db"
)

func cmdQueue(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return cmdQueueList(stdout, stderr)
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "ls", "list":
		return cmdQueueList(stdout, stderr)
	case "skip":
		return cmdQueueSingleAction("skip", rest, stdout, stderr)
	case "retry":
		return cmdQueueSingleAction("retry", rest, stdout, stderr)
	case "hold":
		return cmdQueueSingleAction("hold", rest, stdout, stderr)
	case "release":
		return cmdQueueSingleAction("release", rest, stdout, stderr)
	case "diff":
		return cmdQueueDiff(rest, stdout, stderr)
	case "approve":
		return cmdQueueApprove(rest, stdout, stderr)
	case "reject":
		return cmdQueueReject(rest, stdout, stderr)
	case "pin":
		return cmdQueuePin(rest, stdout, stderr)
	case "unblock":
		return cmdUnblock(rest, stdout, stderr)
	case "check":
		return cmdQueueCheck(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "shepherd queue: unknown subcommand %q (supported: ls, skip, retry, hold, release, unblock, diff, approve, reject, pin, check)\n", sub)
		return 2
	}
}

func cmdQueueList(stdout, stderr io.Writer) int {
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	chunks, err := client.QueueList(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd queue: %v\n", err)
		return 1
	}

	if len(chunks) == 0 {
		fmt.Fprintln(stdout, "queue is empty")
		return 0
	}

	fmt.Fprintf(stdout, "%-6s | %-10s | %-8s | %-12s | %s\n", "ID", "STATUS", "SPRINT", "WORKER", "TITLE")
	fmt.Fprintln(stdout, strings.Repeat("-", 70))
	for _, ch := range chunks {
		fmt.Fprintf(stdout, "%-6s | %-10s | %-8s | %-12s | %s\n", ch.ID, ch.Status, ch.Sprint, ch.WorkerID, ch.Title)
	}
	return 0
}

func cmdQueueSingleAction(action string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "usage: shepherd queue %s <chunk-id>\n", action)
		return 2
	}
	chunkID := args[0]

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	switch action {
	case "skip":
		err = client.QueueSkip(ctx, chunkID)
	case "retry":
		err = client.QueueRetry(ctx, chunkID)
	case "hold":
		err = client.QueueHold(ctx, chunkID)
	case "release":
		err = client.QueueRelease(ctx, chunkID)
	}

	if err != nil {
		fmt.Fprintf(stderr, "shepherd queue %s: %v\n", action, err)
		return 1
	}
	fmt.Fprintf(stdout, "chunk %s %sed\n", chunkID, action)
	return 0
}

func cmdQueueDiff(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: shepherd queue diff <chunk-id>")
		return 2
	}
	chunkID := args[0]

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	diff, err := client.QueueDiff(ctx, chunkID)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd queue diff: %v\n", err)
		return 1
	}
	if strings.TrimSpace(diff) == "" {
		fmt.Fprintf(stdout, "no pending diff recorded for chunk %s\n", chunkID)
		return 0
	}
	fmt.Fprintln(stdout, diff)
	return 0
}

func cmdQueueApprove(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: shepherd queue approve <chunk-id>")
		return 2
	}
	chunkID := args[0]

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.QueueApprove(ctx, chunkID); err != nil {
		fmt.Fprintf(stderr, "shepherd queue approve: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "chunk %s approved and committed\n", chunkID)
	return 0
}

func cmdQueueReject(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("queue reject", flag.ContinueOnError)
	fl.SetOutput(stderr)
	reason := fl.String("reason", "rejected by operator", "rejection reason")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: shepherd queue reject [--reason REASON] <chunk-id>")
		return 2
	}
	chunkID := fl.Arg(0)

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.QueueReject(ctx, chunkID, *reason); err != nil {
		fmt.Fprintf(stderr, "shepherd queue reject: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "chunk %s rejected (%s)\n", chunkID, *reason)
	return 0
}

func cmdQueuePin(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("queue pin", flag.ContinueOnError)
	fl.SetOutput(stderr)
	tier := fl.String("tier", "", "target tier to pin (e.g. strong, primary)")
	pool := fl.String("pool", "", "target pool to pin (e.g. claude, agy)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: shepherd queue pin <chunk-id> [--tier TIER] [--pool POOL]")
		return 2
	}
	chunkID := fl.Arg(0)

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.QueuePin(ctx, chunkID, *tier, *pool); err != nil {
		fmt.Fprintf(stderr, "shepherd queue pin: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "chunk %s pinned (tier=%q, pool=%q)\n", chunkID, *tier, *pool)
	return 0
}

func cmdUnblock(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("unblock", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "project root directory")
	all := fl.Bool("all", false, "unblock all blocked chunks")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	targetChunk := ""
	if fl.NArg() > 0 && !*all {
		targetChunk = fl.Arg(0)
	}

	// 1. Try control client first (if daemon is running)
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	chunks, err := client.QueueList(ctx)
	if err == nil {
		var unblocked []string
		for _, ch := range chunks {
			if targetChunk != "" {
				if ch.ID == targetChunk {
					if err := client.QueueRetry(ctx, ch.ID); err != nil {
						fmt.Fprintf(stderr, "failed to unblock %s: %v\n", ch.ID, err)
						return 1
					}
					unblocked = append(unblocked, ch.ID)
					break
				}
			} else if ch.Status == "blocked" {
				if err := client.QueueRetry(ctx, ch.ID); err != nil {
					fmt.Fprintf(stderr, "failed to unblock %s: %v\n", ch.ID, err)
					return 1
				}
				unblocked = append(unblocked, ch.ID)
			}
		}
		if len(unblocked) == 0 {
			if targetChunk != "" {
				fmt.Fprintf(stdout, "chunk %s was not found in queue\n", targetChunk)
			} else {
				fmt.Fprintln(stdout, "no blocked chunks found in queue")
			}
			return 0
		}
		fmt.Fprintf(stdout, "unblocked %d chunk(s): %s\n", len(unblocked), strings.Join(unblocked, ", "))
		return 0
	}

	// 2. Daemon is not running: perform direct DB unblock
	return cmdUnblockDirect(*root, targetChunk, stdout, stderr)
}

func cmdUnblockDirect(root string, targetChunk string, stdout, stderr io.Writer) int {
	dbPath := filepath.Join(root, ".shepherd", "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		if _, err2 := os.Stat(filepath.Join(".shepherd", "state.db")); err2 == nil {
			dbPath = filepath.Join(".shepherd", "state.db")
		} else {
			fmt.Fprintf(stderr, "shepherd unblock: state database not found at %s (and daemon not running)\n", dbPath)
			return 1
		}
	}

	store, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd unblock: failed to open %s: %v\n", dbPath, err)
		return 1
	}
	defer store.Close()

	var unblocked []string
	if targetChunk != "" {
		if err := store.SetChunkStatus(targetChunk, "pending", "", "", ""); err != nil {
			fmt.Fprintf(stderr, "shepherd unblock: failed to unblock %s: %v\n", targetChunk, err)
			return 1
		}
		unblocked = append(unblocked, targetChunk)
	} else {
		blocked, err := store.ListChunks("blocked")
		if err != nil {
			fmt.Fprintf(stderr, "shepherd unblock: %v\n", err)
			return 1
		}
		for _, ch := range blocked {
			if err := store.SetChunkStatus(ch.ID, "pending", "", "", ""); err != nil {
				fmt.Fprintf(stderr, "shepherd unblock: failed to unblock %s: %v\n", ch.ID, err)
				return 1
			}
			unblocked = append(unblocked, ch.ID)
		}
	}

	if len(unblocked) == 0 {
		fmt.Fprintln(stdout, "no blocked chunks found")
		return 0
	}

	fmt.Fprintf(stdout, "unblocked %d chunk(s): %s\n", len(unblocked), strings.Join(unblocked, ", "))
	return 0
}


// cmdQueueCheck spot-tests one chunk against the plan tree and done directories, repairing a stale row.
func cmdQueueCheck(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: shepherd queue check <chunk-id>")
		return 2
	}
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := client.QueueCheck(ctx, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "shepherd queue check: %v\n", err)
		return 1
	}
	status := res.After
	if res.Reconciled {
		status = fmt.Sprintf("%s -> %s", res.Before, res.After)
	}
	fmt.Fprintf(stdout, "%s: %s\n  %s\n", res.ChunkID, status, res.Note)
	if res.PlanPath != "" {
		fmt.Fprintf(stdout, "  plan tree:  %s\n", res.PlanPath)
	}
	if res.DonePath != "" {
		fmt.Fprintf(stdout, "  done dir:   %s\n", res.DonePath)
	}
	if len(res.DependsOn) > 0 {
		fmt.Fprintf(stdout, "  depends on: %s\n", strings.Join(res.DependsOn, ", "))
	}
	return 0
}
