package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/migrate"
	"github.com/erinwnash/Shepherd/internal/plan"
)

var (
	reviewMedRe = regexp.MustCompile(`(?m)^review:\s*["']?MED["']?`)
	reviewHiRe  = regexp.MustCompile(`(?m)^review:\s*["']?HI["']?`)
	reviewLoRe  = regexp.MustCompile(`(?m)^review:\s*["']?LO["']?`)
	effortMedRe = regexp.MustCompile(`(?m)^(est_effort|effort):\s*["']?MED["']?`)
)

func planFormat(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("plan format", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "project root directory")
	planDir := fl.String("plan", "", "plan directory (defaults to docs/plan or queue.root from shepherd.toml)")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	targetDir := *planDir
	if targetDir == "" {
		if cfg, err := config.LoadFile(filepath.Join(*root, "shepherd.toml")); err == nil && cfg.Queue.Root != "" {
			targetDir = cfg.Queue.Root
		} else {
			targetDir = "docs/plan"
		}
	}
	if !filepath.IsAbs(targetDir) {
		targetDir = filepath.Join(*root, targetDir)
	}

	if st, err := os.Stat(targetDir); err != nil || !st.IsDir() {
		fmt.Fprintf(stderr, "shepherd format: directory %s does not exist\n", targetDir)
		return 1
	}

	formattedCount := 0
	err := filepath.WalkDir(targetDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := strings.ToLower(d.Name())
			if name == "_done" || name == ".done" || strings.HasSuffix(name, "done") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") || plan.ReservedNames[strings.ToLower(d.Name())] {
			return nil
		}

		b, rErr := os.ReadFile(p)
		if rErr != nil {
			return rErr
		}

		// Normalize CRLF to LF
		clean := bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))

		// Check if it's legacy bold-lines format without frontmatter
		if migrate.IsLegacyName(d.Name()) && !bytes.HasPrefix(clean, []byte("---\n")) {
			res, mErr := migrate.BoldLines(filepath.ToSlash(p), clean, migrate.Options{})
			if mErr == nil && len(res.Out) > 0 {
				_ = os.WriteFile(p, res.Out, 0644)
				formattedCount++
				return nil
			}
		}

		// Normalize frontmatter review and effort abbreviations if present
		updated := reviewMedRe.ReplaceAll(clean, []byte("review: MEDIUM"))
		updated = reviewHiRe.ReplaceAll(updated, []byte("review: HIGH"))
		updated = reviewLoRe.ReplaceAll(updated, []byte("review: LOW"))
		updated = effortMedRe.ReplaceAll(updated, []byte("${1}: M"))

		if !bytes.Equal(clean, updated) {
			_ = os.WriteFile(p, updated, 0644)
			formattedCount++
		}
		return nil
	})

	if err != nil {
		fmt.Fprintf(stderr, "shepherd format: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "formatted %d work order(s) in %s\n", formattedCount, targetDir)
	return 0
}
