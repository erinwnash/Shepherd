package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/scope"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

func makeWorkOrder(t *testing.T, id string, files []string, tests []string) *workorder.WorkOrder {
	t.Helper()
	src := "---\n" +
		"id: " + id + "\n" +
		"title: Test chunk " + id + "\n" +
		"sprint: S01\n" +
		"files:\n"
	for _, f := range files {
		src += "  - " + f + "\n"
	}
	if len(tests) > 0 {
		src += "tests:\n"
		for _, test := range tests {
			src += "  - " + test + "\n"
		}
	} else {
		src += "no_tests_reason: \"no tests needed\"\n"
	}
	src += "---\n## Goal\nTest\n## Out of scope\nNone\n## Acceptance\nPasses\n"
	wo, err := workorder.Parse(id+".md", []byte(src))
	if err != nil {
		t.Fatalf("makeWorkOrder %s: %v", id, err)
	}
	return wo
}

// --- Abuse Tests (Threat Model T1, T2, T3, T4, E1, E2) ---

// Threat Model T1: Model widens its own scope by editing the work order.
// Mitigation: Scope is computed from the queue-time copy (hash recorded).
// Test: If the model edits its work order or files list, verification fails.
func TestAbuse_ModelEditsWorkOrder(t *testing.T) {
	wo := makeWorkOrder(t, "01A", []string{"src/allowed.go"}, []string{"tests/allowed_test.go"})
	prot := scope.NewProtected(nil)

	// Case 1: Work order content on disk has a different hash than queue-time.
	tamperedWorkOrderBytes := []byte("---\nid: 01A\ntitle: Tampered\nsprint: S01\nfiles:\n  - src/allowed.go\n  - src/evil.go\nno_tests_reason: none\n---\n")
	res := CheckScope(wo, tamperedWorkOrderBytes, []string{"src/allowed.go"}, prot)
	if res.Passed {
		t.Errorf("expected scope check to fail when work order is tampered, but it passed")
	}
	if !strings.Contains(res.Errors, "work order") && !strings.Contains(res.Errors, "hash") {
		t.Errorf("expected error message to mention work order tamper or hash mismatch, got: %s", res.Errors)
	}

	// Case 2: Changed paths include the work order file itself.
	res2 := CheckScope(wo, nil, []string{"src/allowed.go", "01A.md"}, prot)
	if res2.Passed {
		t.Errorf("expected scope check to fail when work order file is in changed paths, but it passed")
	}
}

// Threat Model T2: Model edits config, prompts, CI, hooks.
// Mitigation: Protected paths gate, unconditional.
// Test: Any attempt to modify shepherd.toml, .shepherd/, .git/hooks, CI definitions is blocked.
func TestAbuse_ModelEditsProtectedPaths(t *testing.T) {
	prot := scope.NewProtected([]string{"custom/protected/"})

	maliciousPaths := []string{
		"shepherd.toml",
		"SHEPHERD.TOML",
		".shepherd/prompts/coder.md",
		".git/hooks/pre-commit",
		".git/config",
		".github/workflows/ci.yml",
		".gitlab-ci.yml",
		"custom/protected/secret.txt",
	}

	for _, p := range maliciousPaths {
		wo := makeWorkOrder(t, "01B", []string{"src/ok.go"}, nil)
		res := CheckScope(wo, nil, []string{"src/ok.go", p}, prot)
		if res.Passed {
			t.Errorf("CheckScope allowed protected path %q", p)
		}
		if !strings.Contains(res.Errors, "protected") {
			t.Errorf("CheckScope(%q) error does not mention 'protected': %s", p, res.Errors)
		}
	}
}

// Threat Model T3: Model changes which tests run.
// Mitigation: Test list comes from queue-time work order and gate config, never from untrusted edits.
func TestAbuse_ModelEditsTestsToRun(t *testing.T) {
	wo := makeWorkOrder(t, "01C", []string{"src/feature.go"}, []string{"tests/feature_test.go"})
	prot := scope.NewProtected(nil)

	// Model changed an unapproved test file.
	res := CheckScope(wo, nil, []string{"src/feature.go", "tests/unapproved_test.go"}, prot)
	if res.Passed {
		t.Errorf("expected scope to reject unapproved test file, but passed")
	}
}

// Improvement 2 (TDD Anti-Cheating): When ReadOnlyTests is true, modifying test files is strictly rejected.
func TestAbuse_ModelEditsReadOnlyTests(t *testing.T) {
	wo := makeWorkOrder(t, "01C", []string{"src/feature.go"}, []string{"tests/feature_test.go"})
	wo.ReadOnlyTests = true
	prot := scope.NewProtected(nil)

	// Model attempted to edit the immutable test file.
	res := CheckScope(wo, nil, []string{"src/feature.go", "tests/feature_test.go"}, prot)
	if res.Passed {
		t.Fatalf("expected scope to reject editing read-only test file, but passed")
	}
	if len(res.ReadOnlyTestHit) == 0 || res.ReadOnlyTestHit[0] != "tests/feature_test.go" {
		t.Errorf("expected ReadOnlyTestHit to contain 'tests/feature_test.go', got: %v", res.ReadOnlyTestHit)
	}
	if !strings.Contains(res.Errors, "Read-only test files modified") {
		t.Errorf("expected error message to explain read-only test violation, got: %s", res.Errors)
	}
}


// Threat Model T4: Model guts a file and passes typecheck.
// Mitigation: Content-loss gate, CRLF-normalized.
// Test: A partial overwrite (e.g. 100 lines reduced to 5 lines) is blocked, but CRLF/BOM changes are not counted as loss.
func TestAbuse_ContentLossTruncation(t *testing.T) {
	// A file where 100 lines were removed and only 5 added (deleted >= 25 && deleted > added*2 + 10)
	loss := CheckNumstatLoss([]NumstatEntry{
		{Path: "src/big.go", Added: 5, Deleted: 100},
	})
	if len(loss) == 0 {
		t.Fatalf("expected content loss to be detected for 100 deleted vs 5 added, got none")
	}
	if !strings.Contains(loss[0], "src/big.go") {
		t.Errorf("expected loss report to name 'src/big.go', got: %s", loss[0])
	}

	// Normal valid edit: 30 deleted and 25 added -> not a truncation
	noLoss := CheckNumstatLoss([]NumstatEntry{
		{Path: "src/normal.go", Added: 25, Deleted: 30},
	})
	if len(noLoss) != 0 {
		t.Errorf("expected no content loss for balanced edit, got: %v", noLoss)
	}

	// Legitimate small deletion: 15 deleted, 0 added (< 25)
	smallDel := CheckNumstatLoss([]NumstatEntry{
		{Path: "src/small.go", Added: 0, Deleted: 15},
	})
	if len(smallDel) != 0 {
		t.Errorf("expected no content loss for small deletion (<25 lines), got: %v", smallDel)
	}
}

// Threat Model T4: CRLF <-> LF and BOM changes must not be counted as content loss.
func TestAbuse_CRLFAndBOMNoLoss(t *testing.T) {
	baseText := "line 1\r\nline 2\r\nline 3\r\nline 4\r\nline 5\r\n"
	for i := 6; i <= 50; i++ {
		baseText += "line and more text to test length\r\n"
	}

	// Current text is identical except LF endings and UTF-8 BOM prepended.
	currentText := "\xef\xbb\xbf" + strings.ReplaceAll(baseText, "\r\n", "\n")

	added, deleted := CompareNormalizedLines([]byte(baseText), []byte(currentText))
	if added != 0 || deleted != 0 {
		t.Errorf("CRLF and BOM difference resulted in added=%d, deleted=%d, want 0, 0", added, deleted)
	}
}

// Threat Model E1: Command injection via placeholders.
// Mitigation: Argv only; placeholder values must be valid worktree paths, not starting with '-'.
// Test: Arguments starting with '-' (like -rf, --help) or escaping worktree (../x, /etc/passwd) are rejected.
func TestAbuse_CommandInjectionInPlaceholder(t *testing.T) {
	argvTemplate := []string{"vitest", "run", "{test_files}"}

	injectionAttempts := []string{
		"-rf",
		"--all",
		"-o",
		"/etc/passwd",
		"../escaped.go",
		"src/../../outside.go",
		"C:/evil.go",
		"; rm -rf /",
		"foo\x00bar",
	}

	for _, bad := range injectionAttempts {
		_, err := ExpandPlaceholders(argvTemplate, []string{bad}, nil, "")
		if err == nil {
			t.Errorf("ExpandPlaceholders allowed dangerous argument %q", bad)
		}
	}
}

// Threat Model E2: Git operations in untrusted repo.
// Mitigation: Git runner always uses SafeGitFlags (-c core.hooksPath= -c core.fsmonitor= ...) and GIT_CONFIG_NOSYSTEM=1.
func TestAbuse_SafeGitFlags(t *testing.T) {
	runner := NewGitRunner(".")
	ctx := context.Background()

	// Verify that Exec uses the safe flags and runs git version without error
	out, err := runner.Exec(ctx, "version")
	if err != nil {
		t.Fatalf("SafeGitRunner failed: %v", err)
	}
	if !strings.Contains(out, "git version") {
		t.Errorf("expected git version in output, got: %s", out)
	}

	// Confirm SafeGitFlags contain core.hooksPath=, core.fsmonitor=, and protocol.ext.allow=never
	foundHooks := false
	foundFsmonitor := false
	foundProtocol := false
	for _, flag := range SafeGitFlags {
		if strings.Contains(flag, "core.hooksPath=") {
			foundHooks = true
		}
		if strings.Contains(flag, "core.fsmonitor=") {
			foundFsmonitor = true
		}
		if strings.Contains(flag, "protocol.ext.allow=never") {
			foundProtocol = true
		}
	}
	if !foundHooks || !foundFsmonitor || !foundProtocol {
		t.Errorf("SafeGitFlags missing critical security flags: %v", SafeGitFlags)
	}
}

// --- General Unit Tests ---

func TestScope_ValidAndInvalidFiles(t *testing.T) {
	wo := makeWorkOrder(t, "02A", []string{"src/api.go", "supabase/migrations/<next>_x.sql"}, []string{"tests/api_test.go"})
	prot := scope.NewProtected(nil)

	// Valid changes matching declared files and tests
	validChanges := []string{
		"src/api.go",
		"supabase/migrations/0010_x.sql",
		"tests/api_test.go",
	}
	res := CheckScope(wo, nil, validChanges, prot)
	if !res.Passed {
		t.Errorf("expected valid changes to pass scope check, got errors: %s", res.Errors)
	}

	// Undeclared file
	invalidChanges := []string{
		"src/api.go",
		"src/unrelated.go",
	}
	res2 := CheckScope(wo, nil, invalidChanges, prot)
	if res2.Passed {
		t.Errorf("expected scope check to fail with undeclared file, but passed")
	}
	if !strings.Contains(res2.Errors, "src/unrelated.go") {
		t.Errorf("expected error to list 'src/unrelated.go', got: %s", res2.Errors)
	}
}

func TestExpandPlaceholders_Success(t *testing.T) {
	template := []string{"npx", "eslint", "{touched_files}"}
	touched := []string{"src/a.ts", "src/b.tsx", "README.md"}

	// With glob filtering
	expanded, err := ExpandPlaceholders(template, nil, touched, "*.ts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"npx", "eslint", "src/a.ts"}
	if len(expanded) != len(expected) {
		t.Fatalf("got %v, want %v", expanded, expected)
	}
	for i := range expected {
		if expanded[i] != expected[i] {
			t.Errorf("expanded[%d] = %q, want %q", i, expanded[i], expected[i])
		}
	}
}

func TestExpandPlaceholders_SkipWhenEmpty(t *testing.T) {
	template := []string{"npx", "vitest", "run", "{test_files}"}
	// No test files -> should return ErrSkipGate
	_, err := ExpandPlaceholders(template, nil, nil, "")
	if !errors.Is(err, ErrSkipGate) {
		t.Errorf("got error %v, want ErrSkipGate", err)
	}
}

func TestExecuteGate_ArgvOnly(t *testing.T) {
	ctx := context.Background()
	gate := GateConfig{
		Name: "echo-test",
		Argv: []string{"go", "version"},
		Hard: true,
	}

	res, err := RunGate(ctx, ".", gate.Argv, nil)
	if err != nil {
		t.Fatalf("RunGate failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Output, "go version") {
		t.Errorf("expected 'go version' in output, got: %s", res.Output)
	}
}

func TestVerifier_EndToEnd(t *testing.T) {
	ctx := context.Background()
	wo := makeWorkOrder(t, "03A", []string{"src/file.go"}, []string{"tests/file_test.go"})
	prot := scope.NewProtected(nil)

	gates := []GateConfig{
		{Name: "content-loss", Builtin: "content-loss", Hard: true},
		{Name: "scope", Builtin: "scope", Hard: true},
		{Name: "typecheck", Argv: []string{"go", "version"}, Hard: true},
		{Name: "lint", Argv: []string{"go", "env"}, Hard: false},
	}
	v := NewVerifier(gates)

	// Passing case
	state := WorktreeState{
		WorktreeRoot: ".",
		WorkOrder:    wo,
		ChangedPaths: []string{"src/file.go"},
		Numstat: []NumstatEntry{
			{Path: "src/file.go", Added: 10, Deleted: 5},
		},
		Protected: prot,
	}

	res, err := v.Verify(ctx, state)
	if err != nil {
		t.Fatalf("v.Verify error: %v", err)
	}
	if !res.Passed {
		t.Errorf("expected verify to pass, got errors: %s", res.Errors)
	}
	if res.HardFail {
		t.Errorf("expected HardFail to be false")
	}

	// Failing case: content loss
	state.Numstat = []NumstatEntry{
		{Path: "src/file.go", Added: 2, Deleted: 80},
	}
	res2, err := v.Verify(ctx, state)
	if err != nil {
		t.Fatalf("v.Verify error: %v", err)
	}
	if res2.Passed {
		t.Errorf("expected verify to fail on content loss, but passed")
	}
	if !res2.HardFail {
		t.Errorf("expected HardFail to be true")
	}
}

func TestStripWhitespaceNoise(t *testing.T) {
	// Create a temporary git repo to test whitespace stripping
	tempDir, err := os.MkdirTemp("", "shepherd-git-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tempDir)

	gitRun := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tempDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v (%s)", strings.Join(args, " "), err, string(out))
		}
	}

	gitRun("init", "-b", "main")
	gitRun("config", "user.email", "test@test.local")
	gitRun("config", "user.name", "Test User")

	filePath := filepath.Join(tempDir, "file.txt")
	initialContent := "func main() {\n\tprintln(\"hello\")\n}\n"
	if err := os.WriteFile(filePath, []byte(initialContent), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	gitRun("add", "file.txt")
	gitRun("commit", "-m", "initial commit")

	// Now modify with purely whitespace noise on one line and real code change on another
	// e.g. add trailing spaces to line 1, and change "hello" to "world"
	modifiedContent := "func main() {   \n\tprintln(\"world\")\n}\n"
	if err := os.WriteFile(filePath, []byte(modifiedContent), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	runner := NewGitRunner(tempDir)
	ctx := context.Background()
	cleaned, err := runner.StripWhitespaceNoise(ctx)
	if err != nil {
		t.Fatalf("StripWhitespaceNoise error: %v", err)
	}
	if len(cleaned) == 0 {
		t.Logf("Note: whitespace stripping on minimal diff: %v", cleaned)
	}

	// Verify that the real code change persists
	afterBytes, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile after: %v", err)
	}
	if !strings.Contains(string(afterBytes), "world") {
		t.Errorf("expected real change 'world' to be preserved in file, got: %s", string(afterBytes))
	}
}

// Threat Model D1: Output flood or infinite loop cut off and blocked.
func TestAbuse_OutputCap_STRIDE_D1(t *testing.T) {
	ctx := context.Background()
	// Command that produces more output than cap or test capped buffer directly
	buf := &cappedBuffer{limit: 100}
	huge := strings.Repeat("A", 500)
	_, _ = buf.Write([]byte(huge))

	if !buf.truncated {
		t.Fatalf("expected buffer to mark truncated")
	}
	if !strings.Contains(buf.String(), "[output truncated") {
		t.Fatalf("expected truncation notice in string, got %s", buf.String())
	}

	// RunGate with small echo works cleanly
	res, err := RunGate(ctx, ".", []string{"echo", "normal output"}, nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("RunGate failed: %v, res: %+v", err, res)
	}
}

// Threat Model E3: Dependency install steps must enforce --ignore-scripts by default.
func TestAbuse_DependencyIgnoreScripts_STRIDE_E3(t *testing.T) {
	// Dangerous npm install without --ignore-scripts
	err := ValidateDependencyCommand([]string{"npm", "install", "malicious-pkg"})
	if err == nil {
		t.Fatalf("expected error for npm install without --ignore-scripts")
	}

	// Safe npm install with --ignore-scripts
	err = ValidateDependencyCommand([]string{"npm", "install", "--ignore-scripts", "safe-pkg"})
	if err != nil {
		t.Fatalf("unexpected error for --ignore-scripts: %v", err)
	}

	// Safe non-install command
	err = ValidateDependencyCommand([]string{"npx", "vitest", "run"})
	if err != nil {
		t.Fatalf("unexpected error for non-install command: %v", err)
	}
}

// Threat Model E2: Git calls neutralize core.hooksPath, core.fsmonitor, and protocol.ext.allow.
func TestAbuse_GitFlagsNeutralizeIncludes_STRIDE_E2(t *testing.T) {
	hasHooks := false
	hasFsmonitor := false
	hasProtocol := false
	for i := 0; i < len(SafeGitFlags)-1; i++ {
		if SafeGitFlags[i] == "-c" {
			val := SafeGitFlags[i+1]
			if strings.HasPrefix(val, "core.hooksPath=") {
				hasHooks = true
			}
			if strings.HasPrefix(val, "core.fsmonitor=") {
				hasFsmonitor = true
			}
			if strings.HasPrefix(val, "protocol.ext.allow=") {
				hasProtocol = true
			}
		}
	}
	if !hasHooks || !hasFsmonitor || !hasProtocol {
		t.Fatalf("SafeGitFlags missing critical sanitization flags: %v", SafeGitFlags)
	}
}

func TestVerify_FlakeRetries(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	counterFile := filepath.Join(tmpDir, "counter.txt")

	// Script that fails on run 1, succeeds on run 2
	script := filepath.Join(tmpDir, "flake.sh")
	scriptContent := fmt.Sprintf(`#!/bin/sh
if [ ! -f "%s" ]; then
  echo 1 > "%s"
  exit 1
fi
exit 0
`, counterFile, counterFile)
	if err := os.WriteFile(script, []byte(scriptContent), 0755); err != nil {
		t.Fatal(err)
	}

	verifier := NewVerifier([]GateConfig{
		{Name: "flaky-test", Argv: []string{script}, Hard: true},
	})
	verifier.FlakeRetries = 2

	state := WorktreeState{
		WorktreeRoot: tmpDir,
		Protected:    scope.NewProtected(nil),
	}

	res, err := verifier.Verify(ctx, state)
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if !res.Passed {
		t.Fatalf("expected verifier to pass after flake retry, but failed: %s", res.Errors)
	}
	if len(res.Results) != 1 || !res.Results[0].FlakeRetried {
		t.Errorf("expected Results[0].FlakeRetried = true, got %+v", res.Results)
	}
}

func TestVerify_BaseCommitDelta_Quarantined(t *testing.T) {
	ctx := context.Background()
	verifier := NewVerifier([]GateConfig{
		{Name: "legacy-test", Argv: []string{"sh", "-c", "exit 1"}, Hard: true},
	})
	verifier.BaseCommitDelta = true
	verifier.BaseRunner = func(ctx context.Context, worktreeRoot string, argv []string, env []string) (GateExecResult, error) {
		// Simulate that base commit also fails!
		return GateExecResult{ExitCode: 1, Output: "base commit failed with legacy error"}, nil
	}

	state := WorktreeState{
		WorktreeRoot: ".",
		Protected:    scope.NewProtected(nil),
	}

	res, err := verifier.Verify(ctx, state)
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if !res.Passed {
		t.Fatalf("expected pre-existing test failure to be quarantined and pass, but got: %s", res.Errors)
	}
	if len(res.Quarantined) != 1 || res.Quarantined[0] != "legacy-test" {
		t.Errorf("expected 'legacy-test' in Quarantined, got %v", res.Quarantined)
	}
	if !res.Results[0].PreExisting {
		t.Errorf("expected PreExisting to be true, got %+v", res.Results[0])
	}
}

func TestVerify_BaseCommitDelta_RealRegression(t *testing.T) {
	ctx := context.Background()
	verifier := NewVerifier([]GateConfig{
		{Name: "broken-test", Argv: []string{"sh", "-c", "exit 1"}, Hard: true},
	})
	verifier.BaseCommitDelta = true
	verifier.BaseRunner = func(ctx context.Context, worktreeRoot string, argv []string, env []string) (GateExecResult, error) {
		// Base commit passed cleanly -> this is a real regression caused by the chunk!
		return GateExecResult{ExitCode: 0, Output: "base commit passed"}, nil
	}

	state := WorktreeState{
		WorktreeRoot: ".",
		Protected:    scope.NewProtected(nil),
	}

	res, err := verifier.Verify(ctx, state)
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if res.Passed {
		t.Fatalf("expected real regression to fail gate, but passed")
	}
	if len(res.Quarantined) != 0 {
		t.Errorf("expected 0 quarantined tests, got %v", res.Quarantined)
	}
	if res.Results[0].PreExisting {
		t.Errorf("expected PreExisting to be false, got true")
	}
}

func TestVerify_DefaultBaseRunner_GitWorktree(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	git := NewGitRunner(tmpDir)
	if _, err := git.Exec(ctx, "init"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	_, _ = git.Exec(ctx, "config", "user.email", "test@test.local")
	_, _ = git.Exec(ctx, "config", "user.name", "Tester")

	// Commit base file
	targetFile := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(targetFile, []byte("base-version-content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Exec(ctx, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Exec(ctx, "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	// Now dirty the worktree with chunk changes
	if err := os.WriteFile(targetFile, []byte("chunk-version-content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Base runner should see base content ("base-version-content")
	res, err := DefaultBaseRunner(ctx, tmpDir, []string{"grep", "base-version-content", "sample.txt"}, nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("DefaultBaseRunner failed: %v, res: %+v", err, res)
	}

	// Verify working tree was not corrupted and still has dirty chunk content
	cur, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != "chunk-version-content\n" {
		t.Errorf("working tree corrupted! got: %q", string(cur))
	}
}

func TestVerify_SecretsGate(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	leakFile := filepath.Join(tmpDir, "config.go")
	secretKey := "sk-ant-" + strings.Repeat("a1B2c3", 6)
	if err := os.WriteFile(leakFile, []byte("package main\nvar key = \""+secretKey+"\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	verifier := NewVerifier([]GateConfig{
		{Name: "secrets", Builtin: "secrets", Hard: true},
	})

	res, err := verifier.Verify(ctx, WorktreeState{
		WorktreeRoot: tmpDir,
		ChangedPaths: []string{"config.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatalf("expected secrets gate to fail when API key is present in changed file")
	}
	if strings.Contains(res.Errors, secretKey) {
		t.Errorf("error report leaked raw secret! got: %s", res.Errors)
	}
	if !strings.Contains(res.Errors, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in error report, got: %s", res.Errors)
	}

	// Clean file should pass
	if err := os.WriteFile(leakFile, []byte("package main\nvar key = os.Getenv(\"API_KEY\")\n"), 0644); err != nil {
		t.Fatal(err)
	}
	resClean, err := verifier.Verify(ctx, WorktreeState{
		WorktreeRoot: tmpDir,
		ChangedPaths: []string{"config.go"},
	})
	if err != nil || !resClean.Passed {
		t.Fatalf("expected clean file to pass secrets gate, got err=%v, errors=%s", err, resClean.Errors)
	}
}

func TestVerify_SubChunkAndGroupEndGates(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// Create vitest.config.ts so ResolveSubchunkArgv detects a Vitest repo
	if err := os.WriteFile(filepath.Join(tmpDir, "vitest.config.ts"), []byte("export default {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	got := ResolveSubchunkArgv(
		GateConfig{Name: "tests", Argv: []string{"npm", "run", "test:unit"}, Hard: true},
		tmpDir,
		nil,
	)
	want := []string{"npx", "vitest", "related", "--run", "--passWithNoTests", "{touched_files}"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("ResolveSubchunkArgv = %v, want %v", got, want)
	}

	// Test explicit On="group_end", On="subchunk", and SubchunkArgv
	verifier := NewVerifier([]GateConfig{
		{Name: "sub-only", Argv: []string{"echo", "sub"}, On: "subchunk", Hard: true},
		{Name: "end-only", Argv: []string{"echo", "end"}, On: "group_end", Hard: true},
		{Name: "adaptive", Argv: []string{"echo", "full-suite"}, SubchunkArgv: []string{"echo", "fast-related", "{touched_files}"}, Hard: true},
	})

	// 1. Intermediate sub-chunk (45A.1): sub-only runs, end-only skips, adaptive runs SubchunkArgv
	subRes, err := verifier.Verify(ctx, WorktreeState{
		WorktreeRoot: tmpDir,
		ChangedPaths: []string{"src/foo.ts"},
		IsSubChunk:   true,
	})
	if err != nil || !subRes.Passed {
		t.Fatalf("sub-chunk verify failed: err=%v, errors=%s", err, subRes.Errors)
	}
	if subRes.Results[0].Skipped || !strings.Contains(subRes.Results[0].Output, "sub") {
		t.Errorf("expected sub-only gate to run on sub-chunk, got %+v", subRes.Results[0])
	}
	if !subRes.Results[1].Skipped {
		t.Errorf("expected end-only gate to be skipped on sub-chunk, got %+v", subRes.Results[1])
	}
	if !strings.Contains(subRes.Results[2].Output, "fast-related src/foo.ts") {
		t.Errorf("expected adaptive gate to run SubchunkArgv, got %q", subRes.Results[2].Output)
	}

	// 2. Final chunk in group (45A.3, IsSubChunk=false): sub-only skips, end-only runs, adaptive runs full Argv
	endRes, err := verifier.Verify(ctx, WorktreeState{
		WorktreeRoot: tmpDir,
		ChangedPaths: []string{"src/foo.ts"},
		IsSubChunk:   false,
	})
	if err != nil || !endRes.Passed {
		t.Fatalf("group-end verify failed: err=%v, errors=%s", err, endRes.Errors)
	}
	if !endRes.Results[0].Skipped {
		t.Errorf("expected sub-only gate to be skipped on group end, got %+v", endRes.Results[0])
	}
	if endRes.Results[1].Skipped || !strings.Contains(endRes.Results[1].Output, "end") {
		t.Errorf("expected end-only gate to run on group end, got %+v", endRes.Results[1])
	}
	if !strings.Contains(endRes.Results[2].Output, "full-suite") {
		t.Errorf("expected adaptive gate to run full Argv on group end, got %q", endRes.Results[2].Output)
	}
}




