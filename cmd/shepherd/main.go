// Command shepherd is the Shepherd CLI.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/doctor"
	"github.com/erinwnash/Shepherd/internal/initcmd"
	"github.com/erinwnash/Shepherd/internal/migrate"
	"github.com/erinwnash/Shepherd/internal/plan"
)

const usage = `usage: shepherd [command] [flags]

quick start:
  (no command)   boot up daemon and interactive dashboard in current repo
  up, run        boot up daemon and attach interactive dashboard (--preflight, --nocloud)
  format, fmt    format and normalize work orders in docs/plan (fixes review codes, converts bold-lines)

project onboarding:
  onboard        per-project onboarding wizard & launch.md orchestration (--cli, --goal, -l)
  wizard         alias for onboard

lifecycle:
  start          start queue execution or run local daemon (--root DIR, -f, --nocloud)
  stop           clean stop at chunk boundary
  halt           abort current chunk immediately, roll back git worktree, and requeue
  pause          pause queue execution
  resume         resume paused queue execution
  status         display live controller status, active chunk, and queue metrics
  drain <on|off> spend quota toward reset window
  lockdown       immediately halt executions and lock daemon
  unlock         release lockdown mode

queue & plan:
  unblock        reset blocked status on all (or specified) chunks (--root DIR, [chunk-id])
  queue <ls|skip|retry|hold|release|unblock|diff|approve|reject|pin>  inspect and steer work orders
  plan lint      validate the work orders under the plan root
  plan migrate   convert legacy work orders to frontmatter, once (--from bold-lines)
  plan split     partition oversize work orders into sub-chunks (--parts N, --write)
  plan test-first decompose chunk into test-authoring and read-only implementation pair

configuration & quota:
  config <show|set|reload>   inspect and set live configuration overrides
  quota <show|set-floor|set-reserve|tier|reset-bucket>   inspect and manage quota pools

remote workers:
  worker <add|join|ls|revoke|drain>   LAN worker pairing, listing, revocation, and draining

service & retrospective:
  pr             generate sprint PR summary body or create PR via gh (--sprint, --create)
  retro          generate sprint retrospective and quota cost ledger (--format md|table)
  service <install|uninstall|status>  manage systemd / launchd user background service

diagnostics & observation:
  dashboard      interactive terminal dashboard TUI (--preflight, --tab stack, --once)
  web            embedded web dashboard & live DAG visualizer (--port 8080)
  init           autodetect project build tools and scaffold shepherd.toml
  doctor         preflight diagnostic checks with actionable remediation
  audit          display immutable hash-chained audit log events
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		if _, err := os.Stat("shepherd.toml"); err == nil {
			return cmdUp(nil, stdout, stderr)
		}
		if _, err := os.Stat("docs/plan"); err == nil {
			return cmdUp(nil, stdout, stderr)
		}
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "up", "run":
		return cmdUp(args[1:], stdout, stderr)
	case "format", "fmt":
		return planFormat(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version", "-v":
		return cmdVersion(args[1:], stdout, stderr)
	case "dashboard", "tui":
		return cmdDashboard(args[1:], stdout, stderr)
	case "web":
		return cmdWeb(args[1:], stdout, stderr)
	case "onboard", "wizard":
		return cmdOnboard(args[1:], stdout, stderr)
	case "init":
		return cmdInit(args[1:], stdout, stderr)
	case "doctor":
		return cmdDoctor(args[1:], stdout, stderr)
	case "pr":
		return cmdPR(args[1:], stdout, stderr)
	case "retro":
		return cmdRetro(args[1:], stdout, stderr)
	case "service":
		return cmdService(args[1:], stdout, stderr)
	case "start":
		return cmdStart(args[1:], stdout, stderr)
	case "stop":
		return cmdStop(args[1:], stdout, stderr)
	case "halt":
		return cmdHalt(args[1:], stdout, stderr)
	case "pause":
		return cmdPause(args[1:], stdout, stderr)
	case "resume":
		return cmdResume(args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "drain":
		return cmdDrain(args[1:], stdout, stderr)
	case "lockdown":
		return cmdLockdown(args[1:], stdout, stderr)
	case "unlock":
		return cmdUnlock(args[1:], stdout, stderr)
	case "audit":
		return cmdAudit(args[1:], stdout, stderr)
	case "unblock":
		return cmdUnblock(args[1:], stdout, stderr)
	case "queue":
		return cmdQueue(args[1:], stdout, stderr)
	case "config":
		return cmdConfig(args[1:], stdout, stderr)
	case "quota":
		return cmdQuota(args[1:], stdout, stderr)
	case "worker", "workers":
		return cmdWorker(args[1:], stdout, stderr)
	case "plan":
		if len(args) >= 2 {
			switch args[1] {
			case "format", "fmt":
				return planFormat(args[2:], stdout, stderr)
			case "lint":
				return planLint(args[2:], stdout, stderr)
			case "migrate":
				if len(args) == 2 {
					return planFormat(nil, stdout, stderr)
				}
				return planMigrate(args[2:], stdout, stderr)
			case "split":
				return planSplit(args[2:], stdout, stderr)
			case "test-first":
				return planTestFirst(args[2:], stdout, stderr)
			}
		}
	}
	fmt.Fprint(stderr, usage)
	return 2
}

func planLint(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("plan lint", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "project root")
	planRoot := fl.String("plan", "docs/plan", "plan tree, relative to the project root or absolute")
	adrDir := fl.String("adr", "docs/adr", "ADR directory, relative to the project root")
	maxFiles := fl.Int("max-files", 5, "maximum declared files per chunk")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	if *planRoot == "docs/plan" {
		if cfg, cErr := config.LoadFile(filepath.Join(*root, "shepherd.toml")); cErr == nil && cfg.Queue.Root != "" {
			*planRoot = cfg.Queue.Root
		}
	}

	orders, findings, err := plan.Load(*root, *planRoot)
	if err != nil {
		fmt.Fprintf(stderr, "shepherd: %v\n", err)
		return 2
	}
	findings = append(findings, plan.Lint(orders, plan.Options{
		MaxFiles:    *maxFiles,
		SprintOrder: plan.SprintOrder(orders),
	}, plan.DirRepo{Root: *root, ADRDir: *adrDir})...)

	errs, warns := 0, 0
	for _, f := range findings {
		fmt.Fprintln(stdout, f)
		if f.Severity == plan.Error {
			errs++
		} else {
			warns++
		}
	}
	fmt.Fprintf(stdout, "%d chunks, %d errors, %d warnings\n", len(orders), errs, warns)
	if errs > 0 {
		return 1
	}
	return 0
}

type dirList []string

func (d *dirList) String() string     { return strings.Join(*d, ",") }
func (d *dirList) Set(v string) error { *d = append(*d, v); return nil }

func planMigrate(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("plan migrate", flag.ContinueOnError)
	fl.SetOutput(stderr)
	from := fl.String("from", "", "legacy format: bold-lines")
	outDir := fl.String("out", "docs/plan", "output plan tree")
	root := fl.String("root", "", "project root, used to expand abbreviated .../ paths (optional)")
	var done dirList
	fl.Var(&done, "done", "directory of finished legacy work orders (repeatable)")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "usage: shepherd plan migrate --from bold-lines [--out DIR] [--root DIR] [--done DIR]... DIR...")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *from != "bold-lines" {
		fmt.Fprintf(stderr, "shepherd: unknown --from %q (supported: bold-lines)\n", *from)
		return 2
	}
	if fl.NArg() == 0 && len(done) == 0 {
		fl.Usage()
		return 2
	}

	type src struct {
		path string
		done bool
	}
	var srcs []src
	collect := func(dir string, isDone bool) error {
		return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 || d.IsDir() || !migrate.IsLegacyName(d.Name()) {
				return nil
			}
			srcs = append(srcs, src{p, isDone})
			return nil
		})
	}
	for _, d := range done {
		if err := collect(d, true); err != nil {
			fmt.Fprintf(stderr, "shepherd: %v\n", err)
			return 2
		}
	}
	for _, d := range fl.Args() {
		if err := collect(d, false); err != nil {
			fmt.Fprintf(stderr, "shepherd: %v\n", err)
			return 2
		}
	}

	var resolve func(string) []string
	var exists func(string) bool
	if *root != "" {
		files, err := projectFiles(*root)
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: %v\n", err)
			return 2
		}
		have := map[string]bool{}
		for _, f := range files {
			have[f] = true
		}
		exists = func(rel string) bool { return have[rel] }
		resolve = func(suffix string) []string {
			var m []string
			for _, f := range files {
				if strings.HasSuffix(f, "/"+suffix) {
					m = append(m, f)
				}
			}
			return m
		}
	}

	// Convert everything before writing anything: one bad file leaves no half-migrated tree.
	var results []migrate.Result
	names := map[string]string{}
	failed := 0
	for _, s := range srcs {
		b, err := os.ReadFile(s.path)
		if err == nil {
			var r migrate.Result
			r, err = migrate.BoldLines(filepath.ToSlash(s.path), b, migrate.Options{Done: s.done, Resolve: resolve, Exists: exists})
			if err == nil {
				if prev, dup := names[strings.ToLower(r.Name)]; dup {
					err = fmt.Errorf("%s: same output %s as %s", s.path, r.Name, prev)
				} else {
					names[strings.ToLower(r.Name)] = s.path
					results = append(results, r)
				}
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: %v\n", err)
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(stderr, "shepherd: %d of %d work orders failed; nothing written\n", failed, len(srcs))
		return 1
	}
	for _, r := range results {
		p := filepath.Join(*outDir, filepath.FromSlash(r.Name))
		if _, err := os.Lstat(p); err == nil {
			fmt.Fprintf(stderr, "shepherd: %s exists; migration runs once and never overwrites\n", p)
			return 1
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	notes := 0
	for _, r := range results {
		p := filepath.Join(*outDir, filepath.FromSlash(r.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintf(stderr, "shepherd: %v\n", err)
			return 1
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, err = f.Write(r.Out)
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: %v\n", err)
			return 1
		}
		for _, n := range r.Notes {
			fmt.Fprintf(stdout, "%s: check: %s\n", r.Name, n)
			notes++
		}
	}
	fmt.Fprintf(stdout, "%d converted, %d lines to check by hand\n", len(results), notes)
	return 0
}

// projectFiles lists the project's files as slash paths, skipping VCS and dependency folders.
func projectFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out, err
}

func cmdInit(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("init", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "project root")
	force := fl.Bool("force", false, "overwrite existing shepherd.toml")
	name := fl.String("name", "", "project name")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	report, err := initcmd.Run(*root, initcmd.Options{
		Force:       *force,
		ProjectName: *name,
	})
	if err != nil {
		fmt.Fprintf(stderr, "shepherd init: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, report)
	return 0
}

func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "project root")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	checks, allOk := doctor.Check(*root)
	for _, c := range checks {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
		}
		if c.Passed {
			fmt.Fprintf(stdout, "[%s] %s: %s\n", status, c.Name, c.Detail)
		} else {
			fmt.Fprintf(stdout, "[%s] %s: %s\n  Fix: %s\n", status, c.Name, c.Detail, c.Fix)
		}
	}
	if !allOk {
		return 1
	}
	return 0
}
