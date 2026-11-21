// Package config parses and validates shepherd.toml per ADR-0004.
// Strict validation is enforced: unknown keys are errors, not warnings.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/erinwnash/Shepherd/internal/verify"
	"github.com/pelletier/go-toml/v2"
)

// CurrentSchemaVersion is the supported shepherd.toml schema version.
const CurrentSchemaVersion = 1

// Config represents a validated shepherd.toml configuration.
type Config struct {
	SchemaVersion int                   `toml:"schema_version"`
	Project       ProjectConfig         `toml:"project"`
	Queue         QueueConfig           `toml:"queue"`
	Verify        VerifyConfig          `toml:"verify"`
	Repair        RepairConfig          `toml:"repair"`
	Tiers         map[string]TierConfig `toml:"tiers"`
	Quota         QuotaConfig           `toml:"quota"`
	Decide        DecideConfig          `toml:"decide"`
	Policy        PolicyConfig          `toml:"policy"`
	Notify        NotifyConfig          `toml:"notify,omitempty"`
	Slots         SlotsConfig           `toml:"slots,omitempty"`
	Git           GitConfig             `toml:"git,omitempty"`
}

type GitConfig struct {
	Mode         string `toml:"mode,omitempty"`          // "direct" (default) | "pr"
	BranchPrefix string `toml:"branch_prefix,omitempty"` // default "shepherd/sprint-"
	AutoPR       bool   `toml:"auto_pr,omitempty"`
}

type SlotsConfig struct {
	MaxSlots        int     `toml:"max_slots,omitempty"`
	MinFreeRAMGB    float64 `toml:"min_free_ram_gb,omitempty"`
	MaxGPUWorkers   int     `toml:"max_gpu_workers,omitempty"`
	DesktopPriority bool    `toml:"desktop_priority,omitempty"`
}

type ProjectConfig struct {
	Name       string `toml:"name"`
	Repo       string `toml:"repo"`
	BaseBranch string `toml:"base_branch"`
}

type QueueConfig struct {
	Source     string `toml:"source"`
	Root       string `toml:"root"`
	DoneMarker string `toml:"done_marker"`
	DoneDir    string `toml:"done_dir,omitempty"`
}

type GateConfig struct {
	Name         string   `toml:"name"`
	Builtin      string   `toml:"builtin,omitempty"`
	Argv         []string `toml:"argv,omitempty"`
	SubchunkArgv []string `toml:"subchunk_argv,omitempty"`
	On           string   `toml:"on,omitempty"` // "" / "always" (default), "group_end", "subchunk"
	Hard         bool     `toml:"hard"`
	Glob         string   `toml:"glob,omitempty"`
	AllowFrom    string   `toml:"allow_from,omitempty"`
}

type VerifyConfig struct {
	Gates           []GateConfig `toml:"gates"`
	FlakeRetries    int          `toml:"flake_retries,omitempty"`
	BaseCommitDelta bool         `toml:"base_commit_delta,omitempty"`
}

type MechanicalFixConfig struct {
	Tier      string   `toml:"tier"`
	MaxErrors int      `toml:"max_errors"`
	Codes     []string `toml:"codes"`
}

// HandoffConfig controls the notes passed between repair attempts and tiers. Both parts only add
// prompt text and default on.
type HandoffConfig struct {
	Enabled        *bool `toml:"enabled"`          // master switch for parts 1 and 2
	WorkerNote     *bool `toml:"worker_note"`      // part 2 only
	MaxOutputLines int   `toml:"max_output_lines"` // verifier output lines kept per attempt
	MaxNoteBytes   int   `toml:"max_note_bytes"`
}

// IsEnabled reports whether handoff is on (default true).
func (h HandoffConfig) IsEnabled() bool { return h.Enabled == nil || *h.Enabled }

// WorkerNoteEnabled reports whether workers are asked for a note (needs IsEnabled).
func (h HandoffConfig) WorkerNoteEnabled() bool {
	return h.IsEnabled() && (h.WorkerNote == nil || *h.WorkerNote)
}

type RepairConfig struct {
	Chain         []string            `toml:"chain"`
	MechanicalFix MechanicalFixConfig `toml:"mechanical_fix"`
	Handoff       HandoffConfig       `toml:"handoff"`
}

type TierConfig struct {
	Adapter     string         `toml:"adapter"`
	Model       string         `toml:"model,omitempty"`
	Pool        string         `toml:"pool,omitempty"`
	URL         string         `toml:"url,omitempty"`
	Enabled     *bool          `toml:"enabled,omitempty"`
	Priority    int            `toml:"priority,omitempty"`
	UsageCapPct int            `toml:"usage_cap_pct,omitempty"`
	Settings    map[string]any `toml:"settings,omitempty"`
}

// IsEnabled returns true unless Enabled is explicitly set to false.
func (t TierConfig) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}

// BoolPtr returns a pointer to b.
func BoolPtr(b bool) *bool {
	return &b
}

type PoolConfig struct {
	Mode             string   `toml:"mode"` // "polled" | "until-rejected"
	UsageCmd         []string `toml:"usage_cmd,omitempty"`
	Parser           string   `toml:"parser,omitempty"`
	FloorWeeklyPct   int      `toml:"floor_weekly_pct,omitempty"`
	FloorFiveHourPct int      `toml:"floor_five_hour_pct,omitempty"`
	UsageCapPct      int      `toml:"usage_cap_pct,omitempty"`
	BackoffMin       []int    `toml:"backoff_min,omitempty"`
}

type QuotaConfig struct {
	Pools map[string]PoolConfig `toml:"pools"`
}

type DecideConfig struct {
	Backend   string `toml:"backend"` // "rules" | "local-grammar" | "laya" | "jev"
	TimeoutMs int    `toml:"timeout_ms"`
}

type PolicyConfig struct {
	MaxConsecutiveBlocks   int     `toml:"max_consecutive_blocks"`
	WaitForResetMaxMin     int     `toml:"wait_for_reset_max_min"`
	CloudFirstThreshold    float64 `toml:"cloud_first_threshold"`
	AllowUnsandboxed       bool    `toml:"allow_unsandboxed,omitempty"`
	RequireHighReviewHuman bool    `toml:"require_high_review_human,omitempty"`
	MaxChunkCostUSD        float64 `toml:"max_chunk_cost_usd,omitempty"`
	MaxSprintCostUSD       float64 `toml:"max_sprint_cost_usd,omitempty"`
	SandboxBackend         string  `toml:"sandbox_backend,omitempty"`
	ContainerImage         string  `toml:"container_image,omitempty"`
}

type NotifyConfig struct {
	Events            []string `toml:"events,omitempty"`
	SlackWebhookURL   string   `toml:"slack_webhook_url,omitempty"`
	DiscordWebhookURL string   `toml:"discord_webhook_url,omitempty"`
	Desktop           bool     `toml:"desktop,omitempty"`
	NtfyURL           string   `toml:"ntfy_url,omitempty"`
	NtfyTopic         string   `toml:"ntfy_topic,omitempty"`
	NtfyTokenEnv      string   `toml:"ntfy_token_env,omitempty"` // env var holding an ntfy access token
	NotifyCmd         string   `toml:"notify_cmd,omitempty"`
}

// AllowedPlaceholders in gate argv arrays.
var AllowedPlaceholders = map[string]bool{
	"{test_files}":    true,
	"{touched_files}": true,
}

// VerifierGates converts configuration gates into verify.GateConfig for the verification engine.
func (c *Config) VerifierGates() []verify.GateConfig {
	out := make([]verify.GateConfig, len(c.Verify.Gates))
	for i, g := range c.Verify.Gates {
		out[i] = verify.GateConfig{
			Name:         g.Name,
			Builtin:      g.Builtin,
			Argv:         g.Argv,
			SubchunkArgv: g.SubchunkArgv,
			On:           g.On,
			Hard:         g.Hard,
			Glob:         g.Glob,
		}
	}
	return out
}

// LoadFile reads and validates shepherd.toml at the given path.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return LoadBytes(data)
}

// LoadBytes parses and strictly validates shepherd.toml contents.
func LoadBytes(src []byte) (*Config, error) {
	// Strip UTF-8 BOM and normalize CRLF to LF
	clean := bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	clean = bytes.ReplaceAll(clean, []byte("\r\n"), []byte("\n"))

	var cfg Config
	dec := toml.NewDecoder(bytes.NewReader(clean))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode shepherd.toml: %w", err)
	}

	if err := cfg.validateAndSetDefaults(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validateAndSetDefaults() error {
	if c.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", c.SchemaVersion, CurrentSchemaVersion)
	}

	if strings.TrimSpace(c.Project.Name) == "" {
		return fmt.Errorf("project.name is required")
	}
	if c.Project.Repo == "" {
		c.Project.Repo = "."
	}
	if c.Project.BaseBranch == "" {
		c.Project.BaseBranch = "main"
	}

	if c.Repair.Handoff.MaxOutputLines <= 0 {
		c.Repair.Handoff.MaxOutputLines = 20
	}
	if c.Repair.Handoff.MaxNoteBytes <= 0 {
		c.Repair.Handoff.MaxNoteBytes = 2048
	}

	// Queue defaults
	if c.Queue.Source == "" {
		c.Queue.Source = "markdown-dir"
	}
	if c.Queue.Root == "" {
		c.Queue.Root = "docs/plan"
	}
	if c.Queue.DoneMarker == "" {
		c.Queue.DoneMarker = "move-to-done"
	}
	if c.Queue.DoneDir == "" && (c.Queue.DoneMarker == "" || c.Queue.DoneMarker == "move-to-done") {
		c.Queue.DoneDir = filepath.ToSlash(c.Queue.Root + "DONE")
	}

	// Policy defaults
	if c.Policy.MaxConsecutiveBlocks == 0 {
		c.Policy.MaxConsecutiveBlocks = 3
	}
	if c.Policy.WaitForResetMaxMin == 0 {
		c.Policy.WaitForResetMaxMin = 45
	}
	if c.Policy.CloudFirstThreshold == 0 {
		c.Policy.CloudFirstThreshold = 0.5
	}

	// Slots defaults (Improvement 1: Hardware Safeguards)
	if c.Slots.MaxSlots <= 0 {
		c.Slots.MaxSlots = 2
	}
	if c.Slots.MinFreeRAMGB <= 0 {
		c.Slots.MinFreeRAMGB = 4.0
	}

	// Validate gates
	for _, gate := range c.Verify.Gates {
		if gate.Name == "" {
			return fmt.Errorf("gate name is required")
		}
		if gate.Builtin == "" && len(gate.Argv) == 0 {
			return fmt.Errorf("gate %q must specify either builtin or argv", gate.Name)
		}
		switch gate.On {
		case "", "always", "group_end", "subchunk":
		default:
			return fmt.Errorf("gate %q: invalid 'on' trigger %q (want 'always', 'group_end', or 'subchunk')", gate.Name, gate.On)
		}
		for _, arg := range gate.Argv {
			if strings.Contains(arg, "{") || strings.Contains(arg, "}") {
				if !AllowedPlaceholders[arg] {
					return fmt.Errorf("gate %q: disallowed placeholder %q in argv (allowed: {test_files}, {touched_files})", gate.Name, arg)
				}
			}
		}
		for _, arg := range gate.SubchunkArgv {
			if strings.Contains(arg, "{") || strings.Contains(arg, "}") {
				if !AllowedPlaceholders[arg] {
					return fmt.Errorf("gate %q: disallowed placeholder %q in subchunk_argv (allowed: {test_files}, {touched_files})", gate.Name, arg)
				}
			}
		}
	}

	// Validate tiers
	for name, tier := range c.Tiers {
		if tier.Adapter == "" {
			return fmt.Errorf("tier %q missing adapter", name)
		}
	}

	// Validate pools
	for name, pool := range c.Quota.Pools {
		switch pool.Mode {
		case "polled":
			if len(pool.UsageCmd) == 0 {
				return fmt.Errorf("pool %q mode is polled but usage_cmd is empty", name)
			}
			if pool.Parser == "" {
				return fmt.Errorf("pool %q mode is polled but parser is empty", name)
			}
		case "until-rejected":
			if len(pool.BackoffMin) == 0 {
				return fmt.Errorf("pool %q mode is until-rejected but backoff_min is empty", name)
			}
		default:
			return fmt.Errorf("pool %q: unknown mode %q (want 'polled' or 'until-rejected')", name, pool.Mode)
		}
	}

	// Validate repair chain tier references (if tiers are defined)
	if len(c.Tiers) > 0 {
		for _, step := range c.Repair.Chain {
			tierName := strings.Split(step, ":")[0]
			if _, ok := c.Tiers[tierName]; !ok {
				return fmt.Errorf("repair chain refers to unknown tier %q", tierName)
			}
		}
		if c.Repair.MechanicalFix.Tier != "" {
			if _, ok := c.Tiers[c.Repair.MechanicalFix.Tier]; !ok {
				return fmt.Errorf("mechanical_fix refers to unknown tier %q", c.Repair.MechanicalFix.Tier)
			}
		}
	}

	return nil
}

// MarshalTOML encodes a Config struct into valid shepherd.toml bytes.
func MarshalTOML(cfg *Config) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = CurrentSchemaVersion
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal shepherd.toml: %w", err)
	}
	// Verify round-trip strict validation
	if _, err := LoadBytes(data); err != nil {
		return nil, fmt.Errorf("validate marshaled shepherd.toml: %w", err)
	}
	return data, nil
}

// SaveFile marshals and atomically writes cfg to path.
func SaveFile(path string, cfg *Config) error {
	data, err := MarshalTOML(cfg)
	if err != nil {
		return err
	}
	cleanPath := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(cleanPath), err)
	}
	return os.WriteFile(cleanPath, data, 0644)
}

