// Package remote lets an operator steer the controller by posting keywords to the ntfy topic
// Shepherd already notifies on. The command set is a fixed enum of control actions; the config
// file only binds keywords to them. It is loaded from .shepherd/ (a protected path), so a chunk
// can never add or widen a remote command.
package remote

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// FileName is the commands file inside the project's .shepherd directory.
const FileName = "ntfy-commands.toml"

// EmergencyKeyword is the built-in full-stop codeword. It always maps to ActionLockdown.
const EmergencyKeyword = "obliviate"

// Actions that can be bound to a keyword. Anything not listed here (unlock, approve, config,
// quota) is deliberately unreachable from the ntfy channel.
const (
	ActionLockdown = "lockdown"
	ActionHalt     = "halt"
	ActionPause    = "pause"
	ActionStop     = "stop"
	ActionDrainOn  = "drain-on"
	ActionHold     = "hold"
	ActionSkip     = "skip"
	ActionStatus   = "status"

	// Escalating actions start or widen work; they need allow_escalating = true.
	ActionStart    = "start"
	ActionResume   = "resume"
	ActionDrainOff = "drain-off"
	ActionRetry    = "retry"
	ActionRelease  = "release"
)

type actionSpec struct {
	needsChunk bool
	escalating bool
	emergency  bool
}

var actions = map[string]actionSpec{
	ActionLockdown: {emergency: true},
	ActionHalt:     {emergency: true},
	ActionPause:    {},
	ActionStop:     {},
	ActionDrainOn:  {},
	ActionHold:     {needsChunk: true},
	ActionSkip:     {needsChunk: true},
	ActionStatus:   {},
	ActionStart:    {escalating: true},
	ActionResume:   {escalating: true},
	ActionDrainOff: {escalating: true},
	ActionRetry:    {needsChunk: true, escalating: true},
	ActionRelease:  {needsChunk: true, escalating: true},
}

// Command binds one keyword to one action.
type Command struct {
	Keyword     string `toml:"keyword"`
	Action      string `toml:"action"`
	Description string `toml:"description,omitempty"`
}

// Commands is the parsed ntfy-commands.toml.
type Commands struct {
	SchemaVersion   int       `toml:"schema_version"`
	Enabled         bool      `toml:"enabled"`
	URL             string    `toml:"url,omitempty"`
	TokenEnv        string    `toml:"token_env,omitempty"`
	RateLimitPerMin int       `toml:"rate_limit_per_min,omitempty"`
	AllowEscalating bool      `toml:"allow_escalating,omitempty"`
	Command         []Command `toml:"command"`

	byKeyword map[string]Command
}

//go:embed default-commands.toml
var defaultCommandsTOML []byte

var (
	keywordRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)
	envNameRe  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	chunkIDRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	errNoValue = errors.New("no value")
)

// DefaultCommands returns the built-in configuration (obliviate only).
func DefaultCommands() *Commands {
	c, err := ParseCommands(defaultCommandsTOML)
	if err != nil {
		panic("remote: embedded default commands invalid: " + err.Error())
	}
	return c
}

// DefaultCommandsTOML returns the text written to .shepherd/ntfy-commands.toml when it is missing.
func DefaultCommandsTOML() []byte { return append([]byte(nil), defaultCommandsTOML...) }

// LoadCommands reads and validates a commands file.
func LoadCommands(path string) (*Commands, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	c, err := ParseCommands(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// EnsureCommandsFile loads <stateDir>/ntfy-commands.toml, writing the default first if absent.
func EnsureCommandsFile(stateDir string) (*Commands, string, error) {
	path := filepath.Join(stateDir, FileName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, defaultCommandsTOML, 0o600); err != nil {
			return DefaultCommands(), path, fmt.Errorf("write default %s: %w", path, err)
		}
	}
	c, err := LoadCommands(path)
	return c, path, err
}

// ParseCommands strictly parses commands TOML (unknown keys are errors; BOM and CRLF tolerated).
func ParseCommands(src []byte) (*Commands, error) {
	clean := bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	clean = bytes.ReplaceAll(clean, []byte("\r\n"), []byte("\n"))

	var c Commands
	dec := toml.NewDecoder(bytes.NewReader(clean))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode ntfy commands: %w", err)
	}
	if c.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported schema_version %d (want 1)", c.SchemaVersion)
	}
	if c.URL != "" {
		if err := ValidateURL(c.URL); err != nil {
			return nil, fmt.Errorf("url: %w", err)
		}
	}
	if c.TokenEnv != "" && !envNameRe.MatchString(c.TokenEnv) {
		return nil, fmt.Errorf("token_env %q is not a valid environment variable name", c.TokenEnv)
	}
	if c.RateLimitPerMin < 0 {
		return nil, fmt.Errorf("rate_limit_per_min must be >= 0")
	}
	if c.RateLimitPerMin == 0 {
		c.RateLimitPerMin = 10
	}

	c.byKeyword = map[string]Command{}
	for _, cmd := range c.Command {
		kw := strings.ToLower(strings.TrimSpace(cmd.Keyword))
		if !keywordRe.MatchString(kw) {
			return nil, fmt.Errorf("keyword %q: use 2-32 chars of a-z, 0-9, '-' or '_'", cmd.Keyword)
		}
		if _, ok := actions[cmd.Action]; !ok {
			return nil, fmt.Errorf("keyword %q: action %q is not available remotely (allowed: %s)", kw, cmd.Action, strings.Join(ActionNames(), ", "))
		}
		if kw == EmergencyKeyword && cmd.Action != ActionLockdown {
			return nil, fmt.Errorf("keyword %q is reserved for the full-stop lockdown and cannot be remapped", EmergencyKeyword)
		}
		if _, dup := c.byKeyword[kw]; dup {
			return nil, fmt.Errorf("keyword %q is defined twice", kw)
		}
		cmd.Keyword = kw
		c.byKeyword[kw] = cmd
	}
	if _, ok := c.byKeyword[EmergencyKeyword]; !ok {
		c.byKeyword[EmergencyKeyword] = Command{Keyword: EmergencyKeyword, Action: ActionLockdown, Description: "full stop (built in)"}
	}
	return &c, nil
}

func (c *Commands) lookup(keyword string) (Command, bool) {
	cmd, ok := c.byKeyword[keyword]
	return cmd, ok
}

// Keywords lists "keyword=action" pairs, sorted, for logging.
func (c *Commands) Keywords() []string {
	out := make([]string, 0, len(c.byKeyword))
	for kw, cmd := range c.byKeyword {
		out = append(out, kw+"="+cmd.Action)
	}
	sort.Strings(out)
	return out
}

// Token returns the ntfy access token from the configured environment variable, if any.
func (c *Commands) Token() (string, error) {
	if c.TokenEnv == "" {
		return "", errNoValue
	}
	v := strings.TrimSpace(os.Getenv(c.TokenEnv))
	if v == "" {
		return "", errNoValue
	}
	return v, nil
}

// ActionNames returns all remotely available actions, sorted.
func ActionNames() []string {
	out := make([]string, 0, len(actions))
	for a := range actions {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ValidateURL requires https, except plain http to loopback/private hosts (self-hosted ntfy on the LAN).
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not an absolute URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
			return nil
		}
		return fmt.Errorf("%q: plain http is only allowed for loopback/LAN ntfy servers; use https", raw)
	default:
		return fmt.Errorf("%q: scheme must be https", raw)
	}
}
