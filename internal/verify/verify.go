package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/erinwnash/Shepherd/internal/scope"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

// GateConfig defines one verification gate from shepherd.toml (ADR-0004).
type GateConfig struct {
	Name         string   `toml:"name"`
	Builtin      string   `toml:"builtin,omitempty"` // "content-loss", "scope", etc.
	Argv         []string `toml:"argv,omitempty"`
	SubchunkArgv []string `toml:"subchunk_argv,omitempty"`
	On           string   `toml:"on,omitempty"` // "" / "always" (default), "group_end", "subchunk"
	Hard         bool     `toml:"hard"`
	Glob         string   `toml:"glob,omitempty"`
}

// GateResult holds the outcome of running one gate.
type GateResult struct {
	Name         string
	Hard         bool
	Passed       bool
	Skipped      bool
	Output       string
	FlakeRetried bool
	PreExisting  bool
}

// VerifyResult aggregates the outcomes of all gates run against a chunk.
type VerifyResult struct {
	Passed      bool
	HardFail    bool
	Results     []GateResult
	Errors      string   // short readable summary for the next model call
	SoftDebt    []string // soft gate failures (e.g. lint debt)
	Quarantined []string // gates quarantined as pre-existing base failures
}

// WorktreeState holds the necessary context for verifying a chunk.
type WorktreeState struct {
	WorktreeRoot          string
	WorkOrder             *workorder.WorkOrder
	CurrentWorkOrderBytes []byte
	ChangedPaths          []string
	Numstat               []NumstatEntry
	Protected             scope.Protected
	Env                   []string
	IsSubChunk            bool // true when other chunks in the same group (e.g. 45A.*) are still pending/running
}

// Verifier coordinates running verification gates in sequence.
type Verifier struct {
	Gates           []GateConfig
	FlakeRetries    int
	BaseCommitDelta bool
	GateMutex       interface {
		Lock(ctx context.Context) error
		Unlock()
	}
	PriorityWrapper func(argv []string) []string
	SandboxWrapper  func(argv []string) ([]string, error)
	BaseRunner      func(ctx context.Context, worktreeRoot string, argv []string, env []string) (GateExecResult, error)
}

// NewVerifier creates a new Verifier with the specified gate sequence.
func NewVerifier(gates []GateConfig) *Verifier {
	return &Verifier{Gates: gates}
}

// DefaultGates returns the default built-in gate configuration.
func DefaultGates() []GateConfig {
	return []GateConfig{
		{Name: "content-loss", Builtin: "content-loss", Hard: true},
		{Name: "scope", Builtin: "scope", Hard: true},
		{Name: "secrets", Builtin: "secrets", Hard: true},
	}
}

// Verify executes all gates against the worktree state. Built-in gates run in-process;
// custom gates are executed as argv arrays directly without a shell (ADR-0004).
func (v *Verifier) Verify(ctx context.Context, state WorktreeState) (VerifyResult, error) {
	out := VerifyResult{Passed: true}
	var errSections []string

	for _, gate := range v.Gates {
		var gRes GateResult
		gRes.Name = gate.Name
		gRes.Hard = gate.Hard

		switch gate.Builtin {
		case "content-loss":
			losses := CheckNumstatLoss(state.Numstat)
			if len(losses) > 0 {
				gRes.Passed = false
				gRes.Output = FormatContentLossErrors(losses)
			} else {
				gRes.Passed = true
			}

		case "scope":
			sRes := CheckScope(state.WorkOrder, state.CurrentWorkOrderBytes, state.ChangedPaths, state.Protected)
			if !sRes.Passed {
				gRes.Passed = false
				gRes.Output = sRes.Errors
			} else {
				gRes.Passed = true
			}

		case "secrets":
			secretFindings := CheckSecrets(state.WorktreeRoot, state.ChangedPaths)
			if len(secretFindings) > 0 {
				gRes.Passed = false
				gRes.Output = FormatSecretErrors(secretFindings)
			} else {
				gRes.Passed = true
			}

		default:
			// Custom gate
			if (gate.On == "group_end" && state.IsSubChunk) || (gate.On == "subchunk" && !state.IsSubChunk) {
				gRes.Passed = true
				gRes.Skipped = true
				out.Results = append(out.Results, gRes)
				continue
			}

			tests := []string{}
			if state.WorkOrder != nil {
				tests = state.WorkOrder.Tests
			}

			gateArgv := gate.Argv
			if state.IsSubChunk {
				gateArgv = ResolveSubchunkArgv(gate, state.WorktreeRoot, tests)
			}

			expanded, err := ExpandPlaceholders(gateArgv, tests, state.ChangedPaths, gate.Glob)
			if errors.Is(err, ErrSkipGate) {
				gRes.Passed = true
				gRes.Skipped = true
			} else if err != nil {
				gRes.Passed = false
				gRes.Output = fmt.Sprintf("## Gate configuration error (%s)\n%v\n", gate.Name, err)
			} else {
				if v.GateMutex != nil {
					_ = v.GateMutex.Lock(ctx)
				}
				execArgv := expanded
				if v.SandboxWrapper != nil {
					if wrapped, wErr := v.SandboxWrapper(execArgv); wErr == nil {
						execArgv = wrapped
					}
				}
				if v.PriorityWrapper != nil {
					execArgv = v.PriorityWrapper(execArgv)
				}
				execRes, runErr := RunGate(ctx, state.WorktreeRoot, execArgv, state.Env)
				if (runErr != nil || execRes.ExitCode != 0) && v.FlakeRetries > 0 {
					for attempt := 1; attempt <= v.FlakeRetries; attempt++ {
						rRes, rErr := RunGate(ctx, state.WorktreeRoot, execArgv, state.Env)
						if rErr == nil && rRes.ExitCode == 0 {
							execRes = rRes
							runErr = nil
							gRes.FlakeRetried = true
							break
						}
					}
				}

				if runErr != nil {
					gRes.Passed = false
					gRes.Output = fmt.Sprintf("## Gate execution error (%s)\n%v\n%s\n", gate.Name, runErr, execRes.Output)
				} else if execRes.ExitCode != 0 {
					if v.BaseCommitDelta {
						runner := v.BaseRunner
						if runner == nil {
							runner = DefaultBaseRunner
						}
						baseRes, baseErr := runner(ctx, state.WorktreeRoot, expanded, state.Env)
						if baseErr == nil && baseRes.ExitCode != 0 {
							// Failure pre-existed on base commit! Quarantine it so chunk is not penalized.
							gRes.PreExisting = true
							gRes.Passed = true
							gRes.Output = fmt.Sprintf("## Gate quarantined (%s, pre-existing failure)\nTest failed with exit %d on chunk, but also failed on base commit (exit %d).\n%s\n",
								gate.Name, execRes.ExitCode, baseRes.ExitCode, execRes.Output)
							out.Quarantined = append(out.Quarantined, gate.Name)
						} else {
							gRes.Passed = false
							gRes.Output = fmt.Sprintf("## Gate failed (%s, exit %d)\n%s\n", gate.Name, execRes.ExitCode, execRes.Output)
						}
					} else {
						gRes.Passed = false
						gRes.Output = fmt.Sprintf("## Gate failed (%s, exit %d)\n%s\n", gate.Name, execRes.ExitCode, execRes.Output)
					}
				} else {
					gRes.Passed = true
					gRes.Output = execRes.Output
				}

				if v.GateMutex != nil {
					v.GateMutex.Unlock()
				}
			}
		}

		out.Results = append(out.Results, gRes)

		if !gRes.Passed {
			if gRes.Hard {
				out.HardFail = true
				out.Passed = false
				errSections = append(errSections, gRes.Output)
			} else {
				out.SoftDebt = append(out.SoftDebt, gRes.Output)
			}
		}
	}

	if out.HardFail {
		out.Passed = false
	} else {
		out.Passed = true
	}

	out.Errors = strings.Join(errSections, "\n")
	return out, nil
}
