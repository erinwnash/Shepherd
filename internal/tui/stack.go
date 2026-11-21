package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/onboard"
)

func defaultCandidateModels(adapter, currentModel string) []string {
	var base []string
	switch strings.ToLower(adapter) {
	case "agy":
		base = []string{
			"gemini-3.8-flash-medium",
			"gemini-4-argon-medium",
			"claude-sonnet-5-5-medium",
			"claude-opus-5-5-medium",
			"gemini-3.8-flash-high",
			"gemini-4-argon-high",
			"gemini-3.8-flash-low",
			"gemini-4-argon-low",
			"gpt-oss-120b-medium",
			"sonnet",
			"opus",
		}
	case "claude-cli", "claude":
		base = []string{"sonnet", "opus", "haiku"}
	case "codex":
		base = []string{"gpt-5.6-luna", "gpt-5.6-terra", "o3"}
	case "aider":
		base = []string{"sonnet", "deepseek", "gpt-4o"}
	case "gemini-cli", "gemini":
		base = []string{"gemini-2.5-pro", "gemini-2.5-flash"}
	}
	if currentModel != "" {
		found := false
		for _, m := range base {
			if m == currentModel {
				found = true
				break
			}
		}
		if !found {
			base = append([]string{currentModel}, base...)
		}
	}
	return base
}

// SyncStackFromState populates or refreshes st.StackItems and st.Rules from the daemon Quota payload
// or from shepherd.toml on disk when the daemon is offline or has not yet reported quota state.
func SyncStackFromState(st *DashboardState) {
	if st.ConfigPath == "" {
		st.ConfigPath = "shepherd.toml"
	}

	// Preserve existing probe statuses & discovered models by tier name
	prevByTier := make(map[string]StackItem, len(st.StackItems))
	for _, item := range st.StackItems {
		prevByTier[item.Tier] = item
	}

	// 1. Try reading from shepherd.toml first if available so we have exact types, then overlay live daemon state
	var fileCfg *config.Config
	if _, err := os.Stat(st.ConfigPath); err == nil {
		fileCfg, _ = config.LoadFile(st.ConfigPath)
	}

	type rawTier struct {
		Adapter     string
		Model       string
		URL         string
		Pool        string
		Enabled     bool
		Priority    int
		UsageCapPct int
		Attempts    int
	}
	tiers := make(map[string]rawTier)
	poolFloors := make(map[string]int)
	poolModes := make(map[string]string)
	poolCaps := make(map[string]int)
	var chainOrder []string

	if fileCfg != nil {
		st.Rules = StackRules{
			CloudFirstThreshold:    fileCfg.Policy.CloudFirstThreshold,
			MaxChunkCostUSD:        fileCfg.Policy.MaxChunkCostUSD,
			MaxSprintCostUSD:       fileCfg.Policy.MaxSprintCostUSD,
			RequireHighReviewHuman: fileCfg.Policy.RequireHighReviewHuman,
		}
		for pName, pc := range fileCfg.Quota.Pools {
			poolFloors[pName] = pc.FloorWeeklyPct
			poolModes[pName] = pc.Mode
			poolCaps[pName] = pc.UsageCapPct
		}
		chainOrder = append(chainOrder, fileCfg.Repair.Chain...)
		for tName, tc := range fileCfg.Tiers {
			capPct := tc.UsageCapPct
			if capPct <= 0 {
				capPct = poolCaps[tc.Pool]
			}
			if capPct <= 0 {
				capPct = 100
			}
			tiers[tName] = rawTier{
				Adapter:     tc.Adapter,
				Model:       tc.Model,
				URL:         tc.URL,
				Pool:        tc.Pool,
				Enabled:     tc.IsEnabled(),
				Priority:    tc.Priority,
				UsageCapPct: capPct,
				Attempts:    2,
			}
		}
	}

	// 2. Overlay live daemon Quota map if present
	if len(st.Quota) > 0 {
		if rawPools, ok := st.Quota["pools"].(map[string]any); ok {
			for pName, pVal := range rawPools {
				if pMap, ok := pVal.(map[string]any); ok {
					if m, ok := stringField(pMap, "Mode", "mode"); ok && m != "" {
						poolModes[pName] = m
					}
					if f, ok := intField(pMap, "FloorWeeklyPct", "floor_weekly_pct"); ok {
						poolFloors[pName] = f
					}
					if c, ok := intField(pMap, "UsageCapPct", "usage_cap_pct"); ok && c > 0 {
						poolCaps[pName] = c
					}
				}
			}
		}
		if rawChain, ok := st.Quota["chain"].([]any); ok && len(rawChain) > 0 {
			chainOrder = nil
			for _, step := range rawChain {
				if s, ok := step.(string); ok {
					chainOrder = append(chainOrder, s)
				}
			}
		} else if rawChainStr, ok := st.Quota["chain"].([]string); ok && len(rawChainStr) > 0 {
			chainOrder = append([]string(nil), rawChainStr...)
		}

		if rawTiers, ok := st.Quota["tiers"].(map[string]any); ok {
			for tName, tVal := range rawTiers {
				if tMap, ok := tVal.(map[string]any); ok {
					rt := tiers[tName]
					if rt.UsageCapPct <= 0 {
						rt.UsageCapPct = 100
					}
					if rt.Attempts <= 0 {
						rt.Attempts = 2
					}
					rt.Enabled = true
					if a, ok := stringField(tMap, "Adapter", "adapter"); ok && a != "" {
						rt.Adapter = a
					}
					if m, ok := stringField(tMap, "Model", "model"); ok {
						rt.Model = m
					}
					if u, ok := stringField(tMap, "URL", "url"); ok {
						rt.URL = u
					}
					if p, ok := stringField(tMap, "Pool", "pool"); ok && p != "" {
						rt.Pool = p
					}
					if en, ok := boolField(tMap, "Enabled", "enabled"); ok {
						rt.Enabled = en
					}
					if pr, ok := intField(tMap, "Priority", "priority"); ok && pr > 0 {
						rt.Priority = pr
					}
					if cp, ok := intField(tMap, "UsageCapPct", "usage_cap_pct"); ok && cp > 0 {
						rt.UsageCapPct = cp
					}
					tiers[tName] = rt
				}
			}
		}

		if rawPolicy, ok := st.Quota["policy"].(map[string]any); ok {
			if cf, ok := floatField(rawPolicy, "CloudFirstThreshold", "cloud_first_threshold"); ok && cf > 0 {
				st.Rules.CloudFirstThreshold = cf
			}
			if mc, ok := floatField(rawPolicy, "MaxChunkCostUSD", "max_chunk_cost_usd"); ok {
				st.Rules.MaxChunkCostUSD = mc
			}
			if ms, ok := floatField(rawPolicy, "MaxSprintCostUSD", "max_sprint_cost_usd"); ok {
				st.Rules.MaxSprintCostUSD = ms
			}
			if hr, ok := boolField(rawPolicy, "RequireHighReviewHuman", "require_high_review_human"); ok {
				st.Rules.RequireHighReviewHuman = hr
			}
		}
	}

	// Apply live overrides from st.Status.Overrides if present
	for k, v := range st.Status.Overrides {
		switch k {
		case "policy.cloud_first_threshold":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				st.Rules.CloudFirstThreshold = f
			}
		case "policy.max_chunk_cost_usd":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				st.Rules.MaxChunkCostUSD = f
			}
		case "policy.max_sprint_cost_usd":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				st.Rules.MaxSprintCostUSD = f
			}
		case "policy.require_high_review_human":
			if b, err := strconv.ParseBool(v); err == nil {
				st.Rules.RequireHighReviewHuman = b
			}
		case "repair.chain":
			parts := strings.Split(v, ",")
			var clean []string
			for _, p := range parts {
				if strings.TrimSpace(p) != "" {
					clean = append(clean, strings.TrimSpace(p))
				}
			}
			if len(clean) > 0 {
				chainOrder = clean
			}
		default:
			if strings.HasPrefix(k, "tiers.") {
				rem := strings.TrimPrefix(k, "tiers.")
				parts := strings.SplitN(rem, ".", 2)
				if len(parts) == 2 {
					if rt, ok := tiers[parts[0]]; ok {
						switch parts[1] {
						case "enabled":
							if b, err := strconv.ParseBool(v); err == nil {
								rt.Enabled = b
								tiers[parts[0]] = rt
							}
						case "model":
							rt.Model = v
							tiers[parts[0]] = rt
						case "usage_cap_pct":
							if n, err := strconv.Atoi(v); err == nil {
								rt.UsageCapPct = n
								tiers[parts[0]] = rt
							}
						}
					}
				}
			} else if strings.HasPrefix(k, "quota.pools.") {
				rem := strings.TrimPrefix(k, "quota.pools.")
				parts := strings.SplitN(rem, ".", 2)
				if len(parts) == 2 && parts[1] == "floor_weekly_pct" {
					if n, err := strconv.Atoi(v); err == nil {
						poolFloors[parts[0]] = n
					}
				}
			}
		}
	}

	if st.Rules.CloudFirstThreshold <= 0 {
		st.Rules.CloudFirstThreshold = 0.50
	}

	// Ensure Claude Sonnet via agy option is available in the stack if agy CLI is installed
	if _, ok := tiers["sonnet"]; !ok {
		agyInfo := onboard.DetectAgyInfo(context.Background())
		if agyInfo.Installed {
			sonnetModel := "claude-sonnet-5-5-medium"
			for _, m := range agyInfo.Models {
				if strings.Contains(strings.ToLower(m), "sonnet") {
					sonnetModel = m
					break
				}
			}
			capPct := poolCaps["claude"]
			if capPct <= 0 {
				capPct = 100
			}
			tiers["sonnet"] = rawTier{
				Adapter:     "agy",
				Model:       sonnetModel,
				Pool:        "claude",
				Enabled:     false,
				Priority:    len(tiers) + 1,
				UsageCapPct: capPct,
				Attempts:    2,
			}
			if poolFloors["claude"] <= 0 {
				poolFloors["claude"] = 25
			}
			if poolModes["claude"] == "" {
				poolModes["claude"] = "polled"
			}
			if st.Quota == nil {
				st.Quota = make(map[string]any)
			}
			poolsMap, ok := st.Quota["pools"].(map[string]any)
			if !ok || poolsMap == nil {
				poolsMap = make(map[string]any)
				st.Quota["pools"] = poolsMap
			}
			if _, hasClaude := poolsMap["claude"]; !hasClaude {
				poolsMap["claude"] = map[string]any{
					"Mode":             "polled",
					"UsageCmd":         []string{"agy", "-p", "/usage"},
					"Parser":           "agy-usage",
					"FloorWeeklyPct":   float64(25),
					"mode":             "polled",
					"usage_cmd":        []string{"agy", "-p", "/usage"},
					"parser":           "agy-usage",
					"floor_weekly_pct": float64(25),
				}
			}
		}
	}

	// Surface the Raon test provider when RAON_API_KEY is set and it isn't configured yet
	if _, ok := tiers["raon"]; !ok && onboard.ResolveRaonKey() != "" {
		tiers["raon"] = rawTier{
			Adapter:     "opencode",
			Model:       "raon/" + onboard.RaonDefaultModel,
			URL:         onboard.RaonBaseURL + "/v1",
			Pool:        "raon",
			Enabled:     false,
			Priority:    len(tiers) + 1,
			UsageCapPct: 100,
			Attempts:    2,
		}
	}

	// Order tiers according to repair.chain first, then by Priority / name
	seen := make(map[string]bool)
	var orderedNames []string
	for _, step := range chainOrder {
		parts := strings.SplitN(step, ":", 2)
		tName := strings.TrimSpace(parts[0])
		if rt, ok := tiers[tName]; ok && !seen[tName] {
			if len(parts) == 2 {
				if att, err := strconv.Atoi(parts[1]); err == nil && att > 0 {
					rt.Attempts = att
					tiers[tName] = rt
				}
			}
			seen[tName] = true
			orderedNames = append(orderedNames, tName)
		}
	}
	var restNames []string
	for tName := range tiers {
		if !seen[tName] {
			restNames = append(restNames, tName)
		}
	}
	sort.Slice(restNames, func(i, j int) bool {
		pi, pj := tiers[restNames[i]].Priority, tiers[restNames[j]].Priority
		if pi > 0 && pj > 0 && pi != pj {
			return pi < pj
		}
		return restNames[i] < restNames[j]
	})
	orderedNames = append(orderedNames, restNames...)

	var items []StackItem
	for idx, tName := range orderedNames {
		rt := tiers[tName]
		prev := prevByTier[tName]
		models := prev.Models
		if len(models) == 0 {
			if rt.Adapter == "agy" {
				info := onboard.DetectAgyInfo(context.Background())
				if len(info.Models) > 0 {
					models = info.Models
				}
			}
			if len(models) == 0 {
				models = defaultCandidateModels(rt.Adapter, rt.Model)
			}
		}
		floor := poolFloors[rt.Pool]
		pMode := poolModes[rt.Pool]
		if pMode == "" {
			pMode = "until-rejected"
		}
		capPct := rt.UsageCapPct
		if capPct <= 0 {
			capPct = poolCaps[rt.Pool]
		}
		if capPct <= 0 {
			capPct = 100
		}
		att := rt.Attempts
		if att <= 0 {
			att = 2
		}
		items = append(items, StackItem{
			Tier:        tName,
			Adapter:     rt.Adapter,
			Model:       rt.Model,
			Models:      models,
			URL:         rt.URL,
			Pool:        rt.Pool,
			PoolMode:    pMode,
			Enabled:     rt.Enabled,
			Priority:    idx + 1,
			Attempts:    att,
			FloorPct:    floor,
			UsageCapPct: capPct,
			ProbeStatus: prev.ProbeStatus,
			ProbeOk:     prev.ProbeOk,
		})
	}

	if len(items) > 0 {
		st.StackItems = items
		if st.SelectedStackIdx >= len(st.StackItems) {
			st.SelectedStackIdx = len(st.StackItems) - 1
		}
		if st.SelectedStackIdx < 0 {
			st.SelectedStackIdx = 0
		}
	}
}

// PersistStackAndRules saves the current StackItems and StackRules to shepherd.toml (if it exists)
// and pushes live overrides to the controller daemon (if reachable).
func PersistStackAndRules(ctx context.Context, client *control.Client, st *DashboardState) {
	// Rebuild Status.Stack summary and repair.chain string
	var stackSummary []string
	var chainSteps []string
	for idx := range st.StackItems {
		st.StackItems[idx].Priority = idx + 1
		it := st.StackItems[idx]
		att := it.Attempts
		if att <= 0 {
			att = 2
		}
		if it.Enabled {
			chainSteps = append(chainSteps, fmt.Sprintf("%s:%d", it.Tier, att))
		}
		desc := fmt.Sprintf("%s (%s", it.Tier, it.Adapter)
		if it.Model != "" {
			desc += ":" + it.Model
		}
		if it.URL != "" {
			desc += " @ " + it.URL
		}
		desc += ")"
		if !it.Enabled {
			desc += " [OFF]"
		}
		stackSummary = append(stackSummary, desc)
	}
	st.Status.Stack = stackSummary

	// 1. Update shepherd.toml on disk if present
	cfgPath := st.ConfigPath
	if cfgPath == "" {
		cfgPath = "shepherd.toml"
	}
	if fileCfg, err := config.LoadFile(cfgPath); err == nil {
		if fileCfg.Tiers == nil {
			fileCfg.Tiers = make(map[string]config.TierConfig)
		}
		if fileCfg.Quota.Pools == nil {
			fileCfg.Quota.Pools = make(map[string]config.PoolConfig)
		}
		for _, it := range st.StackItems {
			tc := fileCfg.Tiers[it.Tier]
			tc.Adapter = it.Adapter
			tc.Model = it.Model
			tc.URL = it.URL
			tc.Pool = it.Pool
			tc.Enabled = config.BoolPtr(it.Enabled)
			tc.Priority = it.Priority
			tc.UsageCapPct = it.UsageCapPct
			fileCfg.Tiers[it.Tier] = tc

			if it.Pool != "" {
				pc, hasPool := fileCfg.Quota.Pools[it.Pool]
				if !hasPool {
					pc = config.PoolConfig{
						Mode:           it.PoolMode,
						FloorWeeklyPct: it.FloorPct,
						UsageCapPct:    it.UsageCapPct,
					}
					if it.Adapter == "agy" {
						pc.Mode = "polled"
						pc.UsageCmd = []string{"agy", "-p", "/usage"}
						pc.Parser = "agy-usage"
						if pc.FloorWeeklyPct <= 0 {
							pc.FloorWeeklyPct = 25
						}
					}
				} else {
					if it.FloorPct > 0 || pc.Mode == "polled" {
						pc.FloorWeeklyPct = it.FloorPct
					}
					pc.UsageCapPct = it.UsageCapPct
				}
				fileCfg.Quota.Pools[it.Pool] = pc
			}
		}
		if len(chainSteps) > 0 {
			fileCfg.Repair.Chain = chainSteps
		}
		fileCfg.Policy.CloudFirstThreshold = st.Rules.CloudFirstThreshold
		fileCfg.Policy.MaxChunkCostUSD = st.Rules.MaxChunkCostUSD
		fileCfg.Policy.MaxSprintCostUSD = st.Rules.MaxSprintCostUSD
		fileCfg.Policy.RequireHighReviewHuman = st.Rules.RequireHighReviewHuman
		_ = config.SaveFile(cfgPath, fileCfg)
	}

	// 2. Push live updates to daemon if client is available
	if client != nil {
		if len(chainSteps) > 0 {
			_ = client.ConfigSet(ctx, "repair.chain", strings.Join(chainSteps, ","))
		}
		_ = client.ConfigSet(ctx, "policy.cloud_first_threshold", fmt.Sprintf("%.2f", st.Rules.CloudFirstThreshold))
		_ = client.ConfigSet(ctx, "policy.max_chunk_cost_usd", fmt.Sprintf("%.2f", st.Rules.MaxChunkCostUSD))
		_ = client.ConfigSet(ctx, "policy.max_sprint_cost_usd", fmt.Sprintf("%.2f", st.Rules.MaxSprintCostUSD))
		_ = client.ConfigSet(ctx, "policy.require_high_review_human", strconv.FormatBool(st.Rules.RequireHighReviewHuman))
	}
}

// MoveStackPriority moves the currently selected StackItem up (delta=-1) or down (delta=+1) in priority.
func MoveStackPriority(ctx context.Context, client *control.Client, st *DashboardState, delta int) {
	n := len(st.StackItems)
	if n <= 1 {
		return
	}
	idx := st.SelectedStackIdx
	target := idx + delta
	if target < 0 || target >= n {
		return
	}
	st.StackItems[idx], st.StackItems[target] = st.StackItems[target], st.StackItems[idx]
	st.SelectedStackIdx = target
	PersistStackAndRules(ctx, client, st)
	dir := "UP"
	if delta > 0 {
		dir = "DOWN"
	}
	st.LastAction = fmt.Sprintf("moved %q %s to priority #%d", st.StackItems[target].Tier, dir, target+1)
	st.ActionErr = ""
}

// ToggleStackItem activates or deactivates the selected StackItem.
func ToggleStackItem(ctx context.Context, client *control.Client, st *DashboardState) {
	if len(st.StackItems) == 0 || st.SelectedStackIdx < 0 || st.SelectedStackIdx >= len(st.StackItems) {
		return
	}
	it := &st.StackItems[st.SelectedStackIdx]
	it.Enabled = !it.Enabled
	if client != nil {
		_ = client.QuotaTierToggle(ctx, it.Tier, it.Enabled)
	}
	PersistStackAndRules(ctx, client, st)
	stateStr := "ACTIVATED"
	if !it.Enabled {
		stateStr = "DEACTIVATED"
	}
	st.LastAction = fmt.Sprintf("tier %q %s", it.Tier, stateStr)
	st.ActionErr = ""
}

// CycleStackModel cycles through candidate models for the selected StackItem.
func CycleStackModel(ctx context.Context, client *control.Client, st *DashboardState) {
	if len(st.StackItems) == 0 || st.SelectedStackIdx < 0 || st.SelectedStackIdx >= len(st.StackItems) {
		return
	}
	it := &st.StackItems[st.SelectedStackIdx]
	if len(it.Models) == 0 {
		it.Models = defaultCandidateModels(it.Adapter, it.Model)
	}
	if len(it.Models) == 0 {
		st.LastAction = fmt.Sprintf("press [t] on %q to discover models from endpoint", it.Tier)
		return
	}
	cur := -1
	for i, m := range it.Models {
		if m == it.Model {
			cur = i
			break
		}
	}
	it.Model = it.Models[(cur+1)%len(it.Models)]
	if client != nil {
		_ = client.ConfigSet(ctx, fmt.Sprintf("tiers.%s.model", it.Tier), it.Model)
	}
	PersistStackAndRules(ctx, client, st)
	st.LastAction = fmt.Sprintf("set %q model to %q", it.Tier, it.Model)
	st.ActionErr = ""
}

// CycleStackUsageCap cycles the usage cap percentage for the selected StackItem.
func CycleStackUsageCap(ctx context.Context, client *control.Client, st *DashboardState) {
	if len(st.StackItems) == 0 || st.SelectedStackIdx < 0 || st.SelectedStackIdx >= len(st.StackItems) {
		return
	}
	it := &st.StackItems[st.SelectedStackIdx]
	caps := []int{100, 80, 60, 40, 25}
	next := 100
	for i, v := range caps {
		if it.UsageCapPct == v {
			next = caps[(i+1)%len(caps)]
			break
		}
	}
	it.UsageCapPct = next
	if client != nil {
		_ = client.ConfigSet(ctx, fmt.Sprintf("tiers.%s.usage_cap_pct", it.Tier), strconv.Itoa(it.UsageCapPct))
	}
	PersistStackAndRules(ctx, client, st)
	st.LastAction = fmt.Sprintf("set %q usage cap to %d%%", it.Tier, it.UsageCapPct)
	st.ActionErr = ""
}

// CycleStackFloor cycles the weekly quota reserve floor percentage for the selected StackItem.
func CycleStackFloor(ctx context.Context, client *control.Client, st *DashboardState) {
	if len(st.StackItems) == 0 || st.SelectedStackIdx < 0 || st.SelectedStackIdx >= len(st.StackItems) {
		return
	}
	it := &st.StackItems[st.SelectedStackIdx]
	floors := []int{0, 10, 20, 25, 40, 50}
	next := 25
	for i, v := range floors {
		if it.FloorPct == v {
			next = floors[(i+1)%len(floors)]
			break
		}
	}
	it.FloorPct = next
	if client != nil && it.Pool != "" {
		_ = client.QuotaSetFloor(ctx, it.Pool, it.FloorPct)
	}
	PersistStackAndRules(ctx, client, st)
	st.LastAction = fmt.Sprintf("set %q (pool %s) weekly quota floor to %d%%", it.Tier, it.Pool, it.FloorPct)
	st.ActionErr = ""
}

// CycleRoutingRule cycles the cloud_first_threshold routing rule (Local-First / Balanced / Cloud-First).
func CycleRoutingRule(ctx context.Context, client *control.Client, st *DashboardState) {
	switch {
	case st.Rules.CloudFirstThreshold < 0.40:
		st.Rules.CloudFirstThreshold = 0.50
		st.LastAction = "routing rule: Balanced (cloud_first_threshold = 0.50)"
	case st.Rules.CloudFirstThreshold < 0.65:
		st.Rules.CloudFirstThreshold = 0.75
		st.LastAction = "routing rule: Local-First (cloud_first_threshold = 0.75)"
	default:
		st.Rules.CloudFirstThreshold = 0.25
		st.LastAction = "routing rule: Cloud-First (cloud_first_threshold = 0.25)"
	}
	PersistStackAndRules(ctx, client, st)
	st.ActionErr = ""
}

// CycleBudgetCap cycles sprint and chunk cost caps.
func CycleBudgetCap(ctx context.Context, client *control.Client, st *DashboardState) {
	switch {
	case st.Rules.MaxSprintCostUSD == 0:
		st.Rules.MaxSprintCostUSD = 5.0
		st.Rules.MaxChunkCostUSD = 1.0
	case st.Rules.MaxSprintCostUSD <= 5.0:
		st.Rules.MaxSprintCostUSD = 15.0
		st.Rules.MaxChunkCostUSD = 2.5
	case st.Rules.MaxSprintCostUSD <= 15.0:
		st.Rules.MaxSprintCostUSD = 50.0
		st.Rules.MaxChunkCostUSD = 5.0
	default:
		st.Rules.MaxSprintCostUSD = 0
		st.Rules.MaxChunkCostUSD = 0
	}
	PersistStackAndRules(ctx, client, st)
	if st.Rules.MaxSprintCostUSD == 0 {
		st.LastAction = "usage budget cap set to Unlimited ($0)"
	} else {
		st.LastAction = fmt.Sprintf("usage budget cap set to $%.2f/sprint ($%.2f/chunk)", st.Rules.MaxSprintCostUSD, st.Rules.MaxChunkCostUSD)
	}
	st.ActionErr = ""
}

// ToggleHumanGate toggles the HIGH-review human sign-off policy rule.
func ToggleHumanGate(ctx context.Context, client *control.Client, st *DashboardState) {
	st.Rules.RequireHighReviewHuman = !st.Rules.RequireHighReviewHuman
	PersistStackAndRules(ctx, client, st)
	st.LastAction = fmt.Sprintf("HIGH-risk human review gate set to %v", st.Rules.RequireHighReviewHuman)
	st.ActionErr = ""
}

// QuickTestSelected probes the currently selected AI router or CLI live.
func QuickTestSelected(ctx context.Context, st *DashboardState) {
	if len(st.StackItems) == 0 || st.SelectedStackIdx < 0 || st.SelectedStackIdx >= len(st.StackItems) {
		return
	}
	it := &st.StackItems[st.SelectedStackIdx]
	ok, _, detail, models := onboard.ProbeProvider(ctx, onboard.DiscoveredProvider{
		ID:           it.Tier,
		Adapter:      it.Adapter,
		Endpoint:     it.URL,
		DefaultModel: it.Model,
	})
	it.ProbeOk = ok
	if len(models) > 0 {
		it.Models = models
		if it.Model == "" {
			it.Model = models[0]
		}
	}
	if ok {
		it.ProbeStatus = "PASS: " + detail
		st.LastAction = fmt.Sprintf("quick-test %q PASSED: %s", it.Tier, detail)
		st.ActionErr = ""
	} else {
		it.ProbeStatus = "FAIL: " + detail
		st.ActionErr = fmt.Sprintf("quick-test %q FAILED: %s", it.Tier, detail)
	}
}

// QuickTestAll probes all AI routers and CLIs in the stack concurrently.
func QuickTestAll(ctx context.Context, st *DashboardState) {
	passed := 0
	for i := range st.StackItems {
		it := &st.StackItems[i]
		ok, _, detail, models := onboard.ProbeProvider(ctx, onboard.DiscoveredProvider{
			ID:           it.Tier,
			Adapter:      it.Adapter,
			Endpoint:     it.URL,
			DefaultModel: it.Model,
		})
		it.ProbeOk = ok
		if len(models) > 0 {
			it.Models = models
			if it.Model == "" {
				it.Model = models[0]
			}
		}
		if ok {
			it.ProbeStatus = "PASS: " + detail
			passed++
		} else {
			it.ProbeStatus = "FAIL: " + detail
		}
	}
	st.LastAction = fmt.Sprintf("quick-tested %d stack items: %d passed", len(st.StackItems), passed)
	st.ActionErr = ""
}

func stringField(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return v, true
		}
	}
	return "", false
}

func intField(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case int:
			return v, true
		case int64:
			return int(v), true
		case float64:
			return int(v), true
		}
	}
	return 0, false
}

func floatField(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return v, true
		case int:
			return float64(v), true
		}
	}
	return 0, false
}

func boolField(m map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v, true
		}
	}
	return false, false
}
