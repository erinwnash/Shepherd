package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/retro"
)

func cmdRetro(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("retro", flag.ContinueOnError)
	fl.SetOutput(stderr)
	dbPath := fl.String("db", filepath.Join(".shepherd", "state.db"), "path to sqlite state db")
	format := fl.String("format", "md", "output format: md or table")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	store, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd: open db %s: %v\n", *dbPath, err)
		return 1
	}
	defer store.Close()

	rep, err := retro.Generate(store)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd: generate retro: %v\n", err)
		return 1
	}

	if *format == "table" {
		fmt.Fprintf(stdout, "Total: %d, Done: %d, Blocked: %d, Held: %d\n", rep.TotalChunks, rep.DoneCount, rep.BlockedCount, rep.HeldCount)
		for p, tot := range rep.TotalSpendByPool {
			fmt.Fprintf(stdout, "Pool %s: total %d%%, avg %.1f%%\n", p, tot, rep.AverageCostByPool[p])
		}
	} else {
		fmt.Fprint(stdout, retro.FormatMarkdown(rep))
	}
	return 0
}
