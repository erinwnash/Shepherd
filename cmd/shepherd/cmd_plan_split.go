package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/erinwnash/Shepherd/internal/plan"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

func planSplit(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("plan split", flag.ContinueOnError)
	fl.SetOutput(stderr)
	parts := fl.Int("parts", 2, "number of sub-chunks to split into (2-4)")
	writeFiles := fl.Bool("write", false, "write sub-chunk markdown files to disk")
	outDir := fl.String("out-dir", "", "output directory (default: same directory as input file)")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	remaining := fl.Args()
	if len(remaining) == 0 {
		fmt.Fprintln(stderr, "usage: shepherd plan split [flags] <chunk-file.md>")
		return 2
	}

	filePath := remaining[0]
	data, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd: read %s: %v\n", filePath, err)
		return 1
	}

	wo, err := workorder.Parse(filePath, data)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd: parse %s: %v\n", filePath, err)
		return 1
	}

	subChunks, err := plan.SplitChunk(wo, plan.SplitOptions{Parts: *parts})
	if err != nil {
		fmt.Fprintf(stderr, "shepherd: split chunk: %v\n", err)
		return 1
	}

	dir := *outDir
	if dir == "" {
		dir = filepath.Dir(filePath)
	}

	for _, sub := range subChunks {
		md, err := plan.FormatWorkOrderMarkdown(sub)
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: format markdown %s: %v\n", sub.ID, err)
			return 1
		}

		if *writeFiles {
			target := filepath.Join(dir, fmt.Sprintf("%s.md", sub.ID))
			if err := os.WriteFile(target, []byte(md), 0644); err != nil {
				fmt.Fprintf(stderr, "shepherd: write %s: %v\n", target, err)
				return 1
			}
			fmt.Fprintf(stdout, "wrote %s\n", target)
		} else {
			fmt.Fprintf(stdout, "--- %s ---\n%s\n", sub.ID, md)
		}
	}

	return 0
}
