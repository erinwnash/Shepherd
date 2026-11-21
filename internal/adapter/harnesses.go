package adapter

import (
	"context"
	"errors"
)

// --- HermesAdapter (Nous Research Hermes Agent) ---
//
// `hermes chat --oneshot -q <prompt>` answers one query and exits with the tool output in the
// transcript. Exit codes: 0 turn completed, 1 failed or stopped early, 130 interrupted.

type HermesAdapter struct{}

func NewHermesAdapter() *HermesAdapter { return &HermesAdapter{} }
func (a *HermesAdapter) Name() string  { return "hermes" }
func (a *HermesAdapter) Capabilities() Capabilities {
	return Capabilities{
		HasUsage:           false,
		PassPromptViaStdin: false,
		SupportsEffort:     false,
		RequiresNetwork:    true,
		AllowedEnvVars: []string{
			"NOUS_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OLLAMA_HOST",
		},
		Paths: []string{".hermes"},
	}
}

func (a *HermesAdapter) BuildArgv(prompt string, opts RunOptions) []string {
	argv := []string{"hermes", "chat", "--oneshot"}
	if opts.Model != "" {
		argv = append(argv, "-m", opts.Model)
	}
	return append(argv, "-q", prompt)
}

func (a *HermesAdapter) Run(ctx context.Context, prompt string, opts RunOptions) (RunResult, error) {
	return runHarness(ctx, a.BuildArgv(prompt, opts), a.Capabilities(), opts)
}

func (a *HermesAdapter) Usage(_ context.Context, _ string) (string, error) {
	return "", errors.New("hermes does not support a usage report")
}

// --- PiAdapter (pi coding agent, https://pi.dev) ---
//
// `pi -p <prompt>` is print mode: run the prompt, print the response, exit. Pi has no permission
// prompts of its own, so Shepherd's sandbox and scope gate are the containment.

type PiAdapter struct{}

func NewPiAdapter() *PiAdapter    { return &PiAdapter{} }
func (a *PiAdapter) Name() string { return "pi" }
func (a *PiAdapter) Capabilities() Capabilities {
	return Capabilities{
		HasUsage:           false,
		PassPromptViaStdin: false,
		SupportsEffort:     false,
		RequiresNetwork:    true,
		AllowedEnvVars: []string{
			"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY", "OLLAMA_HOST",
		},
		Paths: []string{".pi"},
	}
}

func (a *PiAdapter) BuildArgv(prompt string, opts RunOptions) []string {
	argv := []string{"pi", "-p"}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	return append(argv, prompt)
}

func (a *PiAdapter) Run(ctx context.Context, prompt string, opts RunOptions) (RunResult, error) {
	return runHarness(ctx, a.BuildArgv(prompt, opts), a.Capabilities(), opts)
}

func (a *PiAdapter) Usage(_ context.Context, _ string) (string, error) {
	return "", errors.New("pi does not support a usage report")
}

func runHarness(ctx context.Context, argv []string, caps Capabilities, opts RunOptions) (RunResult, error) {
	env := append([]string{}, caps.AllowedEnvVars...)
	env = append(env, opts.Env...)
	return runSubprocess(ctx, opts.WorktreeRoot, argv, env, "", opts.MaxOutputBytes, opts.Sandbox, opts.OnOutput)
}
