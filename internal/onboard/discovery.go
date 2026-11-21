package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/quota"
)

// DiscoveredProvider represents a compliant AI router, local inference server, or agent CLI
// discovered on the host machine.
type DiscoveredProvider struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"` // "router" | "cli"
	Adapter      string   `json:"adapter"`
	BinaryPath   string   `json:"binary_path,omitempty"`
	Endpoint     string   `json:"endpoint,omitempty"`
	DefaultModel string   `json:"default_model,omitempty"`
	Models       []string `json:"models,omitempty"`
	Pool         string   `json:"pool"`
	PoolMode     string   `json:"pool_mode"` // "polled" | "until-rejected"
	FloorPct     int      `json:"floor_pct"`
	UsageCapPct  int      `json:"usage_cap_pct"`
	Attempts     int      `json:"attempts"`
	Enabled      bool     `json:"enabled"`
	Priority     int      `json:"priority"`
	Source       string   `json:"source"`
	Status       string   `json:"status"` // "online" | "ready" | "offline" | "untested"
	LatencyMs    int64    `json:"latency_ms,omitempty"`
	ProbeDetail  string   `json:"probe_detail,omitempty"`
}

// Raon hosted inference (https://raon-mini.krafton.ai/en/docs); key via RAON_API_KEY.
const (
	RaonBaseURL      = "https://raon-api.krafton.ai"
	RaonDefaultModel = "raon-1-mini-preview"
)

// ResolveRaonKey returns the Raon API key from $RAON_API_KEY, falling back to the "raon"
// provider's options.apiKey in the global OpenCode config (~/.config/opencode/opencode.json),
// so Shepherd works wherever OpenCode already does.
func ResolveRaonKey() string {
	if k := strings.TrimSpace(os.Getenv("RAON_API_KEY")); k != "" {
		return k
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		return ""
	}
	var rc struct {
		Provider map[string]struct {
			Options struct {
				APIKey string `json:"apiKey"`
			} `json:"options"`
		} `json:"provider"`
	}
	if json.Unmarshal(data, &rc) != nil {
		return ""
	}
	k := strings.TrimSpace(rc.Provider["raon"].Options.APIKey)
	if strings.HasPrefix(k, "{env:") {
		return ""
	}
	return k
}

type routerProbeTarget struct {
	ID      string
	Name    string
	BaseURL string
	Source  string
}

// StandardRouterTargets lists well-known local OpenAI-compatible AI routers and inference servers.
var StandardRouterTargets = []routerProbeTarget{
	{ID: "ollama", Name: "Ollama Local Router", BaseURL: "http://127.0.0.1:11434/v1", Source: "probe :11434"},
	{ID: "local", Name: "llama.cpp / LocalAI Router", BaseURL: "http://127.0.0.1:8080/v1", Source: "probe :8080"},
	{ID: "lmstudio", Name: "LM Studio Router", BaseURL: "http://127.0.0.1:1234/v1", Source: "probe :1234"},
	{ID: "vllm", Name: "vLLM OpenAI Server", BaseURL: "http://127.0.0.1:8000/v1", Source: "probe :8000"},
	{ID: "litellm", Name: "LiteLLM AI Router", BaseURL: "http://127.0.0.1:4000/v1", Source: "probe :4000"},
	{ID: "opencode_srv", Name: "OpenCode Local Server", BaseURL: "http://127.0.0.1:4096/v1", Source: "probe :4096"},
	{ID: "jan", Name: "Jan / Cortex Local Router", BaseURL: "http://127.0.0.1:1337/v1", Source: "probe :1337"},
}

func normalizeBaseURL(u string) string {
	u = strings.TrimSpace(strings.TrimRight(u, "/"))
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	if !strings.HasSuffix(u, "/v1") && !strings.Contains(u, "/v1/") {
		u = u + "/v1"
	}
	return u
}

// ProbeOpenAIRouter queries an OpenAI-compatible /v1/models endpoint and returns discovered model IDs and latency.
func ProbeOpenAIRouter(ctx context.Context, baseURL string, timeout time.Duration) (bool, int64, []string, string) {
	return ProbeOpenAIRouterWithKey(ctx, baseURL, os.Getenv("OPENAI_API_KEY"), timeout)
}

// ProbeOpenAIRouterWithKey is ProbeOpenAIRouter with an explicit bearer API key (empty = none).
func ProbeOpenAIRouterWithKey(ctx context.Context, baseURL, apiKey string, timeout time.Duration) (bool, int64, []string, string) {
	if timeout <= 0 {
		timeout = 350 * time.Millisecond
	}
	normURL := normalizeBaseURL(baseURL)
	if normURL == "" {
		return false, 0, nil, "empty endpoint URL"
	}
	modelsURL := strings.TrimRight(normURL, "/") + "/models"

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return false, 0, nil, err.Error()
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return false, latency, nil, err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, latency, nil, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"models"`
	}
	var models []string
	if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil {
		for _, m := range payload.Data {
			if strings.TrimSpace(m.ID) != "" {
				models = append(models, strings.TrimSpace(m.ID))
			}
		}
		for _, m := range payload.Models {
			if strings.TrimSpace(m.ID) != "" {
				models = append(models, strings.TrimSpace(m.ID))
			} else if strings.TrimSpace(m.Name) != "" {
				models = append(models, strings.TrimSpace(m.Name))
			}
		}
	}

	detail := fmt.Sprintf("HTTP 200 (%dms)", latency)
	if len(models) > 0 {
		detail = fmt.Sprintf("HTTP 200 (%dms, %d models)", latency, len(models))
	}
	return true, latency, models, detail
}

// AgyInfo describes detected Antigravity CLI configuration and polled models.
type AgyInfo struct {
	Installed   bool              `json:"installed"`
	BinaryPath  string            `json:"binary_path,omitempty"`
	ConfigPath  string            `json:"config_path,omitempty"`
	ActiveModel string            `json:"active_model,omitempty"`
	ActiveTitle string            `json:"active_title,omitempty"`
	Models      []string          `json:"models,omitempty"`
	ModelTitles map[string]string `json:"model_titles,omitempty"`
}

// CleanCLIOutput strips ANSI escape codes and progress spinners from CLI stdout.
func CleanCLIOutput(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if (s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z') || s[i] == '~' {
				inEsc = false
			}
			continue
		}
		// skip unicode spinner characters (U+2800..U+28FF)
		if i+2 < len(s) && s[i] == 0xe2 && s[i+1] == 0xa0 {
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// DetectAgyInfo inspects the system for Antigravity CLI (agy), reads ~/.gemini/antigravity-cli/settings.json
// to detect the user's active/configured model (e.g. Gemini 3.8 Flash), and executes 'agy models' to poll available models.
func DetectAgyInfo(ctx context.Context) AgyInfo {
	info := AgyInfo{
		ModelTitles: make(map[string]string),
	}
	binPath, err := LookCLIPath("agy")
	if err != nil {
		return info
	}
	info.Installed = true
	info.BinaryPath = binPath

	// 1. Check settings.json for active/preferred model
	var settingsPaths []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		settingsPaths = append(settingsPaths,
			filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"),
			filepath.Join(home, ".gemini", "settings.json"),
		)
	}
	settingsPaths = append(settingsPaths,
		filepath.Join(".gemini", "antigravity-cli", "settings.json"),
		filepath.Join(".gemini", "settings.json"),
	)

	var configuredModel string
	for _, sp := range settingsPaths {
		if data, err := os.ReadFile(sp); err == nil {
			var s struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(data, &s) == nil && s.Model != "" {
				info.ConfigPath = sp
				configuredModel = strings.TrimSpace(s.Model)
				info.ActiveTitle = configuredModel
				break
			}
		}
	}

	// 2. Poll models from 'agy models'
	pollCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pollCtx, binPath, "models")
	out, err := cmd.CombinedOutput()
	if err == nil {
		clean := CleanCLIOutput(string(out))
		lines := strings.Split(clean, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "Fetching") {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) >= 1 {
				mID := parts[0]
				title := mID
				if len(parts) > 1 {
					title = strings.TrimSpace(strings.TrimPrefix(line, mID))
				}
				info.Models = append(info.Models, mID)
				info.ModelTitles[mID] = title
			}
		}
	}

	// Fallback known models if agy models didn't return any
	if len(info.Models) == 0 {
		info.Models = []string{
			"gemini-3.8-flash-medium",
			"gemini-4-argon-medium",
			"gemini-3.8-flash-high",
			"gemini-4-argon-high",
			"gemini-3.8-flash-low",
			"gemini-4-argon-low",
			"claude-sonnet-5-5-medium",
			"claude-opus-5-5-medium",
			"gpt-oss-120b-medium",
		}
		info.ModelTitles["gemini-3.8-flash-medium"] = "Gemini 3.8 Flash (Medium)"
		info.ModelTitles["gemini-4-argon-medium"] = "Gemini 4 Argon (Medium)"
		info.ModelTitles["claude-sonnet-5-5-medium"] = "Claude Sonnet 5.5 (Medium)"
		info.ModelTitles["claude-opus-5-5-medium"] = "Claude Opus 5.5 (Medium)"
	}

	// 3. Resolve active model ID from configuredModel title
	if configuredModel != "" {
		cfgLower := strings.ToLower(configuredModel)
		for mID, title := range info.ModelTitles {
			if strings.EqualFold(mID, configuredModel) || strings.EqualFold(title, configuredModel) {
				info.ActiveModel = mID
				break
			}
			if strings.Contains(strings.ToLower(title), cfgLower) || strings.Contains(cfgLower, strings.ToLower(mID)) {
				info.ActiveModel = mID
				break
			}
		}
		if info.ActiveModel == "" {
			if strings.Contains(cfgLower, "flash") {
				for _, m := range info.Models {
					if strings.Contains(strings.ToLower(m), "flash") {
						info.ActiveModel = m
						break
					}
				}
			} else if strings.Contains(cfgLower, "argon") {
				for _, m := range info.Models {
					if strings.Contains(strings.ToLower(m), "argon") {
						info.ActiveModel = m
						break
					}
				}
			} else if strings.Contains(cfgLower, "sonnet") {
				for _, m := range info.Models {
					if strings.Contains(strings.ToLower(m), "sonnet") {
						info.ActiveModel = m
						break
					}
				}
			} else if strings.Contains(cfgLower, "opus") {
				for _, m := range info.Models {
					if strings.Contains(strings.ToLower(m), "opus") {
						info.ActiveModel = m
						break
					}
				}
			}
		}
	}

	if info.ActiveModel == "" {
		// Default to flash if available
		for _, m := range info.Models {
			if strings.Contains(strings.ToLower(m), "flash") {
				info.ActiveModel = m
				break
			}
		}
		if info.ActiveModel == "" && len(info.Models) > 0 {
			info.ActiveModel = info.Models[0]
		}
	}

	return info
}

// ProbeProvider tests a DiscoveredProvider (either an HTTP AI router or a local CLI binary)
// and returns whether it is responsive, latency in ms, a human-readable status detail, and any discovered models.
func ProbeProvider(ctx context.Context, p DiscoveredProvider) (bool, int64, string, []string) {
	if p.Endpoint != "" {
		ok, lat, models, detail := ProbeOpenAIRouter(ctx, p.Endpoint, 1200*time.Millisecond)
		if ok {
			return true, lat, detail, models
		}
		// If it's OpenCode CLI and the local endpoint isn't running right now, also check if the CLI binary itself works
		if p.Adapter == "opencode" {
			if binPath, err := LookCLIPath("opencode"); err == nil {
				return true, lat, fmt.Sprintf("CLI ready (%s); router %s offline (%s)", binPath, p.Endpoint, detail), nil
			}
		}
		return false, lat, fmt.Sprintf("router %s unreachable (%s)", p.Endpoint, detail), nil
	}

	if p.Adapter == "agy" {
		start := time.Now()
		info := DetectAgyInfo(ctx)
		lat := time.Since(start).Milliseconds()
		if info.Installed {
			active := p.DefaultModel
			if active == "" {
				active = info.ActiveModel
			}
			quotaDetail := ""
			if buckets, qErr := quota.PollCommand(ctx, []string{"agy", "-p", "/usage"}, "agy-usage"); qErr == nil && len(buckets) > 0 {
				quotaDetail = fmt.Sprintf("; %d quota windows polled", len(buckets))
			}
			detail := fmt.Sprintf("polled %d models via 'agy models' (active: %s%s)", len(info.Models), active, quotaDetail)
			return true, lat, detail, info.Models
		}
	}

	binName := p.Adapter
	switch p.Adapter {
	case "claude-cli":
		binName = "claude"
	case "gemini-cli":
		binName = "gemini"
	}
	binPath := p.BinaryPath
	if binPath == "" {
		var err error
		binPath, err = LookCLIPath(binName)
		if err != nil {
			return false, 0, fmt.Sprintf("%s binary not found", binName), nil
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(probeCtx, binPath, "--version")
	out, err := cmd.CombinedOutput()
	lat := time.Since(start).Milliseconds()
	if err != nil {
		// Some CLIs only support -h / --help
		if info, statErr := os.Stat(binPath); statErr == nil && !info.IsDir() {
			return true, lat, fmt.Sprintf("binary verified (%s)", binPath), nil
		}
		return false, lat, fmt.Sprintf("exec failed: %v", err), nil
	}
	verLine := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	if len(verLine) > 48 {
		verLine = verLine[:48] + "..."
	}
	if verLine == "" {
		verLine = "ok"
	}
	return true, lat, fmt.Sprintf("%s (%dms)", verLine, lat), nil
}

// DiscoverProviders scans the machine for all compliant AI routers (via config files,
// environment variables, and fast concurrent localhost port probes) and installed coding CLIs.
func DiscoverProviders(ctx context.Context, root string) []DiscoveredProvider {
	var providers []DiscoveredProvider
	seenEndpoints := make(map[string]bool)
	seenIDs := make(map[string]bool)

	addProvider := func(p DiscoveredProvider) {
		if p.ID == "" {
			return
		}
		origID := p.ID
		idx := 2
		for seenIDs[p.ID] {
			p.ID = fmt.Sprintf("%s_%d", origID, idx)
			idx++
		}
		seenIDs[p.ID] = true
		if p.Endpoint != "" {
			seenEndpoints[normalizeBaseURL(p.Endpoint)] = true
		}
		if p.UsageCapPct <= 0 {
			p.UsageCapPct = 100
		}
		if p.Attempts <= 0 {
			p.Attempts = 2
		}
		providers = append(providers, p)
	}

	// 1. Inspect OpenCode config (opencode.json) if present
	ocInfo := DetectOpenCodeInfo(root)
	if ocInfo.Installed || ocInfo.ConfigPath != "" {
		ep := normalizeBaseURL(ocInfo.BaseURL)
		if ep == "" {
			ep = "http://127.0.0.1:8080/v1"
		}
		ok, lat, liveModels, detail := ProbeOpenAIRouter(ctx, ep, 300*time.Millisecond)
		models := ocInfo.Models
		for _, lm := range liveModels {
			found := false
			for _, em := range models {
				if em == lm {
					found = true
					break
				}
			}
			if !found {
				models = append(models, lm)
			}
		}
		defModel := ocInfo.DefaultModel
		if defModel == "" && len(models) > 0 {
			defModel = models[0]
		}
		status := "ready"
		if ok {
			status = "online"
		}
		src := "opencode"
		if ocInfo.ConfigPath != "" {
			src = "opencode.json"
		}
		name := "OpenCode Local Router"
		if ocInfo.ProviderName != "" {
			name = fmt.Sprintf("OpenCode (%s)", ocInfo.ProviderName)
		}
		addProvider(DiscoveredProvider{
			ID:           "local",
			Name:         name,
			Kind:         "router",
			Adapter:      "opencode",
			BinaryPath:   ocInfo.BinaryPath,
			Endpoint:     ep,
			DefaultModel: defModel,
			Models:       models,
			Pool:         "local",
			PoolMode:     "until-rejected",
			FloorPct:     0,
			UsageCapPct:  100,
			Attempts:     3,
			Enabled:      true,
			Source:       src,
			Status:       status,
			LatencyMs:    lat,
			ProbeDetail:  detail,
		})
	}

	// 2. Check environment variables for compliant AI routers
	envRouters := []struct {
		envKey string
		id     string
		name   string
	}{
		{"OPENAI_BASE_URL", "openai_router", "OpenAI-Compatible Router (ENV)"},
		{"LITELLM_PROXY_URL", "litellm", "LiteLLM Proxy Router (ENV)"},
		{"OLLAMA_HOST", "ollama", "Ollama Host Router (ENV)"},
	}
	for _, er := range envRouters {
		if val := strings.TrimSpace(os.Getenv(er.envKey)); val != "" {
			ep := normalizeBaseURL(val)
			if ep != "" && !seenEndpoints[ep] {
				ok, lat, models, detail := ProbeOpenAIRouter(ctx, ep, 300*time.Millisecond)
				defModel := ""
				if len(models) > 0 {
					defModel = models[0]
				}
				status := "ready"
				if ok {
					status = "online"
				}
				addProvider(DiscoveredProvider{
					ID:           er.id,
					Name:         er.name,
					Kind:         "router",
					Adapter:      "opencode",
					Endpoint:     ep,
					DefaultModel: defModel,
					Models:       models,
					Pool:         "local",
					PoolMode:     "until-rejected",
					FloorPct:     0,
					UsageCapPct:  100,
					Attempts:     2,
					Enabled:      true,
					Source:       "env:" + er.envKey,
					Status:       status,
					LatencyMs:    lat,
					ProbeDetail:  detail,
				})
			}
		}
	}

	// 2b. Raon hosted test provider (OpenAI-compatible), enabled by RAON_API_KEY
	if key := ResolveRaonKey(); key != "" {
		ep := normalizeBaseURL(RaonBaseURL)
		if !seenEndpoints[ep] {
			ok, lat, models, detail := ProbeOpenAIRouterWithKey(ctx, ep, key, 3*time.Second)
			if len(models) == 0 {
				models = []string{RaonDefaultModel}
			}
			// opencode addresses models as <provider>/<model>
			for i, m := range models {
				if !strings.HasPrefix(m, "raon/") {
					models[i] = "raon/" + m
				}
			}
			status := "ready"
			if ok {
				status = "online"
			}
			addProvider(DiscoveredProvider{
				ID:           "raon",
				Name:         "Raon (test)",
				Kind:         "router",
				Adapter:      "opencode",
				Endpoint:     ep,
				DefaultModel: models[0],
				Models:       models,
				Pool:         "raon",
				PoolMode:     "until-rejected",
				UsageCapPct:  100,
				Attempts:     2,
				Enabled:      true,
				Source:       "raon key (env or opencode.json)",
				Status:       status,
				LatencyMs:    lat,
				ProbeDetail:  detail,
			})
		}
	}

	// 3. Concurrent fast probe of standard local OpenAI-compatible router ports
	type probeHit struct {
		target routerProbeTarget
		lat    int64
		models []string
		detail string
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		hits []probeHit
	)
	for _, target := range StandardRouterTargets {
		norm := normalizeBaseURL(target.BaseURL)
		if seenEndpoints[norm] {
			continue
		}
		wg.Add(1)
		go func(t routerProbeTarget) {
			defer wg.Done()
			ok, lat, models, detail := ProbeOpenAIRouter(ctx, t.BaseURL, 250*time.Millisecond)
			if ok {
				mu.Lock()
				hits = append(hits, probeHit{target: t, lat: lat, models: models, detail: detail})
				mu.Unlock()
			}
		}(target)
	}
	wg.Wait()

	for _, h := range hits {
		norm := normalizeBaseURL(h.target.BaseURL)
		if seenEndpoints[norm] {
			continue
		}
		defModel := ""
		if len(h.models) > 0 {
			defModel = h.models[0]
		}
		id := h.target.ID
		if !seenIDs["local"] {
			id = "local"
		}
		addProvider(DiscoveredProvider{
			ID:           id,
			Name:         h.target.Name,
			Kind:         "router",
			Adapter:      "opencode",
			Endpoint:     norm,
			DefaultModel: defModel,
			Models:       h.models,
			Pool:         "local",
			PoolMode:     "until-rejected",
			FloorPct:     0,
			UsageCapPct:  100,
			Attempts:     3,
			Enabled:      true,
			Source:       h.target.Source,
			Status:       "online",
			LatencyMs:    h.lat,
			ProbeDetail:  h.detail,
		})
	}

	// 4. Discover installed Agent CLIs on the machine
	agyInfo := DetectAgyInfo(ctx)
	if agyInfo.Installed {
		activeLower := strings.ToLower(agyInfo.ActiveModel)
		isSonnetActive := strings.Contains(activeLower, "sonnet") || strings.Contains(activeLower, "claude")

		flashModel := "gemini-3.8-flash-medium"
		proModel := "gemini-4-argon-medium"
		sonnetModel := "claude-sonnet-5-5-medium"
		opusModel := "claude-opus-5-5-medium"

		for _, m := range agyInfo.Models {
			mLow := strings.ToLower(m)
			if strings.Contains(mLow, "flash") && flashModel == "gemini-3.8-flash-medium" {
				flashModel = m
			}
			if (strings.Contains(mLow, "argon") || strings.Contains(mLow, "pro")) && proModel == "gemini-4-argon-medium" {
				proModel = m
			}
			if strings.Contains(mLow, "sonnet") && sonnetModel == "claude-sonnet-5-5-medium" {
				sonnetModel = m
			}
			if strings.Contains(mLow, "opus") && opusModel == "claude-opus-5-5-medium" {
				opusModel = m
			}
		}

		if isSonnetActive {
			sonnetModel = agyInfo.ActiveModel
		} else if agyInfo.ActiveModel != "" {
			flashModel = agyInfo.ActiveModel
		}

		// Gemini Flash via agy
		addProvider(DiscoveredProvider{
			ID:           "flash",
			Name:         "Antigravity CLI (Gemini Flash)",
			Kind:         "cli",
			Adapter:      "agy",
			BinaryPath:   agyInfo.BinaryPath,
			DefaultModel: flashModel,
			Models:       agyInfo.Models,
			Pool:         "gemini",
			PoolMode:     "polled",
			FloorPct:     20,
			UsageCapPct:  100,
			Attempts:     2,
			Enabled:      !isSonnetActive,
			Source:       "agy models (" + flashModel + ")",
			Status:       "ready",
			ProbeDetail:  fmt.Sprintf("polled %d models (active: %s)", len(agyInfo.Models), flashModel),
		})

		// Claude Sonnet via agy
		addProvider(DiscoveredProvider{
			ID:           "sonnet",
			Name:         "Antigravity CLI (Claude Sonnet)",
			Kind:         "cli",
			Adapter:      "agy",
			BinaryPath:   agyInfo.BinaryPath,
			DefaultModel: sonnetModel,
			Models:       agyInfo.Models,
			Pool:         "claude",
			PoolMode:     "polled",
			FloorPct:     25,
			UsageCapPct:  100,
			Attempts:     2,
			Enabled:      isSonnetActive,
			Source:       "agy models (" + sonnetModel + ")",
			Status:       "ready",
			ProbeDetail:  fmt.Sprintf("polled %d models (active: %s)", len(agyInfo.Models), sonnetModel),
		})

		// Gemini Argon / Pro escalation via agy
		addProvider(DiscoveredProvider{
			ID:           "pro",
			Name:         "Antigravity CLI (Gemini Argon / Pro)",
			Kind:         "cli",
			Adapter:      "agy",
			BinaryPath:   agyInfo.BinaryPath,
			DefaultModel: proModel,
			Models:       agyInfo.Models,
			Pool:         "gemini",
			PoolMode:     "polled",
			FloorPct:     20,
			UsageCapPct:  100,
			Attempts:     1,
			Enabled:      !isSonnetActive,
			Source:       "agy models (" + proModel + ")",
			Status:       "ready",
			ProbeDetail:  fmt.Sprintf("polled %d models (escalation: %s)", len(agyInfo.Models), proModel),
		})

		// Claude Opus escalation via agy
		addProvider(DiscoveredProvider{
			ID:           "opus",
			Name:         "Antigravity CLI (Opus Escalation)",
			Kind:         "cli",
			Adapter:      "agy",
			BinaryPath:   agyInfo.BinaryPath,
			DefaultModel: opusModel,
			Models:       agyInfo.Models,
			Pool:         "claude",
			PoolMode:     "polled",
			FloorPct:     25,
			UsageCapPct:  100,
			Attempts:     1,
			Enabled:      isSonnetActive,
			Source:       "agy models (" + opusModel + ")",
			Status:       "ready",
			ProbeDetail:  fmt.Sprintf("polled %d models (escalation: %s)", len(agyInfo.Models), opusModel),
		})
	}

	type otherCLISpec struct {
		bin          string
		id           string
		name         string
		adapter      string
		defaultModel string
		models       []string
		pool         string
		poolMode     string
		floorPct     int
		attempts     int
	}
	otherCLIs := []otherCLISpec{
		{
			bin: "claude", id: "cc", name: "Claude Code CLI", adapter: "claude-cli",
			defaultModel: "sonnet", models: []string{"sonnet", "opus", "haiku"},
			pool: "cc", poolMode: "polled", floorPct: 25, attempts: 2,
		},
		{
			bin: "codex", id: "codex", name: "OpenAI Codex CLI", adapter: "codex",
			defaultModel: "gpt-5.6-luna", models: []string{"gpt-5.6-luna", "gpt-5.6-terra", "o3"},
			pool: "codex", poolMode: "until-rejected", floorPct: 20, attempts: 2,
		},
		{
			bin: "aider", id: "aider", name: "Aider Coding CLI", adapter: "aider",
			defaultModel: "sonnet", models: []string{"sonnet", "deepseek", "gpt-4o"},
			pool: "aider", poolMode: "until-rejected", floorPct: 20, attempts: 2,
		},
		{
			bin: "gemini", id: "gemini", name: "Gemini Agent CLI", adapter: "gemini-cli",
			defaultModel: "gemini-2.5-pro", models: []string{"gemini-2.5-pro", "gemini-2.5-flash"},
			pool: "gemini", poolMode: "until-rejected", floorPct: 20, attempts: 2,
		},
		{
			bin: "hermes", id: "hermes", name: "Hermes Agent CLI", adapter: "hermes",
			defaultModel: "", models: nil,
			pool: "hermes", poolMode: "until-rejected", floorPct: 20, attempts: 2,
		},
		{
			bin: "pi", id: "pi", name: "Pi Coding Agent", adapter: "pi",
			defaultModel: "", models: nil,
			pool: "pi", poolMode: "until-rejected", floorPct: 20, attempts: 2,
		},
	}

	for _, spec := range otherCLIs {
		if binPath, err := LookCLIPath(spec.bin); err == nil {
			addProvider(DiscoveredProvider{
				ID:           spec.id,
				Name:         spec.name,
				Kind:         "cli",
				Adapter:      spec.adapter,
				BinaryPath:   binPath,
				DefaultModel: spec.defaultModel,
				Models:       spec.models,
				Pool:         spec.pool,
				PoolMode:     spec.poolMode,
				FloorPct:     spec.floorPct,
				UsageCapPct:  100,
				Attempts:     spec.attempts,
				Enabled:      true,
				Source:       binPath,
				Status:       "ready",
			})
		}
	}

	// Assign sequential priority numbers
	for i := range providers {
		providers[i].Priority = i + 1
	}

	return providers
}
