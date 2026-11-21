package controller

import (
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/config"
)

func TestFallbackTierSkipsLimitedPool(t *testing.T) {
	c, _ := setupTestController(t, nil)
	c.cfg.Tiers = map[string]config.TierConfig{
		"cc":    {Adapter: "claude-cli", Pool: "cc"},
		"flash": {Adapter: "agy", Pool: "gemini"},
		"raon":  {Adapter: "opencode", Pool: "raon"},
	}
	c.cfg.Repair.Chain = []string{"flash:2", "raon:2", "cc:2"}

	if !c.noteLimit("X.1", "cc", "You've hit your session limit · resets 11pm (Asia/Singapore)") {
		t.Fatal("session limit not recognised")
	}
	if !c.tierLimited("cc") {
		t.Fatal("cc should be limited")
	}
	if name, _ := c.fallbackTier("cc"); name != "flash" {
		t.Fatalf("fallback = %q, want flash", name)
	}
	c.mu.Lock()
	c.limitedUntil["gemini"] = time.Now().Add(time.Hour)
	c.mu.Unlock()
	if name, _ := c.fallbackTier("cc"); name != "raon" {
		t.Fatalf("fallback = %q, want raon", name)
	}
	if c.noteLimit("X.1", "flash", "Gate failed (tests, exit 1)") {
		t.Fatal("ordinary failure treated as a limit")
	}
}
