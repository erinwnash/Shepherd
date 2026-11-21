package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

func cmdConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return cmdConfigShow(stdout, stderr)
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "show":
		return cmdConfigShow(stdout, stderr)
	case "set":
		return cmdConfigSet(rest, stdout, stderr)
	case "reload":
		return cmdConfigReload(stdout, stderr)
	default:
		fmt.Fprintf(stderr, "shepherd config: unknown subcommand %q (supported: show, set, reload)\n", sub)
		return 2
	}
}

func cmdConfigShow(stdout, stderr io.Writer) int {
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	overrides, err := client.ConfigGet(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd config show: %v\n", err)
		return 1
	}

	if len(overrides) == 0 {
		fmt.Fprintln(stdout, "no active live config overrides (using base shepherd.toml)")
		return 0
	}

	fmt.Fprintf(stdout, "%-35s | %s\n", "KEY", "OVERRIDE VALUE")
	fmt.Fprintln(stdout, strings.Repeat("-", 60))
	for k, v := range overrides {
		fmt.Fprintf(stdout, "%-35s | %s\n", k, v)
	}
	return 0
}

func cmdConfigSet(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: shepherd config set <key> <value>")
		return 2
	}
	key := args[0]
	val := args[1]

	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.ConfigSet(ctx, key, val); err != nil {
		fmt.Fprintf(stderr, "shepherd config set: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "configuration override %s = %s scheduled for next chunk boundary\n", key, val)
	return 0
}

func cmdConfigReload(stdout, stderr io.Writer) int {
	client := getClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.ConfigReload(ctx); err != nil {
		fmt.Fprintf(stderr, "shepherd config reload: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "configuration reloaded")
	return 0
}
