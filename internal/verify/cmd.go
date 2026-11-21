package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/erinwnash/Shepherd/internal/scope"
)

// ErrSkipGate is returned when a placeholder (such as {test_files}) expands to an empty set,
// signalling that the gate has no targets to run on and should be skipped cleanly.
var ErrSkipGate = errors.New("gate skipped: no matching files for placeholder")

// DefaultMaxGateOutput caps gate command output to 10 MiB per STRIDE D1.
const DefaultMaxGateOutput = 10 * 1024 * 1024

// GateExecResult holds the output and exit status of a gate execution.
type GateExecResult struct {
	ExitCode int
	Output   string
}

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (n int, err error) {
	if c.buf.Len() >= c.limit {
		c.truncated = true
		return len(p), nil
	}
	remaining := c.limit - c.buf.Len()
	if len(p) > remaining {
		_, _ = c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string {
	s := c.buf.String()
	if c.truncated {
		s += "\n[output truncated: exceeded 10 MiB limit (STRIDE D1)]"
	}
	return s
}

// ExpandPlaceholders expands allow-listed placeholders ({test_files}, {touched_files}) into
// argv arrays. Each substituted value is strictly validated (ADR-0004, Threat Model E1):
// - Must be a canonical worktree path (scope.ValidatePattern)
// - Must not start with '-' (guards against argument injection)
// - Must match the glob filter if provided
func ExpandPlaceholders(argv []string, tests []string, touched []string, glob string) ([]string, error) {
	hasTestPlaceholder := false
	hasTouchedPlaceholder := false

	for _, arg := range argv {
		if arg == "{test_files}" {
			hasTestPlaceholder = true
		} else if arg == "{touched_files}" {
			hasTouchedPlaceholder = true
		} else if strings.Contains(arg, "{test_files}") || strings.Contains(arg, "{touched_files}") {
			return nil, fmt.Errorf("placeholders must be distinct argv elements, not substrings: %q", arg)
		}
	}

	// Validate test files if needed
	var validTests []string
	if hasTestPlaceholder {
		if len(tests) == 0 {
			return nil, ErrSkipGate
		}
		for _, t := range tests {
			if strings.HasPrefix(t, "-") {
				return nil, fmt.Errorf("argument injection: path %q starts with '-'", t)
			}
			if err := scope.ValidatePattern(t); err != nil {
				return nil, fmt.Errorf("invalid test path %q: %w", t, err)
			}
			validTests = append(validTests, t)
		}
	}

	// Filter and validate touched files if needed
	var validTouched []string
	if hasTouchedPlaceholder {
		for _, p := range touched {
			if glob != "" {
				matched := matchGlob(glob, p)
				if !matched {
					continue
				}
			}
			if strings.HasPrefix(p, "-") {
				return nil, fmt.Errorf("argument injection: path %q starts with '-'", p)
			}
			if err := scope.ValidatePattern(p); err != nil {
				return nil, fmt.Errorf("invalid touched path %q: %w", p, err)
			}
			validTouched = append(validTouched, p)
		}
		if len(validTouched) == 0 {
			return nil, ErrSkipGate
		}
	}

	// Build expanded argv
	var out []string
	for _, arg := range argv {
		switch arg {
		case "{test_files}":
			out = append(out, validTests...)
		case "{touched_files}":
			out = append(out, validTouched...)
		default:
			out = append(out, arg)
		}
	}

	return out, nil
}

func matchGlob(pattern, p string) bool {
	// If pattern contains '/', match against full path, otherwise match against basename
	target := p
	if !strings.Contains(pattern, "/") {
		target = path.Base(p)
	}
	matched, err := filepath.Match(pattern, target)
	return err == nil && matched
}

// ResolveSubchunkArgv returns the argv to run for a gate when verifying an intermediate
// sub-chunk (e.g. 45A.1 or 45A.2 before the final 45A.3 boundary chunk).
// If SubchunkArgv is explicitly configured on the gate, it takes precedence. Otherwise,
// full-suite Vitest / Go test gates automatically narrow to related/declared tests on
// intermediate sub-chunks while the full suite runs at the group boundary.
func ResolveSubchunkArgv(gate GateConfig, worktreeRoot string, tests []string) []string {
	if len(gate.SubchunkArgv) > 0 {
		return gate.SubchunkArgv
	}
	for _, arg := range gate.Argv {
		if arg == "{test_files}" || arg == "{touched_files}" {
			return gate.Argv
		}
	}
	if isFullSuiteNodeTestGate(gate.Argv) && hasVitestConfig(worktreeRoot) {
		return []string{"npx", "vitest", "related", "--run", "--passWithNoTests", "{touched_files}"}
	}
	if isFullSuiteGoTestGate(gate.Argv) && len(tests) > 0 {
		return []string{"go", "test", "{test_files}"}
	}
	return gate.Argv
}

func isFullSuiteNodeTestGate(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	bin := strings.TrimSuffix(filepath.Base(argv[0]), ".cmd")
	rest := strings.Join(argv[1:], " ")
	switch bin {
	case "npm", "pnpm", "yarn", "bun":
		switch rest {
		case "test", "run test", "run test:unit", "test:unit":
			return true
		}
	case "npx":
		if rest == "vitest" || rest == "vitest run" {
			return true
		}
	case "vitest":
		if rest == "" || rest == "run" {
			return true
		}
	}
	return false
}

func isFullSuiteGoTestGate(argv []string) bool {
	return len(argv) == 3 && filepath.Base(argv[0]) == "go" && argv[1] == "test" && argv[2] == "./..."
}

func hasVitestConfig(worktreeRoot string) bool {
	if worktreeRoot == "" {
		return false
	}
	for _, name := range []string{
		"vitest.config.ts",
		"vitest.config.mts",
		"vitest.config.cts",
		"vitest.config.js",
		"vitest.config.mjs",
		"vitest.config.cjs",
	} {
		if _, err := os.Stat(filepath.Join(worktreeRoot, name)); err == nil {
			return true
		}
	}
	return false
}

// RunGate executes an argv command directly without a shell (Threat Model B3, ADR-0004).
func RunGate(ctx context.Context, dir string, argv []string, env []string) (GateExecResult, error) {
	if len(argv) == 0 {
		return GateExecResult{}, errors.New("empty argv")
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = env
	}

	combined := &cappedBuffer{limit: DefaultMaxGateOutput}
	cmd.Stdout = combined
	cmd.Stderr = combined

	err := cmd.Run()
	outStr := combined.String()

	if combined.truncated {
		return GateExecResult{ExitCode: 1, Output: outStr}, errors.New("gate output exceeded 10 MiB limit (STRIDE D1)")
	}

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return GateExecResult{ExitCode: -1, Output: outStr}, err
		}
	}

	return GateExecResult{
		ExitCode: exitCode,
		Output:   outStr,
	}, nil
}

// ValidateDependencyCommand ensures package installation commands enforce --ignore-scripts (STRIDE E3).
func ValidateDependencyCommand(argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	bin := filepath.Base(argv[0])
	isInstall := false
	switch bin {
	case "npm", "pnpm", "yarn", "bun":
		for _, arg := range argv[1:] {
			if arg == "install" || arg == "i" || arg == "add" {
				isInstall = true
				break
			}
		}
	}
	if !isInstall {
		return nil
	}

	hasIgnore := false
	for _, arg := range argv[1:] {
		if arg == "--ignore-scripts" {
			hasIgnore = true
			break
		}
	}
	if !hasIgnore {
		return errors.New("dependency install commands must specify '--ignore-scripts' by default (STRIDE E3)")
	}
	return nil
}
