package verify

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SafeGitFlags are prepended to every git invocation to neutralize malicious repo-level
// configurations (Threat Model E2: hook execution, malicious fsmonitor, dangerous protocols).
var SafeGitFlags = []string{
	"-c", "core.hooksPath=",
	"-c", "core.fsmonitor=",
	"-c", "protocol.ext.allow=never",
}

// GitRunner executes git commands safely without trusting repo configuration.
type GitRunner struct {
	WorktreeRoot string
}

// NewGitRunner creates a GitRunner for the given worktree directory.
func NewGitRunner(worktreeRoot string) *GitRunner {
	return &GitRunner{WorktreeRoot: worktreeRoot}
}

// Exec runs a git command with SafeGitFlags prepended and GIT_CONFIG_NOSYSTEM=1.
func (g *GitRunner) Exec(ctx context.Context, args ...string) (string, error) {
	fullArgs := append(append([]string{}, SafeGitFlags...), args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Dir = g.WorktreeRoot
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// ChangedPaths returns all modified, newly added, or untracked paths.
func (g *GitRunner) ChangedPaths(ctx context.Context) ([]string, error) {
	out, err := g.Exec(ctx, "status", "--porcelain", "-uall")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) <= 3 {
			continue
		}
		// status output format: "XY path" or "XY old -> new"
		p := strings.TrimSpace(line[3:])
		p = strings.Trim(p, "\"")
		if idx := strings.Index(p, " -> "); idx >= 0 {
			p = p[idx+4:]
		}
		// ignore internal shepherd files
		if strings.HasPrefix(p, ".shepherd/") || p == "shepherd.toml" {
			continue
		}
		paths = append(paths, filepath.ToSlash(p))
	}
	return paths, nil
}

// Numstat returns line addition and deletion counts against HEAD.
func (g *GitRunner) Numstat(ctx context.Context, whitespaceInsensitive bool, file string) ([]NumstatEntry, error) {
	args := []string{"diff"}
	if whitespaceInsensitive {
		args = append(args, "-w")
	}
	args = append(args, "--numstat", "HEAD")
	if file != "" {
		args = append(args, "--", file)
	}

	out, err := g.Exec(ctx, args...)
	if err != nil {
		return nil, err
	}

	var entries []NumstatEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		p := filepath.ToSlash(parts[2])
		if parts[0] == "-" || parts[1] == "-" {
			entries = append(entries, NumstatEntry{Path: p, Binary: true})
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		entries = append(entries, NumstatEntry{
			Path:    p,
			Added:   added,
			Deleted: deleted,
		})
	}
	return entries, nil
}

// StripWhitespaceNoise cleans whitespace-only edits in tracked files.
func (g *GitRunner) StripWhitespaceNoise(ctx context.Context) ([]string, error) {
	diffNames, err := g.Exec(ctx, "diff", "--name-only", "HEAD")
	if err != nil {
		return nil, err
	}

	var cleaned []string
	for _, file := range strings.Split(diffNames, "\n") {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		filePath := filepath.Join(g.WorktreeRoot, file)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			continue
		}

		rawStat, err := g.Numstat(ctx, false, file)
		if err != nil || len(rawStat) == 0 {
			continue
		}
		wsStat, err := g.Numstat(ctx, true, file)
		if err != nil {
			continue
		}

		rawAdded := rawStat[0].Added
		wsAdded := 0
		if len(wsStat) > 0 {
			wsAdded = wsStat[0].Added
		}

		if rawStat[0].Binary || !(wsAdded < rawAdded) {
			continue
		}

		patch, err := g.Exec(ctx, "diff", "-w", "--no-color", "HEAD", "--", file)
		if err != nil {
			continue
		}

		backup, readErr := os.ReadFile(filePath)
		if readErr != nil {
			continue
		}

		// Revert to HEAD first
		_, checkoutErr := g.Exec(ctx, "checkout", "-q", "HEAD", "--", file)
		if checkoutErr != nil {
			continue
		}

		if strings.TrimSpace(patch) == "" {
			// Entire diff was whitespace noise; checkout to HEAD was all that was needed
			cleaned = append(cleaned, file)
			continue
		}

		// Apply patch containing only the real changes
		tmpPatch := filepath.Join(os.TempDir(), fmt.Sprintf("shepherd-patch-%d-%s.patch", os.Getpid(), filepath.Base(file)))
		if writeErr := os.WriteFile(tmpPatch, []byte(patch), 0600); writeErr != nil {
			_ = os.WriteFile(filePath, backup, 0644)
			continue
		}

		_, applyErr := g.Exec(ctx, "apply", "--ignore-whitespace", "--recount", tmpPatch)
		_ = os.Remove(tmpPatch)

		if applyErr == nil {
			cleaned = append(cleaned, file)
		} else {
			// Restore original backup if patch application failed
			_ = os.WriteFile(filePath, backup, 0644)
		}
	}

	return cleaned, nil
}

// DefaultBaseRunner runs the command argv in a temporary detached worktree at HEAD
// to check if a test failure already existed prior to chunk modifications.
// It places the temporary worktree under <repoRoot>/.shepherd/ when possible so tools
// that resolve dependencies upward (e.g. node_modules) still find the main tree's.
func DefaultBaseRunner(ctx context.Context, worktreeRoot string, argv []string, env []string) (GateExecResult, error) {
	parentDir := ""
	if worktreeRoot != "" {
		absRoot, err := filepath.Abs(worktreeRoot)
		if err == nil {
			shepherdDir := filepath.Join(absRoot, ".shepherd")
			if filepath.Base(filepath.Dir(absRoot)) == ".shepherd" {
				shepherdDir = filepath.Dir(absRoot)
			}
			if err := os.MkdirAll(shepherdDir, 0755); err == nil {
				parentDir = shepherdDir
			}
		}
	}
	tempDir, err := os.MkdirTemp(parentDir, "shepherd-base-delta-*")
	if err != nil {
		return GateExecResult{}, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	git := NewGitRunner(worktreeRoot)
	_, err = git.Exec(ctx, "worktree", "add", "--detach", tempDir, "HEAD")
	if err != nil {
		return GateExecResult{}, fmt.Errorf("git worktree add: %w", err)
	}
	defer func() {
		_, _ = git.Exec(ctx, "worktree", "remove", "--force", tempDir)
	}()

	return RunGate(ctx, tempDir, argv, env)
}

