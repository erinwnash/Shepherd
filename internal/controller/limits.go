package controller

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/erinwnash/Shepherd/internal/adapter"
	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/quota"
)

// poolOf names the quota pool a tier draws from (the tier itself when none is set).
func (c *Controller) poolOf(tierName string) string {
	if tc, ok := c.cfg.Tiers[tierName]; ok && tc.Pool != "" {
		return tc.Pool
	}
	return tierName
}

func (c *Controller) limitHit(text string) bool {
	ok, _ := quota.DetectLimit(text, time.Now())
	return ok
}

// noteLimit records a provider limit ("You've hit your session limit · resets 11pm") against
// the tier's pool until its reset time, so routing and repair stop choosing it. It reports
// whether text was such a rejection.
func (c *Controller) noteLimit(chunkID, tierName, text string) bool {
	now := time.Now()
	ok, until := quota.DetectLimit(text, now)
	if !ok {
		return false
	}
	pool := c.poolOf(tierName)
	c.mu.Lock()
	if c.limitedUntil == nil {
		c.limitedUntil = map[string]time.Time{}
	}
	c.limitedUntil[pool] = until
	c.mu.Unlock()
	_, _ = c.store.EventLog.Append("quota.exhausted", "quota", fmt.Sprintf("chunk %s: pool %q hit its provider limit; unavailable until %s", chunkID, pool, until.Local().Format("15:04 Mon")))
	return true
}

// tierLimited reports whether the tier's pool is still inside a recorded provider limit.
func (c *Controller) tierLimited(tierName string) bool {
	pool := c.poolOf(tierName)
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.limitedUntil[pool]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(c.limitedUntil, pool)
		return false
	}
	return true
}

// fallbackTier picks the first enabled tier outside exclude's pool that is not at a limit,
// following repair.chain order and then the remaining tiers by name.
func (c *Controller) fallbackTier(exclude string) (string, config.TierConfig) {
	var order []string
	seen := map[string]bool{}
	for _, step := range c.cfg.Repair.Chain {
		n := strings.Split(step, ":")[0]
		if !seen[n] {
			seen[n] = true
			order = append(order, n)
		}
	}
	var rest []string
	for n := range c.cfg.Tiers {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)
	for _, n := range order {
		tc, ok := c.cfg.Tiers[n]
		if !ok || !tc.IsEnabled() || n == exclude || c.poolOf(n) == c.poolOf(exclude) || c.tierLimited(n) {
			continue
		}
		return n, tc
	}
	return "", config.TierConfig{}
}

// adapterFor mirrors the controller's adapter lookup, defaulting like the primary path does.
func (c *Controller) adapterFor(name string) adapter.Adapter {
	if name == "" {
		name = "agy"
	}
	if ad, ok := c.adapters[name]; ok {
		return ad
	}
	if isLocalAdapter(name) {
		return adapter.NewOpenCodeAdapter()
	}
	return adapter.NewAgyAdapter()
}
