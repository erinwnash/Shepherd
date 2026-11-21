package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/erinwnash/Shepherd/internal/db"
)

func cmdPR(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("pr", flag.ContinueOnError)
	fl.SetOutput(stderr)
	sprint := fl.String("sprint", "", "filter chunks by sprint ID")
	dbPath := fl.String("db", filepath.Join(".shepherd", "state.db"), "path to SQLite state database")
	title := fl.String("title", "", "PR title (default: auto-generated from sprint)")
	base := fl.String("base", "main", "base branch for pull request")
	create := fl.Bool("create", false, "invoke 'gh pr create' instead of printing the PR body")
	draft := fl.Bool("draft", false, "create PR as draft when --create is specified")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	store, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd pr: open database: %v\n", err)
		return 1
	}
	defer store.Close()

	chunks, err := store.ListChunks("")
	if err != nil {
		fmt.Fprintf(stderr, "shepherd pr: list chunks: %v\n", err)
		return 1
	}

	var filtered []db.ChunkRecord
	for _, ch := range chunks {
		if *sprint != "" && ch.Sprint != *sprint {
			continue
		}
		filtered = append(filtered, ch)
	}

	costUSD, tokens, _ := store.GetSprintCostUSD(*sprint)

	prTitle := *title
	if prTitle == "" {
		if *sprint != "" {
			prTitle = fmt.Sprintf("feat(sprint-%s): automated ShepherdAI sprint delivery", *sprint)
		} else {
			prTitle = "feat: automated ShepherdAI sprint delivery"
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", prTitle)
	b.WriteString("### Summary of Work Orders\n\n")
	if len(filtered) == 0 {
		b.WriteString("_No chunks recorded in state database._\n\n")
	} else {
		b.WriteString("| Chunk ID | Title | Status | Tier | Worker |\n")
		b.WriteString("|----------|-------|--------|------|--------|\n")
		for _, ch := range filtered {
			tier := ch.Tier
			if tier == "" {
				tier = "primary"
			}
			worker := ch.WorkerID
			if worker == "" {
				worker = "local"
			}
			fmt.Fprintf(&b, "| `%s` | %s | **%s** | `%s` | `%s` |\n", ch.ID, ch.Title, ch.Status, tier, worker)
		}
		b.WriteString("\n")
	}

	b.WriteString("### Verification & Cost Telemetry\n\n")
	fmt.Fprintf(&b, "- **Total Chunks**: %d\n", len(filtered))
	fmt.Fprintf(&b, "- **Total Cost (USD)**: `$%.4f`\n", costUSD)
	fmt.Fprintf(&b, "- **Total Tokens**: `%d`\n", tokens)

	body := b.String()

	if !*create {
		fmt.Fprint(stdout, body)
		return 0
	}

	ghArgs := []string{"pr", "create", "--title", prTitle, "--body", body, "--base", *base}
	if *draft {
		ghArgs = append(ghArgs, "--draft")
	}
	cmd := exec.CommandContext(context.Background(), "gh", ghArgs...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "shepherd pr: gh pr create failed: %v\n", err)
		return 1
	}
	return 0
}
