// Package adapter provides plugins for agent CLIs (Claude Code, Antigravity, Codex, local models).
// Built from scratch: pure argv execution, environment scrubbing, and output caps (STRIDE B3, I2, D1).
package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/erinwnash/Shepherd/internal/sandbox"
)

// DefaultMaxOutputBytes caps subprocess transcript output at 10 MiB to prevent memory exhaustion (STRIDE D1).
const DefaultMaxOutputBytes = 10 * 1024 * 1024

// RunOptions configures one execution of an agent CLI.
type RunOptions struct {
	WorktreeRoot   string
	Model          string
	Effort         string
	SystemPrompt   string
	Timeout        time.Duration
	Env            []string
	MaxOutputBytes int
	Sandbox        *sandbox.Profile
	// OnOutput, if set, is handed each piece of stdout/stderr as the subprocess writes it, so a
	// caller can follow progress live. It must not block; the final RunResult.Output is unaffected.
	OnOutput func(p []byte)
}

// RunResult captures the exit status, output transcript, duration, and cost/token telemetry of an adapter call.
type RunResult struct {
	ExitCode   int
	Output     string
	Duration   time.Duration
	TokensUsed int
	CostUSD    float64
}

// Capabilities describes an adapter's supported features.
type Capabilities struct {
	HasUsage           bool
	PassPromptViaStdin bool
	SupportsEffort     bool
	RequiresNetwork    bool
	AllowedEnvVars     []string
	Paths              []string
	// ReadOnlyPaths are directories under $HOME the agent needs to read but must not change,
	// such as the CLI's config and credentials. The sandbox mounts them read-only.
	ReadOnlyPaths []string
}

// Adapter defines the uniform interface for agent CLI runners (brief section 4).
type Adapter interface {
	Name() string
	Run(ctx context.Context, prompt string, opts RunOptions) (RunResult, error)
	Usage(ctx context.Context, worktreeRoot string) (string, error)
	Capabilities() Capabilities
}

// SafeEnvPrefixes and SafeEnvVars define allow-listed environment variables passed to agent CLIs (STRIDE I2).
var SafeEnvVars = map[string]bool{
	"PATH":         true,
	"HOME":         true,
	"USER":         true,
	"LOGNAME":      true,
	"SHELL":        true,
	"TERM":         true,
	"LANG":         true,
	"LC_ALL":       true,
	"LC_CTYPE":     true,
	"TMPDIR":       true,
	"TEMP":         true,
	"TMP":          true,
	"SYSTEMROOT":   true,
	"WINDIR":       true,
	"LOCALAPPDATA": true,
	"APPDATA":      true,
	"NODE_PATH":    true,
	"GOPATH":       true,
	"GOROOT":       true,
}

// ScrubEnv filters the environment to prevent leaking secrets, credentials, or dangerous git configs (STRIDE I2).
func ScrubEnv(hostEnv []string, extraEnv []string) []string {
	allowedKeys := make(map[string]bool)
	var explicitKV []string
	for _, item := range extraEnv {
		if strings.Contains(item, "=") {
			explicitKV = append(explicitKV, item)
		} else if item != "" {
			allowedKeys[item] = true
		}
	}

	var scrubbed []string
	for _, kv := range hostEnv {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k := parts[0]
		if SafeEnvVars[k] || allowedKeys[k] {
			scrubbed = append(scrubbed, kv)
		}
	}
	// Append explicitly requested extra K=V pairs
	scrubbed = append(scrubbed, explicitKV...)
	return scrubbed
}

func runSubprocess(ctx context.Context, dir string, argv []string, env []string, stdin string, maxBytes int, prof *sandbox.Profile, onOutput func([]byte)) (RunResult, error) {
	if len(argv) == 0 {
		return RunResult{}, errors.New("empty argv")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxOutputBytes
	}

	if prof != nil {
		var err error
		argv, err = sandbox.Wrap(argv, *prof)
		if err != nil {
			return RunResult{}, err
		}
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = ScrubEnv(os.Environ(), env)

	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var buf bytes.Buffer
	// Wrap buffer with a limited writer to enforce maxBytes cap
	w := &limitedWriter{w: &buf, remaining: int64(maxBytes), tee: onOutput}
	cmd.Stdout = w
	cmd.Stderr = w

	t0 := time.Now()
	err := cmd.Run()
	duration := time.Since(t0)

	if ctx.Err() != nil {
		return RunResult{ExitCode: -1, Output: buf.String(), Duration: duration}, ctx.Err()
	}

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return RunResult{ExitCode: -1, Output: buf.String(), Duration: duration}, err
		}
	}

	outStr := buf.String()
	tokens, costUSD := ParseCostAndTokens(outStr)

	return RunResult{
		ExitCode:   exitCode,
		Output:     outStr,
		Duration:   duration,
		TokensUsed: tokens,
		CostUSD:    costUSD,
	}, nil
}

var (
	reCostUSD    = regexp.MustCompile(`(?i)(?:total[_ ]cost|cost(?:_usd)?)\s*[:=]\s*\$?([0-9]+(?:\.[0-9]+)?)`)
	reDollarCost = regexp.MustCompile(`\$([0-9]+\.[0-9]{2,4})\b`)
	reTokens     = regexp.MustCompile(`(?i)(?:total[_ ]tokens|tokens(?:_used)?)\s*[:=]\s*([0-9,]+)`)
)

// ParseCostAndTokens extracts token usage and USD cost from agent CLI output transcripts.
func ParseCostAndTokens(output string) (tokens int, costUSD float64) {
	if m := reCostUSD.FindStringSubmatch(output); len(m) == 2 {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			costUSD = f
		}
	} else if m := reDollarCost.FindStringSubmatch(output); len(m) == 2 {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			costUSD = f
		}
	}
	if m := reTokens.FindStringSubmatch(output); len(m) == 2 {
		clean := strings.ReplaceAll(m[1], ",", "")
		if n, err := strconv.Atoi(clean); err == nil {
			tokens = n
		}
	}
	return tokens, costUSD
}

type limitedWriter struct {
	w         io.Writer
	remaining int64
	tee       func([]byte)
}

func (l *limitedWriter) Write(p []byte) (n int, err error) {
	if l.tee != nil {
		l.tee(p) // live followers see everything, even once the stored transcript is capped
	}
	if l.remaining <= 0 {
		return len(p), nil // drop excess bytes silently
	}
	toWrite := p
	if int64(len(p)) > l.remaining {
		toWrite = p[:l.remaining]
	}
	n, err = l.w.Write(toWrite)
	l.remaining -= int64(n)
	return len(p), err
}

// --- AgyAdapter (Antigravity CLI) ---

type AgyAdapter struct{}

func NewAgyAdapter() *AgyAdapter   { return &AgyAdapter{} }
func (a *AgyAdapter) Name() string { return "agy" }
func (a *AgyAdapter) Capabilities() Capabilities {
	return Capabilities{
		HasUsage:           true,
		PassPromptViaStdin: false,
		SupportsEffort:     false,
		RequiresNetwork:    true,
		AllowedEnvVars:     []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "ANTHROPIC_API_KEY"},
		Paths:              []string{".gemini"},
	}
}

func (a *AgyAdapter) BuildArgv(prompt string, opts RunOptions) []string {
	argv := []string{"agy", "-p", prompt}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	argv = append(argv, "--dangerously-skip-permissions")
	return argv
}

func (a *AgyAdapter) Run(ctx context.Context, prompt string, opts RunOptions) (RunResult, error) {
	argv := a.BuildArgv(prompt, opts)
	env := append([]string{}, a.Capabilities().AllowedEnvVars...)
	env = append(env, opts.Env...)
	return runSubprocess(ctx, opts.WorktreeRoot, argv, env, "", opts.MaxOutputBytes, opts.Sandbox, opts.OnOutput)
}

func (a *AgyAdapter) Usage(ctx context.Context, worktreeRoot string) (string, error) {
	res, err := runSubprocess(ctx, worktreeRoot, []string{"agy", "-p", "/usage"}, a.Capabilities().AllowedEnvVars, "", 1024*1024, nil, nil)
	if err != nil {
		return "", err
	}
	return res.Output, nil
}

// --- ClaudeAdapter (Claude Code CLI) ---

type ClaudeAdapter struct{}

func NewClaudeAdapter() *ClaudeAdapter { return &ClaudeAdapter{} }
func (a *ClaudeAdapter) Name() string  { return "claude-cli" }
func (a *ClaudeAdapter) Capabilities() Capabilities {
	return Capabilities{
		HasUsage:           true,
		PassPromptViaStdin: false,
		SupportsEffort:     true,
		RequiresNetwork:    true,
		AllowedEnvVars:     []string{"ANTHROPIC_API_KEY"},
		Paths:              []string{".claude", ".claude.json"},
	}
}

func (a *ClaudeAdapter) BuildArgv(prompt string, opts RunOptions) []string {
	argv := []string{"claude", "-p", prompt}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	if opts.Effort != "" {
		argv = append(argv, "--effort", opts.Effort)
	}
	if opts.SystemPrompt != "" {
		argv = append(argv, "--append-system-prompt", opts.SystemPrompt)
	}
	argv = append(argv, "--dangerously-skip-permissions")
	return argv
}

func (a *ClaudeAdapter) Run(ctx context.Context, prompt string, opts RunOptions) (RunResult, error) {
	argv := a.BuildArgv(prompt, opts)
	env := append([]string{}, a.Capabilities().AllowedEnvVars...)
	env = append(env, opts.Env...)
	return runSubprocess(ctx, opts.WorktreeRoot, argv, env, "", opts.MaxOutputBytes, opts.Sandbox, opts.OnOutput)
}

func (a *ClaudeAdapter) Usage(ctx context.Context, worktreeRoot string) (string, error) {
	res, err := runSubprocess(ctx, worktreeRoot, []string{"claude", "-p", "/usage"}, a.Capabilities().AllowedEnvVars, "", 1024*1024, nil, nil)
	if err != nil {
		return "", err
	}
	return res.Output, nil
}

// --- CodexAdapter (OpenAI Codex CLI) ---

type CodexAdapter struct{}

func NewCodexAdapter() *CodexAdapter { return &CodexAdapter{} }
func (a *CodexAdapter) Name() string { return "codex" }
func (a *CodexAdapter) Capabilities() Capabilities {
	return Capabilities{
		HasUsage:           false,
		PassPromptViaStdin: true,
		SupportsEffort:     true,
		RequiresNetwork:    true,
		AllowedEnvVars:     []string{"OPENAI_API_KEY"},
		Paths:              []string{".codex"},
	}
}

// codexEffort turns a work order's size (S, M, L) or a reasoning word into one codex accepts.
// Codex rejects anything but none, minimal, low, medium, high, xhigh and max, so a raw "M"
// blocked the chunk with "Invalid value: 'M'".
func codexEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "s":
		return "low"
	case "m", "":
		return "medium"
	case "l":
		return "high"
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	}
	return "medium"
}

func (a *CodexAdapter) BuildArgv(_ string, opts RunOptions) []string {
	argv := []string{"codex", "exec"}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	argv = append(argv, "-c", fmt.Sprintf("model_reasoning_effort=%q", codexEffort(opts.Effort)))
	argv = append(argv, "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "-")
	return argv
}

func (a *CodexAdapter) Run(ctx context.Context, prompt string, opts RunOptions) (RunResult, error) {
	argv := a.BuildArgv(prompt, opts)
	env := append([]string{}, a.Capabilities().AllowedEnvVars...)
	env = append(env, opts.Env...)
	// prompt goes in on stdin ("-")
	return runSubprocess(ctx, opts.WorktreeRoot, argv, env, prompt, opts.MaxOutputBytes, opts.Sandbox, opts.OnOutput)
}

func (a *CodexAdapter) Usage(_ context.Context, _ string) (string, error) {
	return "", errors.New("codex does not support a usage report (unmetered until-rejected pool)")
}

// --- OpenCodeAdapter (OpenAI-compatible / local model CLI) ---

type OpenCodeAdapter struct{}

func NewOpenCodeAdapter() *OpenCodeAdapter { return &OpenCodeAdapter{} }
func (a *OpenCodeAdapter) Name() string    { return "openai-compat" }
func (a *OpenCodeAdapter) Capabilities() Capabilities {
	return Capabilities{
		HasUsage:           false,
		PassPromptViaStdin: false,
		SupportsEffort:     false,
		RequiresNetwork:    true, // required for loopback/LAN llama.cpp, Ollama, or vLLM HTTP API
		AllowedEnvVars:     []string{"OPENAI_API_KEY", "OLLAMA_HOST"},
		// The config holds the provider definitions and keys; opencode only reads it, and a
		// worker running with --auto must not be able to delete it.
		ReadOnlyPaths: []string{".config/opencode"},
		Paths: []string{
			".opencode",
			".local/share/opencode",
			".local/state/opencode",
			".cache/opencode",
		},
	}
}

func (a *OpenCodeAdapter) BuildArgv(prompt string, opts RunOptions) []string {
	argv := []string{"opencode", "run", "--auto"}
	if opts.Model != "" {
		argv = append(argv, "-m", opts.Model)
	}
	argv = append(argv, prompt)
	return argv
}

func (a *OpenCodeAdapter) Run(ctx context.Context, prompt string, opts RunOptions) (RunResult, error) {
	argv := a.BuildArgv(prompt, opts)
	env := append([]string{}, a.Capabilities().AllowedEnvVars...)
	env = append(env, opts.Env...)
	return runSubprocess(ctx, opts.WorktreeRoot, argv, env, "", opts.MaxOutputBytes, opts.Sandbox, opts.OnOutput)
}

func (a *OpenCodeAdapter) Usage(_ context.Context, _ string) (string, error) {
	return "", errors.New("openai-compat does not support a usage report")
}

// --- AiderAdapter (Aider CLI) ---

type AiderAdapter struct{}

func NewAiderAdapter() *AiderAdapter { return &AiderAdapter{} }
func (a *AiderAdapter) Name() string { return "aider" }
func (a *AiderAdapter) Capabilities() Capabilities {
	return Capabilities{
		HasUsage:           false,
		PassPromptViaStdin: false,
		SupportsEffort:     false,
		RequiresNetwork:    true,
		AllowedEnvVars:     []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"},
		Paths:              []string{".aider.conf.yml"},
	}
}

func (a *AiderAdapter) BuildArgv(prompt string, opts RunOptions) []string {
	argv := []string{"aider", "--yes", "--no-auto-commits", "--message", prompt}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	return argv
}

func (a *AiderAdapter) Run(ctx context.Context, prompt string, opts RunOptions) (RunResult, error) {
	argv := a.BuildArgv(prompt, opts)
	env := append([]string{}, a.Capabilities().AllowedEnvVars...)
	env = append(env, opts.Env...)
	return runSubprocess(ctx, opts.WorktreeRoot, argv, env, "", opts.MaxOutputBytes, opts.Sandbox, opts.OnOutput)
}

func (a *AiderAdapter) Usage(_ context.Context, _ string) (string, error) {
	return "", errors.New("aider does not support a usage report")
}

// --- GeminiAdapter (gemini-cli) ---

type GeminiAdapter struct{}

func NewGeminiAdapter() *GeminiAdapter { return &GeminiAdapter{} }
func (a *GeminiAdapter) Name() string  { return "gemini-cli" }
func (a *GeminiAdapter) Capabilities() Capabilities {
	return Capabilities{
		HasUsage:           false,
		PassPromptViaStdin: false,
		SupportsEffort:     false,
		RequiresNetwork:    true,
		AllowedEnvVars:     []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
		Paths:              []string{".gemini"},
	}
}

func (a *GeminiAdapter) BuildArgv(prompt string, opts RunOptions) []string {
	argv := []string{"gemini", "-p", prompt}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	return argv
}

func (a *GeminiAdapter) Run(ctx context.Context, prompt string, opts RunOptions) (RunResult, error) {
	argv := a.BuildArgv(prompt, opts)
	env := append([]string{}, a.Capabilities().AllowedEnvVars...)
	env = append(env, opts.Env...)
	return runSubprocess(ctx, opts.WorktreeRoot, argv, env, "", opts.MaxOutputBytes, opts.Sandbox, opts.OnOutput)
}

func (a *GeminiAdapter) Usage(_ context.Context, _ string) (string, error) {
	return "", errors.New("gemini-cli does not support a usage report")
}

// --- Adapter Registry ---

// AllAdapters returns the default registry of all supported agent CLI adapters.
func AllAdapters() map[string]Adapter {
	return map[string]Adapter{
		"agy":             NewAgyAdapter(),
		"claude-cli":      NewClaudeAdapter(),
		"claude":          NewClaudeAdapter(),
		"codex":           NewCodexAdapter(),
		"openai-compat":   NewOpenCodeAdapter(),
		"opencode":        NewOpenCodeAdapter(),
		"aider":           NewAiderAdapter(),
		"gemini-cli":      NewGeminiAdapter(),
		"gemini":          NewGeminiAdapter(),
		"hermes":          NewHermesAdapter(),
		"hermes-agent":    NewHermesAdapter(),
		"pi":              NewPiAdapter(),
		"pi-coding-agent": NewPiAdapter(),
	}
}

// Get returns the adapter instance registered under the given name.
func Get(name string) (Adapter, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "agy":
		return NewAgyAdapter(), nil
	case "claude-cli", "claude":
		return NewClaudeAdapter(), nil
	case "codex":
		return NewCodexAdapter(), nil
	case "openai-compat", "opencode":
		return NewOpenCodeAdapter(), nil
	case "aider":
		return NewAiderAdapter(), nil
	case "gemini-cli", "gemini":
		return NewGeminiAdapter(), nil
	case "hermes", "hermes-agent":
		return NewHermesAdapter(), nil
	case "pi", "pi-coding-agent":
		return NewPiAdapter(), nil
	default:
		return nil, fmt.Errorf("unknown adapter %q", name)
	}
}
