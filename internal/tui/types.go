// Package tui implements the terminal user interface dashboard per Section 5 of the design brief.
// Provides live visualization of quota gauges, worker execution, queue state, audit trail,
// and interactive keyboard steering.
package tui

import (
	"time"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/eventlog"
	"github.com/erinwnash/Shepherd/internal/quota"
)

// ANSI color and formatting codes.
const (
	Reset        = "\x1b[0m"
	Bold         = "\x1b[1m"
	Dim          = "\x1b[2m"
	Italic       = "\x1b[3m"
	Underline    = "\x1b[4m"
	FgBlack      = "\x1b[30m"
	FgRed        = "\x1b[31m"
	FgGreen      = "\x1b[32m"
	FgYellow     = "\x1b[33m"
	FgBlue       = "\x1b[34m"
	FgMagenta    = "\x1b[35m"
	FgCyan       = "\x1b[36m"
	FgWhite      = "\x1b[37m"
	FgHiBlack    = "\x1b[90m"
	FgHiRed      = "\x1b[91m"
	FgHiGreen    = "\x1b[92m"
	FgHiYellow   = "\x1b[93m"
	FgHiBlue     = "\x1b[94m"
	FgHiMagenta  = "\x1b[95m"
	FgHiCyan     = "\x1b[96m"
	FgHiWhite    = "\x1b[97m"
	BgRed        = "\x1b[41m"
	BgGreen      = "\x1b[42m"
	BgYellow     = "\x1b[43m"
	BgBlue       = "\x1b[44m"
	BgDarkSlate  = "\x1b[48;5;236m"
	ClearScreen  = "\x1b[2J\x1b[H"
	AltScreenOn  = "\x1b[?1049h"
	AltScreenOff = "\x1b[?1049l"
	CursorHide   = "\x1b[?25l"
	CursorShow   = "\x1b[?25h"
)

// DashboardTab denotes which view tab is active in the TUI.
type DashboardTab int

const (
	TabOverview DashboardTab = 0
	TabLogs     DashboardTab = 1
	TabStack    DashboardTab = 2
	TabQueue    DashboardTab = 3
)

// tabCount is the number of dashboard tabs.
const tabCount = 4

// StackItem represents a single AI router or coding CLI tier in the TUI stack manager.
type StackItem struct {
	Tier        string   `json:"tier"`
	Adapter     string   `json:"adapter"`
	Model       string   `json:"model"`
	Models      []string `json:"models,omitempty"`
	URL         string   `json:"url,omitempty"`
	Pool        string   `json:"pool"`
	PoolMode    string   `json:"pool_mode"`
	Enabled     bool     `json:"enabled"`
	Priority    int      `json:"priority"`
	Attempts    int      `json:"attempts"`
	FloorPct    int      `json:"floor_pct"`
	UsageCapPct int      `json:"usage_cap_pct"`
	ProbeStatus string   `json:"probe_status,omitempty"`
	ProbeOk     bool     `json:"probe_ok"`
}

// StackRules holds routing rules and usage caps editable from the TUI.
type StackRules struct {
	CloudFirstThreshold    float64 `json:"cloud_first_threshold"`
	MaxChunkCostUSD        float64 `json:"max_chunk_cost_usd"`
	MaxSprintCostUSD       float64 `json:"max_sprint_cost_usd"`
	RequireHighReviewHuman bool    `json:"require_high_review_human"`
}

// DashboardState captures a full snapshot of the daemon for TUI rendering.
type DashboardState struct {
	Status           control.DaemonStatus
	Chunks           []db.ChunkRecord
	Quota            map[string]any
	QuotaReadings    map[string]quota.Bucket
	pollPools        map[string]config.PoolConfig // pools the background quota poller should read
	AuditEvents      []eventlog.Event
	LastAction       string
	ActionErr        string
	Timestamp        time.Time
	ActiveTab        DashboardTab
	TopLines         []string
	LlamaLines       []string
	NvtopLines       []string
	StackItems       []StackItem
	SelectedStackIdx int
	Rules            StackRules
	ConfigPath       string

	SelectedQueueID  string // the queue row under the cursor; follows the chunk when the list reorders
	SelectedQueueIdx int
	QueuePicker      bool // choosing a model for the selected chunk
	QueuePickerIdx   int
	QueueFilter      QueueFilter // which statuses the queue tab lists
	QRHidden         bool        // ntfy QR panel on the Overview tab is on by default
	QueueHideDone    bool        // independent toggle: leave completed chunks out of the list

	FrontRunConfirm bool // the "do you want to front run with X?" dialog is open
	frontRunTier    string
	frontRunModel   string

	ntfyRotateArmed time.Time // first press of [n]; a second press within ntfyRotateWindow rotates
}

// ntfyRotateWindow is how long the first [n] press stays armed.
const ntfyRotateWindow = 5 * time.Second
