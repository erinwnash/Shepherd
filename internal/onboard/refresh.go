package onboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/erinwnash/Shepherd/internal/config"
)

// RefreshResult describes what RefreshProviders changed (or would change in dry-run).
type RefreshResult struct {
	TOMLPath   string
	AddedTiers []string // newly appended tier IDs (disabled, lowest priority)
	AddedPools []string // newly appended quota pools
	Stale      []string // existing tiers whose model is no longer offered by the live provider
	Unchanged  int      // discovered providers already present in shepherd.toml
	Appended   string   // exact text appended
}

// RefreshProviders re-runs provider discovery against an already-onboarded project and
// APPENDS only providers missing from shepherd.toml. Existing tiers, priorities, the repair
// chain, pools, comments and all other settings are left byte-for-byte untouched. New tiers
// are added disabled and outside the repair chain so nothing changes until the operator opts in.
func RefreshProviders(ctx context.Context, root string, dryRun bool) (*RefreshResult, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	tomlPath := filepath.Join(absRoot, "shepherd.toml")
	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		return nil, fmt.Errorf("no shepherd.toml in %s (run 'shepherd onboard' first): %w", absRoot, err)
	}
	cfg, err := config.LoadBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("existing shepherd.toml is invalid: %w", err)
	}

	res := &RefreshResult{TOMLPath: tomlPath}
	maxPrio := len(cfg.Tiers)
	for _, tc := range cfg.Tiers {
		if tc.Priority > maxPrio {
			maxPrio = tc.Priority
		}
	}

	var sb strings.Builder
	addedPools := map[string]bool{}
	for _, p := range DiscoverProviders(ctx, absRoot) {
		// Discovery de-dups colliding IDs as "<id>_2"; match on the base ID too.
		baseID := p.ID
		if i := strings.LastIndex(baseID, "_"); i > 0 && strings.Trim(baseID[i+1:], "0123456789") == "" {
			baseID = baseID[:i]
		}
		if existing, ok := cfg.Tiers[p.ID]; ok {
			res.Unchanged++
			if existing.Model != "" && len(p.Models) > 0 && p.Status == "online" && !containsStr(p.Models, existing.Model) {
				res.Stale = append(res.Stale, fmt.Sprintf("%s (model %q not offered; available: %s)", p.ID, existing.Model, strings.Join(p.Models, ", ")))
			}
			continue
		}
		if _, ok := cfg.Tiers[baseID]; ok && baseID != p.ID {
			res.Unchanged++
			continue
		}

		maxPrio++
		poolName := p.Pool
		if poolName == "" {
			poolName = p.ID
		}
		capPct := p.UsageCapPct
		if capPct <= 0 {
			capPct = 100
		}
		fmt.Fprintf(&sb, "\n[tiers.%s]\nadapter = %q\n", p.ID, p.Adapter)
		if p.DefaultModel != "" {
			fmt.Fprintf(&sb, "model = %q\n", p.DefaultModel)
		}
		if p.Endpoint != "" {
			fmt.Fprintf(&sb, "url = %q\n", p.Endpoint)
		}
		fmt.Fprintf(&sb, "pool = %q\nenabled = false\npriority = %d\nusage_cap_pct = %d\n", poolName, maxPrio, capPct)
		res.AddedTiers = append(res.AddedTiers, p.ID)

		if _, ok := cfg.Quota.Pools[poolName]; !ok && !addedPools[poolName] {
			addedPools[poolName] = true
			res.AddedPools = append(res.AddedPools, poolName)
			if p.PoolMode == "polled" {
				bin, parser := "agy", "agy-usage"
				if p.Adapter == "claude-cli" {
					bin, parser = "claude", "claude-usage"
				}
				floor := p.FloorPct
				if floor <= 0 {
					floor = 25
				}
				fmt.Fprintf(&sb, "\n[quota.pools.%s]\nmode = \"polled\"\nusage_cmd = [%q, \"-p\", \"/usage\"]\nparser = %q\nfloor_weekly_pct = %d\nusage_cap_pct = %d\n", poolName, bin, parser, floor, capPct)
			} else {
				fmt.Fprintf(&sb, "\n[quota.pools.%s]\nmode = \"until-rejected\"\nbackoff_min = [10, 30]\nusage_cap_pct = %d\n", poolName, capPct)
			}
		}
	}
	sort.Strings(res.Stale)
	res.Appended = sb.String()
	if res.Appended == "" {
		return res, nil
	}

	updated := string(raw)
	if !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	updated += "\n# --- added by 'shepherd onboard --refresh' (disabled; enable via dashboard stack tab) ---" + res.Appended
	if _, err := config.LoadBytes([]byte(updated)); err != nil {
		return nil, fmt.Errorf("refresh would produce an invalid shepherd.toml (nothing written): %w", err)
	}
	if dryRun {
		return res, nil
	}
	if err := os.WriteFile(tomlPath, []byte(updated), 0644); err != nil {
		return nil, fmt.Errorf("write shepherd.toml: %w", err)
	}
	return res, nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
