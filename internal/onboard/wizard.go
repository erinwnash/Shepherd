package onboard

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	wizReset     = "\x1b[0m"
	wizBold      = "\x1b[1m"
	wizDim       = "\x1b[2m"
	wizGreen     = "\x1b[92m"
	wizYellow    = "\x1b[93m"
	wizCyan      = "\x1b[96m"
	wizRed       = "\x1b[91m"
	wizMagenta   = "\x1b[95m"
	wizClearHome = "\x1b[2J\x1b[H"
)

// WizardState holds the interactive state of the first-run configuration wizard.
type WizardState struct {
	ProjectName            string
	Language               string
	Branch                 string
	Goal                   string
	AvailableRAMGB         float64
	NumGPUs                int
	RecommendedSlots       int
	Providers              []DiscoveredProvider
	SelectedIdx            int
	CloudFirstThreshold    float64
	MaxChunkCostUSD        float64
	MaxSprintCostUSD       float64
	RequireHighReviewHuman bool
	StatusMessage          string
	Saved                  bool
}

// NewWizardState initializes a WizardState from a detected Environment and default Config.
func NewWizardState(env *Environment, cfg Config) *WizardState {
	name := cfg.ProjectName
	if name == "" && env != nil {
		name = env.ProjectName
	}
	if name == "" {
		name = "shepherd-project"
	}

	goal := cfg.Goal
	if goal == "" {
		goal = "Analyze repository architecture, record ADRs, and generate atomic Sprint 1 work orders."
	}

	var providers []DiscoveredProvider
	if len(cfg.Providers) > 0 {
		providers = make([]DiscoveredProvider, len(cfg.Providers))
		copy(providers, cfg.Providers)
	} else if env != nil && len(env.Providers) > 0 {
		providers = make([]DiscoveredProvider, len(env.Providers))
		copy(providers, env.Providers)
	}
	for i := range providers {
		providers[i].Priority = i + 1
		if providers[i].UsageCapPct <= 0 {
			providers[i].UsageCapPct = 100
		}
		if providers[i].Attempts <= 0 {
			providers[i].Attempts = 2
		}
	}

	thresh := cfg.CloudFirstThreshold
	if thresh <= 0 {
		thresh = 0.50
	}

	lang := "Generic"
	branch := "main"
	var ram float64
	var gpus, slots int
	if env != nil {
		if env.Language != "" {
			lang = env.Language
		}
		if env.CurrentBranch != "" {
			branch = env.CurrentBranch
		}
		ram = env.AvailableRAMGB
		gpus = env.NumGPUs
		slots = env.RecommendedSlots
	}

	return &WizardState{
		ProjectName:            name,
		Language:               lang,
		Branch:                 branch,
		Goal:                   goal,
		AvailableRAMGB:         ram,
		NumGPUs:                gpus,
		RecommendedSlots:       slots,
		Providers:              providers,
		SelectedIdx:            0,
		CloudFirstThreshold:    thresh,
		MaxChunkCostUSD:        cfg.MaxChunkCostUSD,
		MaxSprintCostUSD:       cfg.MaxSprintCostUSD,
		RequireHighReviewHuman: cfg.RequireHighReviewHuman,
		StatusMessage:          "Use [j/k] to select, [Space] to toggle ON/OFF, [u/d] to prioritize, [t] to test, [Enter] to save.",
	}
}

// RenderWizard renders the First-Run Config Wizard view.
func RenderWizard(st *WizardState) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("%s%s┌──────────────────────────────────────────────────────────────────────────────────────┐%s\n", wizBold, wizCyan, wizReset))
	b.WriteString(fmt.Sprintf("%s%s│  SHEPHERD AI — FIRST-RUN ROUTER & AGENT CLI CONFIGURATION WIZARD                     │%s\n", wizBold, wizCyan, wizReset))
	b.WriteString(fmt.Sprintf("%s%s└──────────────────────────────────────────────────────────────────────────────────────┘%s\n", wizBold, wizCyan, wizReset))

	b.WriteString(fmt.Sprintf("  %sProject:%s %-22s %sStack:%s %-16s %sHardware:%s %.1fGB RAM | %d GPU (%d slots)\n\n",
		wizBold, wizReset, st.ProjectName,
		wizBold, wizReset, st.Language,
		wizBold, wizReset, st.AvailableRAMGB, st.NumGPUs, st.RecommendedSlots))

	b.WriteString(fmt.Sprintf("  %sDISCOVERED AI ROUTERS & CODING CLIS (Ordered by Priority):%s\n", wizBold, wizReset))
	b.WriteString(fmt.Sprintf("  %s%-3s %-4s %-6s %-8s %-26s %-20s %-14s %s%s\n",
		wizDim, "SEL", "PRIO", "STATE", "TYPE", "PROVIDER / ENDPOINT", "MODEL", "CAP / FLOOR", "STATUS", wizReset))
	b.WriteString(fmt.Sprintf("  %s──────────────────────────────────────────────────────────────────────────────────────%s\n", wizDim, wizReset))

	if len(st.Providers) == 0 {
		b.WriteString(fmt.Sprintf("  %sNo AI routers or CLIs detected yet. Default tier template will be scaffolded.%s\n", wizYellow, wizReset))
	} else {
		for i, p := range st.Providers {
			cursor := "   "
			rowPrefix := ""
			if i == st.SelectedIdx {
				cursor = wizBold + wizCyan + " > " + wizReset
				rowPrefix = wizBold
			}

			stateBadge := wizGreen + "[ON ]" + wizReset
			if !p.Enabled {
				stateBadge = wizDim + "[OFF]" + wizReset
			}

			kindBadge := "CLI   "
			if p.Kind == "router" {
				kindBadge = wizMagenta + "ROUTER" + wizReset
			}

			label := p.Name
			if p.Endpoint != "" {
				shortEP := strings.TrimPrefix(strings.TrimPrefix(p.Endpoint, "http://"), "https://")
				label = fmt.Sprintf("%s (%s)", p.ID, shortEP)
			} else {
				label = fmt.Sprintf("%s (%s)", p.ID, p.Adapter)
			}
			if len(label) > 26 {
				label = label[:23] + "..."
			}

			model := p.DefaultModel
			if model == "" {
				model = "auto"
			}
			if len(model) > 20 {
				model = model[:17] + "..."
			}

			capFloor := fmt.Sprintf("Cap:%3d%%", p.UsageCapPct)
			if p.FloorPct > 0 {
				capFloor = fmt.Sprintf("C:%d%% F:%d%%", p.UsageCapPct, p.FloorPct)
			}

			statusStr := p.Status
			if p.ProbeDetail != "" {
				statusStr = p.ProbeDetail
			}
			statusColor := wizGreen
			if !p.Enabled {
				statusColor = wizDim
			} else if strings.Contains(strings.ToLower(statusStr), "offline") || strings.Contains(strings.ToLower(statusStr), "fail") || strings.Contains(strings.ToLower(statusStr), "unreachable") {
				statusColor = wizYellow
			}

			b.WriteString(fmt.Sprintf("%s%s#%-3d %s  %-8s %-26s %-20s %-14s %s%s%s\n",
				cursor, rowPrefix, i+1, stateBadge, kindBadge, label, model, capFloor, statusColor, statusStr, wizReset))
		}
	}

	// Active Escalation Chain Preview
	var chainPreview []string
	for _, p := range st.Providers {
		if p.Enabled {
			att := p.Attempts
			if att <= 0 {
				att = 2
			}
			chainPreview = append(chainPreview, fmt.Sprintf("%s:%d", p.ID, att))
		}
	}
	chainStr := "none (enable at least one provider)"
	if len(chainPreview) > 0 {
		chainStr = strings.Join(chainPreview, " -> ")
	}

	b.WriteString(fmt.Sprintf("\n  %sActive Escalation Chain:%s %s%s%s\n", wizBold, wizReset, wizCyan, chainStr, wizReset))

	// Routing Rule & Budget Caps
	ruleDesc := "Balanced (0.50)"
	if st.CloudFirstThreshold >= 0.70 {
		ruleDesc = "Local-First (0.75 - prefer local router, escalate on failure)"
	} else if st.CloudFirstThreshold <= 0.30 {
		ruleDesc = "Cloud-First (0.25 - route medium/complex tasks straight to cloud)"
	}
	budgetDesc := "Unlimited ($0)"
	if st.MaxSprintCostUSD > 0 {
		budgetDesc = fmt.Sprintf("$%.2f / sprint ($%.2f / chunk)", st.MaxSprintCostUSD, st.MaxChunkCostUSD)
	}
	humanGate := "OFF"
	if st.RequireHighReviewHuman {
		humanGate = "ON (hold HIGH-risk diffs)"
	}

	b.WriteString(fmt.Sprintf("  %sRouting Rule [r]:%s %-46s %sHuman Gate [g]:%s %s\n",
		wizBold, wizReset, ruleDesc, wizBold, wizReset, humanGate))
	b.WriteString(fmt.Sprintf("  %sBudget Cap   [b]:%s %s\n", wizBold, wizReset, budgetDesc))

	if st.StatusMessage != "" {
		b.WriteString(fmt.Sprintf("\n  %sStatus:%s %s\n", wizBold+wizYellow, wizReset, st.StatusMessage))
	}

	b.WriteString(fmt.Sprintf("\n  %sControls:%s %s[j/k]%s Select  %s[Space]%s Toggle ON/OFF  %s[u/d]%s Move Priority  %s[m]%s Model  %s[c]%s Cap%%  %s[f]%s Floor%%\n",
		wizBold, wizReset,
		wizCyan, wizReset, wizCyan, wizReset, wizCyan, wizReset, wizCyan, wizReset, wizCyan, wizReset, wizCyan, wizReset))
	b.WriteString(fmt.Sprintf("            %s[t]%s Quick-Test  %s[T]%s Test All  %s[r]%s Routing Rule  %s[b]%s Budget Cap  %s[Enter/s]%s Save & Finish  %s[q]%s Quit\n",
		wizCyan, wizReset, wizCyan, wizReset, wizCyan, wizReset, wizCyan, wizReset, wizGreen+wizBold, wizReset, wizCyan, wizReset))
	b.WriteString(fmt.Sprintf("  %sTip:%s Press %s[t]%s to poll available models (or run 'agy models'), %s[m]%s to cycle active model.\n",
		wizDim, wizReset, wizCyan, wizReset, wizCyan, wizReset))

	return b.String()
}

// HandleWizardKey processes a single keypress in the wizard and returns true when the wizard should exit.
func HandleWizardKey(ctx context.Context, st *WizardState, key byte) bool {
	n := len(st.Providers)
	switch key {
	case 'q', 'Q', 3, 27: // q, Ctrl+C, Esc
		st.Saved = false
		st.StatusMessage = "Wizard cancelled."
		return true

	case 13, 10, 's', 'S': // Enter or 's' to save
		st.Saved = true
		for i := range st.Providers {
			st.Providers[i].Priority = i + 1
		}
		st.StatusMessage = "Configuration saved!"
		return true

	case 'j', 'J', 'n':
		if n > 0 {
			st.SelectedIdx = (st.SelectedIdx + 1) % n
		}
	case 'k', 'K', 'p':
		if n > 0 {
			st.SelectedIdx = (st.SelectedIdx - 1 + n) % n
		}

	case ' ', 'e', 'E': // Toggle ON/OFF
		if n > 0 && st.SelectedIdx >= 0 && st.SelectedIdx < n {
			st.Providers[st.SelectedIdx].Enabled = !st.Providers[st.SelectedIdx].Enabled
			stateWord := "ENABLED"
			if !st.Providers[st.SelectedIdx].Enabled {
				stateWord = "DEACTIVATED"
			}
			st.StatusMessage = fmt.Sprintf("Provider %q %s.", st.Providers[st.SelectedIdx].ID, stateWord)
		}

	case 'u', 'U', '+': // Move UP in priority
		if n > 1 && st.SelectedIdx > 0 {
			idx := st.SelectedIdx
			st.Providers[idx-1], st.Providers[idx] = st.Providers[idx], st.Providers[idx-1]
			for i := range st.Providers {
				st.Providers[i].Priority = i + 1
			}
			st.SelectedIdx = idx - 1
			st.StatusMessage = fmt.Sprintf("Moved %q UP to priority #%d.", st.Providers[st.SelectedIdx].ID, st.SelectedIdx+1)
		}

	case 'd', 'D', '-': // Move DOWN in priority
		if n > 1 && st.SelectedIdx < n-1 {
			idx := st.SelectedIdx
			st.Providers[idx+1], st.Providers[idx] = st.Providers[idx], st.Providers[idx+1]
			for i := range st.Providers {
				st.Providers[i].Priority = i + 1
			}
			st.SelectedIdx = idx + 1
			st.StatusMessage = fmt.Sprintf("Moved %q DOWN to priority #%d.", st.Providers[st.SelectedIdx].ID, st.SelectedIdx+1)
		}

	case 'm', 'M': // Cycle model on selected provider
		if n > 0 && st.SelectedIdx >= 0 && st.SelectedIdx < n {
			p := &st.Providers[st.SelectedIdx]
			if len(p.Models) == 0 && p.Adapter == "agy" {
				info := DetectAgyInfo(ctx)
				if len(info.Models) > 0 {
					p.Models = info.Models
				}
			}
			if len(p.Models) > 0 {
				curIdx := -1
				for i, m := range p.Models {
					if m == p.DefaultModel {
						curIdx = i
						break
					}
				}
				nextIdx := (curIdx + 1) % len(p.Models)
				p.DefaultModel = p.Models[nextIdx]
				st.StatusMessage = fmt.Sprintf("Set model for %q to %q.", p.ID, p.DefaultModel)
			} else {
				st.StatusMessage = fmt.Sprintf("Press [t] on %q to probe models or run '%s models'", p.ID, p.Adapter)
			}
		}

	case 'c', 'C': // Cycle Usage Cap %
		if n > 0 && st.SelectedIdx >= 0 && st.SelectedIdx < n {
			p := &st.Providers[st.SelectedIdx]
			caps := []int{100, 80, 60, 40, 25}
			next := 100
			for i, v := range caps {
				if p.UsageCapPct == v {
					next = caps[(i+1)%len(caps)]
					break
				}
			}
			p.UsageCapPct = next
			st.StatusMessage = fmt.Sprintf("Set usage cap for %q to %d%%.", p.ID, p.UsageCapPct)
		}

	case 'f', 'F': // Cycle Weekly Quota Floor %
		if n > 0 && st.SelectedIdx >= 0 && st.SelectedIdx < n {
			p := &st.Providers[st.SelectedIdx]
			floors := []int{0, 10, 20, 25, 40, 50}
			next := 25
			for i, v := range floors {
				if p.FloorPct == v {
					next = floors[(i+1)%len(floors)]
					break
				}
			}
			p.FloorPct = next
			st.StatusMessage = fmt.Sprintf("Set quota reserve floor for %q to %d%%.", p.ID, p.FloorPct)
		}

	case 't': // Quick-test selected provider
		if n > 0 && st.SelectedIdx >= 0 && st.SelectedIdx < n {
			p := &st.Providers[st.SelectedIdx]
			ok, lat, detail, models := ProbeProvider(ctx, *p)
			p.LatencyMs = lat
			p.ProbeDetail = detail
			if len(models) > 0 {
				p.Models = models
				if p.DefaultModel == "" {
					p.DefaultModel = models[0]
				}
			}
			if ok {
				p.Status = "online"
				st.StatusMessage = fmt.Sprintf("TEST PASS [%s]: %s", p.ID, detail)
			} else {
				p.Status = "offline"
				st.StatusMessage = fmt.Sprintf("TEST FAIL [%s]: %s", p.ID, detail)
			}
		}

	case 'T': // Quick-test ALL providers
		passed := 0
		for i := range st.Providers {
			p := &st.Providers[i]
			ok, lat, detail, models := ProbeProvider(ctx, *p)
			p.LatencyMs = lat
			p.ProbeDetail = detail
			if len(models) > 0 {
				p.Models = models
				if p.DefaultModel == "" {
					p.DefaultModel = models[0]
				}
			}
			if ok {
				p.Status = "online"
				passed++
			} else {
				p.Status = "offline"
			}
		}
		st.StatusMessage = fmt.Sprintf("Tested %d providers: %d online/ready.", n, passed)

	case 'r', 'R': // Cycle Routing Policy (Local-First / Balanced / Cloud-First)
		switch {
		case st.CloudFirstThreshold < 0.40:
			st.CloudFirstThreshold = 0.50
			st.StatusMessage = "Routing rule set to Balanced (cloud_first_threshold = 0.50)."
		case st.CloudFirstThreshold < 0.65:
			st.CloudFirstThreshold = 0.75
			st.StatusMessage = "Routing rule set to Local-First (cloud_first_threshold = 0.75)."
		default:
			st.CloudFirstThreshold = 0.25
			st.StatusMessage = "Routing rule set to Cloud-First (cloud_first_threshold = 0.25)."
		}

	case 'b', 'B': // Cycle Sprint Budget Cap
		switch {
		case st.MaxSprintCostUSD == 0:
			st.MaxSprintCostUSD = 5.0
			st.MaxChunkCostUSD = 1.0
		case st.MaxSprintCostUSD <= 5.0:
			st.MaxSprintCostUSD = 15.0
			st.MaxChunkCostUSD = 2.5
		case st.MaxSprintCostUSD <= 15.0:
			st.MaxSprintCostUSD = 50.0
			st.MaxChunkCostUSD = 5.0
		default:
			st.MaxSprintCostUSD = 0
			st.MaxChunkCostUSD = 0
		}
		if st.MaxSprintCostUSD == 0 {
			st.StatusMessage = "Budget cap set to Unlimited ($0)."
		} else {
			st.StatusMessage = fmt.Sprintf("Budget cap set to $%.2f/sprint ($%.2f/chunk).", st.MaxSprintCostUSD, st.MaxChunkCostUSD)
		}

	case 'g', 'G': // Toggle HIGH-risk human review gate
		st.RequireHighReviewHuman = !st.RequireHighReviewHuman
		st.StatusMessage = fmt.Sprintf("HIGH-risk human review gate set to %v.", st.RequireHighReviewHuman)
	}
	return false
}

// ApplyWizardToConfig copies the user's wizard selections back into onboard.Config.
func ApplyWizardToConfig(st *WizardState, base Config) Config {
	out := base
	if st.ProjectName != "" {
		out.ProjectName = st.ProjectName
	}
	if st.Goal != "" {
		out.Goal = st.Goal
	}
	out.Providers = make([]DiscoveredProvider, len(st.Providers))
	copy(out.Providers, st.Providers)
	out.CloudFirstThreshold = st.CloudFirstThreshold
	out.MaxChunkCostUSD = st.MaxChunkCostUSD
	out.MaxSprintCostUSD = st.MaxSprintCostUSD
	out.RequireHighReviewHuman = st.RequireHighReviewHuman

	// Pick highest-priority enabled CLI as TargetCLI for launch.md
	for _, p := range st.Providers {
		if p.Enabled {
			switch p.Adapter {
			case "claude-cli":
				out.TargetCLI = "claude"
			case "gemini-cli":
				out.TargetCLI = "gemini"
			default:
				out.TargetCLI = p.Adapter
			}
			break
		}
	}
	return out
}

// RunConfigWizard runs the full-screen interactive First-Run Config Wizard in the terminal.
func RunConfigWizard(ctx context.Context, r io.Reader, w io.Writer, env *Environment, defaultCfg Config) (Config, error) {
	if env == nil {
		env = DetectEnvironment(defaultCfg.ProjectRoot)
	}
	st := NewWizardState(env, defaultCfg)

	isTerm := false
	if f, ok := r.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		isTerm = true
		oldState, err := term.MakeRaw(int(f.Fd()))
		if err == nil {
			defer func() {
				_ = term.Restore(int(f.Fd()), oldState)
				fmt.Fprint(w, "\x1b[?25h\n")
			}()
			fmt.Fprint(w, "\x1b[?25l")
		}
	}

	render := func() {
		if isTerm {
			out := strings.ReplaceAll(RenderWizard(st), "\n", "\r\n")
			fmt.Fprint(w, wizClearHome+out)
		} else {
			fmt.Fprint(w, RenderWizard(st))
		}
	}

	render()

	buf := make([]byte, 1)
	escBuf := make([]byte, 2)
	for {
		select {
		case <-ctx.Done():
			return defaultCfg, ctx.Err()
		default:
		}
		n, err := r.Read(buf)
		if err != nil || n == 0 {
			break
		}
		key := buf[0]
		// Map ANSI arrow keys (ESC [ A / ESC [ B) to k / j
		if key == 27 && isTerm {
			if en, _ := r.Read(escBuf); en == 2 && escBuf[0] == '[' {
				switch escBuf[1] {
				case 'A':
					key = 'k'
				case 'B':
					key = 'j'
				}
			}
		}
		done := HandleWizardKey(ctx, st, key)
		render()
		if done {
			if !st.Saved {
				return defaultCfg, fmt.Errorf("wizard cancelled by user")
			}
			break
		}
	}

	return ApplyWizardToConfig(st, defaultCfg), nil
}
