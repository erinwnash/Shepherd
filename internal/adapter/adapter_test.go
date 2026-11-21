package adapter

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScrubEnv(t *testing.T) {
	hostEnv := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/user",
		"USER=user",
		"AWS_SECRET_ACCESS_KEY=supersecret",
		"GITHUB_TOKEN=ghp_secret",
		"SSH_AUTH_SOCK=/tmp/ssh-agent.sock",
		"CUSTOM_KEY=custom_value",
	}

	// Extra allows CUSTOM_KEY
	scrubbed := ScrubEnv(hostEnv, []string{"CUSTOM_KEY=custom_value"})

	m := make(map[string]string)
	for _, kv := range scrubbed {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			m[parts[0]] = parts[1]
		}
	}

	// Preserved
	if m["PATH"] != "/usr/bin:/bin" {
		t.Errorf("PATH was not preserved")
	}
	if m["HOME"] != "/home/user" {
		t.Errorf("HOME was not preserved")
	}
	if m["CUSTOM_KEY"] != "custom_value" {
		t.Errorf("CUSTOM_KEY was not preserved")
	}

	// Scrubbed / Blocked
	if _, ok := m["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Errorf("AWS_SECRET_ACCESS_KEY leaked into scrubbed env")
	}
	if _, ok := m["GITHUB_TOKEN"]; ok {
		t.Errorf("GITHUB_TOKEN leaked into scrubbed env")
	}
	if _, ok := m["SSH_AUTH_SOCK"]; ok {
		t.Errorf("SSH_AUTH_SOCK leaked into scrubbed env")
	}
}

func TestArgvConstruction_Agy(t *testing.T) {
	a := NewAgyAdapter()
	argv := a.BuildArgv("do work", RunOptions{
		Model: "sonnet",
	})

	expected := []string{"agy", "-p", "do work", "--model", "sonnet", "--dangerously-skip-permissions"}
	if len(argv) != len(expected) {
		t.Fatalf("got %v, want %v", argv, expected)
	}
	for i := range expected {
		if argv[i] != expected[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], expected[i])
		}
	}
}

func TestArgvConstruction_Claude(t *testing.T) {
	a := NewClaudeAdapter()
	argv := a.BuildArgv("implement feature", RunOptions{
		Model:  "claude-3-7-sonnet",
		Effort: "high",
	})

	// Must include effort and dangerous skip permissions
	foundEffort := false
	foundSkip := false
	for i, arg := range argv {
		if arg == "--effort" && i+1 < len(argv) && argv[i+1] == "high" {
			foundEffort = true
		}
		if arg == "--dangerously-skip-permissions" {
			foundSkip = true
		}
	}
	if !foundEffort {
		t.Errorf("expected --effort high in claude argv: %v", argv)
	}
	if !foundSkip {
		t.Errorf("expected --dangerously-skip-permissions in claude argv: %v", argv)
	}
}

func TestArgvConstruction_Codex(t *testing.T) {
	a := NewCodexAdapter()
	argv := a.BuildArgv("prompt via stdin", RunOptions{
		Model:  "gpt-5.6-luna",
		Effort: "medium",
	})

	// Codex takes prompt on stdin, so the last argument is "-"
	if len(argv) == 0 || argv[len(argv)-1] != "-" {
		t.Errorf("expected codex argv to end with '-' (stdin): %v", argv)
	}

	foundSandboxBypass := false
	for _, arg := range argv {
		if arg == "--dangerously-bypass-approvals-and-sandbox" {
			foundSandboxBypass = true
		}
	}
	if !foundSandboxBypass {
		t.Errorf("expected --dangerously-bypass-approvals-and-sandbox in codex argv: %v", argv)
	}
}

func TestArgvConstruction_Opencode(t *testing.T) {
	a := NewOpenCodeAdapter()
	argv := a.BuildArgv("local task", RunOptions{})

	expected := []string{"opencode", "run", "--auto", "local task"}
	if len(argv) != len(expected) {
		t.Fatalf("got %v, want %v", argv, expected)
	}
	for i := range expected {
		if argv[i] != expected[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], expected[i])
		}
	}
}

func TestArgvConstruction_Aider(t *testing.T) {
	a := NewAiderAdapter()
	argv := a.BuildArgv("refactor module", RunOptions{Model: "gpt-4o"})

	expected := []string{"aider", "--yes", "--no-auto-commits", "--message", "refactor module", "--model", "gpt-4o"}
	if len(argv) != len(expected) {
		t.Fatalf("got %v, want %v", argv, expected)
	}
	for i := range expected {
		if argv[i] != expected[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], expected[i])
		}
	}
}

func TestArgvConstruction_Gemini(t *testing.T) {
	a := NewGeminiAdapter()
	argv := a.BuildArgv("audit security", RunOptions{Model: "gemini-1.5-pro"})

	expected := []string{"gemini", "-p", "audit security", "--model", "gemini-1.5-pro"}
	if len(argv) != len(expected) {
		t.Fatalf("got %v, want %v", argv, expected)
	}
	for i := range expected {
		if argv[i] != expected[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], expected[i])
		}
	}
}

func TestAdapterRegistry(t *testing.T) {
	names := []string{"agy", "claude-cli", "codex", "openai-compat", "opencode", "aider", "gemini-cli", "gemini"}
	for _, name := range names {
		ad, err := Get(name)
		if err != nil {
			t.Errorf("Get(%q) failed: %v", name, err)
		}
		if ad == nil {
			t.Errorf("Get(%q) returned nil adapter", name)
		}
	}

	_, err := Get("unsupported-adapter")
	if err == nil {
		t.Errorf("expected error for unsupported adapter")
	}
}

func TestRun_Timeout(t *testing.T) {
	// Custom mock-like adapter runner testing timeout
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	cmd := []string{"sleep", "1"}
	_, err := runSubprocess(ctx, ".", cmd, nil, "", 1024*1024, nil, nil)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
}

func TestParseCostAndTokens(t *testing.T) {
	transcript := "Completed task.\nTotal tokens: 14,250\nTotal cost: $0.18\n"
	tokens, cost := ParseCostAndTokens(transcript)
	if tokens != 14250 {
		t.Errorf("got tokens %d, want 14250", tokens)
	}
	if cost != 0.18 {
		t.Errorf("got cost %f, want 0.18", cost)
	}
}

func TestAllowedEnvVarsScrubbing(t *testing.T) {
	hostEnv := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ant-123",
		"OPENAI_API_KEY=sk-proj-456",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
	}
	claude := NewClaudeAdapter()
	scrubbed := ScrubEnv(hostEnv, claude.Capabilities().AllowedEnvVars)
	joined := strings.Join(scrubbed, "\n")
	if !strings.Contains(joined, "ANTHROPIC_API_KEY=sk-ant-123") {
		t.Errorf("expected ANTHROPIC_API_KEY to be allowed for Claude adapter")
	}
	if strings.Contains(joined, "OPENAI_API_KEY") {
		t.Errorf("OPENAI_API_KEY should not be passed to Claude adapter")
	}
	if strings.Contains(joined, "AWS_SECRET_ACCESS_KEY") {
		t.Errorf("AWS_SECRET_ACCESS_KEY should never be passed")
	}

	all := AllAdapters()
	if len(all) < 6 {
		t.Errorf("expected at least 6 adapters in AllAdapters(), got %d", len(all))
	}
}

func TestRunSubprocess_OnOutputSeesBytesAsTheyArrive(t *testing.T) {
	var mu sync.Mutex
	var got strings.Builder
	res, err := runSubprocess(context.Background(), ".", []string{"echo", "hello-from-helper"}, nil, "", 1024*1024, nil, func(p []byte) {
		mu.Lock()
		got.Write(p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(got.String(), "hello-from-helper") || got.String() != res.Output {
		t.Fatalf("tee %q vs output %q", got.String(), res.Output)
	}
}

func TestCodexEffortMapsWorkOrderSizes(t *testing.T) {
	cases := map[string]string{"": "medium", "S": "low", "M": "medium", "L": "high", "s": "low", "xhigh": "xhigh", "High": "high", "bogus": "medium"}
	for in, want := range cases {
		if got := codexEffort(in); got != want {
			t.Errorf("codexEffort(%q) = %q, want %q", in, got, want)
		}
	}
	argv := NewCodexAdapter().BuildArgv("x", RunOptions{Effort: "M"})
	found := false
	for i, a := range argv {
		if a == "-c" && i+1 < len(argv) && argv[i+1] == `model_reasoning_effort="medium"` {
			found = true
		}
	}
	if !found {
		t.Errorf("argv %v does not carry model_reasoning_effort=\"medium\"", argv)
	}
}

func TestOpenCodeConfigIsReadOnlyToTheSandbox(t *testing.T) {
	caps := NewOpenCodeAdapter().Capabilities()
	has := func(list []string, want string) bool {
		for _, p := range list {
			if p == want {
				return true
			}
		}
		return false
	}
	if !has(caps.ReadOnlyPaths, ".config/opencode") {
		t.Errorf("opencode config must be read-only to the sandbox, ReadOnlyPaths = %v", caps.ReadOnlyPaths)
	}
	if has(caps.Paths, ".config/opencode") {
		t.Errorf("opencode config must not also be writable, Paths = %v", caps.Paths)
	}
	if !has(caps.Paths, ".local/share/opencode") {
		t.Errorf("opencode still needs its data dir writable, Paths = %v", caps.Paths)
	}
}

func TestArgvConstruction_Hermes(t *testing.T) {
	a := NewHermesAdapter()
	argv := a.BuildArgv("do work", RunOptions{Model: "hermes-4"})
	expected := []string{"hermes", "chat", "--oneshot", "-m", "hermes-4", "-q", "do work"}
	if strings.Join(argv, "\x00") != strings.Join(expected, "\x00") {
		t.Fatalf("got %v, want %v", argv, expected)
	}
	if got := a.BuildArgv("x", RunOptions{}); len(got) != 5 {
		t.Fatalf("no model: got %v", got)
	}
}

func TestArgvConstruction_Pi(t *testing.T) {
	a := NewPiAdapter()
	argv := a.BuildArgv("do work", RunOptions{Model: "anthropic/sonnet"})
	expected := []string{"pi", "-p", "--model", "anthropic/sonnet", "do work"}
	if strings.Join(argv, "\x00") != strings.Join(expected, "\x00") {
		t.Fatalf("got %v, want %v", argv, expected)
	}
	if got := a.BuildArgv("x", RunOptions{}); len(got) != 3 {
		t.Fatalf("no model: got %v", got)
	}
}

func TestRegistry_HermesAndPi(t *testing.T) {
	for _, name := range []string{"hermes", "hermes-agent", "pi", "pi-coding-agent"} {
		if _, err := Get(name); err != nil {
			t.Errorf("Get(%q): %v", name, err)
		}
	}
	if _, ok := AllAdapters()["hermes"]; !ok {
		t.Error("hermes missing from AllAdapters")
	}
	if _, ok := AllAdapters()["pi"]; !ok {
		t.Error("pi missing from AllAdapters")
	}
}
