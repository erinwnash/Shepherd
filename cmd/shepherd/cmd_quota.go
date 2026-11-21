package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

func cmdQuota(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "show" {
		return cmdQuotaShow(stdout, stderr)
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "set-floor":
		return cmdQuotaSetFloor(rest, stdout, stderr)
	case "set-reserve":
		return cmdQuotaSetReserve(rest, stdout, stderr)
	case "tier":
		return cmdQuotaTier(rest, stdout, stderr)
	case "reset-bucket":
		return cmdQuotaResetBucket(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "shepherd quota: unknown subcommand %q (supported: show, set-floor, set-reserve, tier, reset-bucket)\n", sub)
		return 2
	}
}

func cmdQuotaShow(stdout, stderr io.Writer) int {
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	q, err := client.QuotaGet(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd quota: %v\n", err)
		return 1
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(q)
	return 0
}

func cmdQuotaSetFloor(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: shepherd quota set-floor <pool> <pct>")
		return 2
	}
	pool := args[0]
	pct, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(stderr, "shepherd quota set-floor: invalid pct %q: %v\n", args[1], err)
		return 2
	}

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.QuotaSetFloor(ctx, pool, pct); err != nil {
		fmt.Fprintf(stderr, "shepherd quota set-floor: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "floor for pool %s set to %d%%\n", pool, pct)
	return 0
}

func cmdQuotaSetReserve(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: shepherd quota set-reserve <pool> <pct>")
		return 2
	}
	pool := args[0]
	pct, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(stderr, "shepherd quota set-reserve: invalid pct %q: %v\n", args[1], err)
		return 2
	}

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.QuotaSetReserve(ctx, pool, pct); err != nil {
		fmt.Fprintf(stderr, "shepherd quota set-reserve: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "reserve for pool %s set to %d%%\n", pool, pct)
	return 0
}

func cmdQuotaTier(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: shepherd quota tier <tier> <enable|disable>")
		return 2
	}
	tier := args[0]
	action := strings.ToLower(args[1])
	enabled := false
	switch action {
	case "enable", "on", "true":
		enabled = true
	case "disable", "off", "false":
		enabled = false
	default:
		fmt.Fprintf(stderr, "shepherd quota tier: unknown action %q (want 'enable' or 'disable')\n", args[1])
		return 2
	}

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.QuotaTierToggle(ctx, tier, enabled); err != nil {
		fmt.Fprintf(stderr, "shepherd quota tier: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tier %s enabled=%v\n", tier, enabled)
	return 0
}

func cmdQuotaResetBucket(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: shepherd quota reset-bucket <pool>")
		return 2
	}
	pool := args[0]

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.QuotaResetBucket(ctx, pool); err != nil {
		fmt.Fprintf(stderr, "shepherd quota reset-bucket: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "quota bucket and learned costs reset for pool %s\n", pool)
	return 0
}
