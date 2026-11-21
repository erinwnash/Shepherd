package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/erinwnash/Shepherd/internal/plan"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

func planTestFirst(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("plan test-first", flag.ContinueOnError)
	fl.SetOutput(stderr)
	writeFiles := fl.Bool("write", false, "write sub-chunk markdown files to disk")
	outDir := fl.String("out-dir", "", "output directory (default: same directory as input file)")
	root := fl.String("root", ".", "project root")
	planRoot := fl.String("plan", "docs/plan", "plan tree, relative to the project root or absolute")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	remaining := fl.Args()
	if len(remaining) == 0 {
		fmt.Fprintln(stderr, "usage: shepherd plan test-first [flags] <chunk-file-or-id>")
		return 2
	}

	target := remaining[0]
	var wo *workorder.WorkOrder
	var sourcePath string

	// Check if target is a directly accessible file
	if st, err := os.Stat(target); err == nil && !st.IsDir() {
		sourcePath = target
		data, err := os.ReadFile(sourcePath)
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: read %s: %v\n", sourcePath, err)
			return 1
		}
		parsed, err := workorder.Parse(sourcePath, data)
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: parse %s: %v\n", sourcePath, err)
			return 1
		}
		wo = parsed
	} else {
		// Look up chunk by ID from the plan tree
		orders, _, err := plan.Load(*root, *planRoot)
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: load plan from %s: %v\n", *planRoot, err)
			return 1
		}
		for _, o := range orders {
			if strings.EqualFold(o.ID, target) {
				wo = o
				sourcePath = filepath.Join(*root, o.Path)
				break
			}
		}
		if wo == nil {
			fmt.Fprintf(stderr, "shepherd: chunk %q not found as a file or in plan tree (%s)\n", target, *planRoot)
			return 1
		}
	}

	specWO, implWO, err := plan.SplitTestFirst(wo)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd: test-first decomposition failed: %v\n", err)
		return 1
	}

	dir := *outDir
	if dir == "" {
		dir = filepath.Dir(sourcePath)
	}

	chunks := []*workorder.WorkOrder{specWO, implWO}
	for _, sub := range chunks {
		md, err := plan.FormatWorkOrderMarkdown(sub)
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: format markdown %s: %v\n", sub.ID, err)
			return 1
		}

		if *writeFiles {
			dest := filepath.Join(dir, fmt.Sprintf("%s.md", sub.ID))
			if err := os.WriteFile(dest, []byte(md), 0644); err != nil {
				fmt.Fprintf(stderr, "shepherd: write %s: %v\n", dest, err)
				return 1
			}
			fmt.Fprintf(stdout, "wrote %s\n", dest)
		} else {
			fmt.Fprintf(stdout, "--- %s ---\n%s\n", sub.ID, md)
		}
	}

	return 0
}
