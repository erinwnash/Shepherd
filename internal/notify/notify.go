// Package notify provides operator notification dispatching to ntfy, Slack, Discord, custom shell commands, and desktop notifications.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Standard notification event types.
const (
	EventDaemonStarted  = "daemon_started"
	EventDaemonStopped  = "daemon_stopped"
	EventDaemonPaused   = "daemon_paused"
	EventDaemonResumed  = "daemon_resumed"
	EventDaemonHalted   = "daemon_halted"
	EventDaemonLockdown = "daemon_lockdown"
	EventDrainMode      = "drain_mode"

	EventChunkStarted  = "chunk_started"
	EventModelSwitch   = "model_switch"
	EventChunkPassed   = "chunk_passed"
	EventChunkHeld     = "chunk_held"
	EventQueueBlocked  = "queue_blocked"
	EventQueueStalled  = "queue_stalled"
	EventChunkStalled  = "chunk_stalled"
	EventSprintDone    = "sprint_done"
	EventQuotaCritical = "quota_critical"
	EventPlanError     = "plan_error"
	EventRemoteCommand = "remote_command"
)

// OutboundTag is added to every ntfy notification Shepherd publishes, so the ntfy command
// listener (internal/remote) can ignore its own echo on a shared topic.
const OutboundTag = "shepherd-out"

// Config holds notification channel settings.
type Config struct {
	Events            []string `toml:"events,omitempty"`
	SlackWebhookURL   string   `toml:"slack_webhook_url,omitempty"`
	DiscordWebhookURL string   `toml:"discord_webhook_url,omitempty"`
	Desktop           bool     `toml:"desktop,omitempty"`
	NtfyURL           string   `toml:"ntfy_url,omitempty"`
	NtfyTopic         string   `toml:"ntfy_topic,omitempty"`
	NtfyTokenEnv      string   `toml:"ntfy_token_env,omitempty"`
	NotifyCmd         string   `toml:"notify_cmd,omitempty"`
}

// ntfyGap is the minimum spacing between ntfy posts. ntfy stamps messages with whole-second
// times and clients sort by them, so two posts in the same second (chunk passed, next chunk
// started) can display in either order. A 1s gap after the previous post completed guarantees
// the server stamps the next one with a later second.
const ntfyGap = time.Second

// Dispatcher manages sending notifications across configured channels.
type Dispatcher struct {
	cfg         Config
	isNtfyCmd   bool
	client      *http.Client
	sendDesktop func(title, message string) error

	ntfyMu   sync.Mutex // serializes ntfy posts so they arrive in send order
	ntfyLast time.Time  // when the previous ntfy post completed

	urlMu sync.RWMutex // guards cfg.NtfyURL, which can be rotated live
}

// NewDispatcher creates a new notification dispatcher.
func NewDispatcher(cfg Config) *Dispatcher {
	// Auto-detect environment variables if not configured
	if cfg.NtfyURL == "" && cfg.NtfyTopic == "" {
		if u := os.Getenv("NTFY_URL"); u != "" {
			cfg.NtfyURL = u
		} else if t := os.Getenv("NTFY_TOPIC"); t != "" {
			cfg.NtfyTopic = t
		}
	}
	if cfg.NotifyCmd == "" {
		if cmd := os.Getenv("NOTIFY_CMD"); cmd != "" {
			cfg.NotifyCmd = cmd
		}
	}

	// If NtfyTopic is set, build full URL
	if cfg.NtfyURL == "" && cfg.NtfyTopic != "" {
		topic := strings.TrimPrefix(cfg.NtfyTopic, "https://ntfy.sh/")
		topic = strings.TrimPrefix(topic, "http://ntfy.sh/")
		topic = strings.TrimPrefix(topic, "ntfy.sh/")
		cfg.NtfyURL = "https://ntfy.sh/" + strings.Trim(topic, "/")
	}

	// If NtfyURL is still empty but NotifyCmd contains ntfy.sh, extract URL for rich HTTP dispatch
	isNtfyCmd := false
	if cfg.NtfyURL == "" && cfg.NotifyCmd != "" && strings.Contains(cfg.NotifyCmd, "ntfy.sh") {
		words := strings.Fields(cfg.NotifyCmd)
		for _, w := range words {
			if strings.Contains(w, "ntfy.sh") {
				target := strings.Trim(w, "'\"")
				if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
					target = "https://" + target
				}
				cfg.NtfyURL = target
				isNtfyCmd = true
				break
			}
		}
	}

	return &Dispatcher{
		cfg:       cfg,
		isNtfyCmd: isNtfyCmd,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		sendDesktop: defaultDesktopNotify,
	}
}

// NtfyURL returns the resolved ntfy URL (if any).
func (d *Dispatcher) NtfyURL() string {
	d.urlMu.RLock()
	defer d.urlMu.RUnlock()
	return d.cfg.NtfyURL
}

// SetNtfyURL switches the ntfy topic live (topic rotation).
func (d *Dispatcher) SetNtfyURL(u string) {
	d.urlMu.Lock()
	d.cfg.NtfyURL = u
	d.urlMu.Unlock()
}

// PostNtfy posts one message to a specific ntfy topic, bypassing the event filter. Used for
// rotation notices that must reach both the retired and the new topic.
func (d *Dispatcher) PostNtfy(ctx context.Context, topicURL, title, message string, tags []string, priority string) error {
	return d.postNtfy(ctx, topicURL, title, message, priority, tags)
}

func defaultTagsAndPriority(eventType string) ([]string, string) {
	switch eventType {
	case EventDaemonStarted:
		return []string{"rocket", "shepherd"}, "default"
	case EventDaemonStopped:
		return []string{"octagonal_sign"}, "default"
	case EventDaemonPaused:
		return []string{"pause_button"}, "default"
	case EventDaemonResumed:
		return []string{"play_pause"}, "default"
	case EventDaemonHalted:
		return []string{"warning", "rotating_light"}, "high"
	case EventDaemonLockdown:
		return []string{"lock", "rotating_light"}, "urgent"
	case EventDrainMode:
		return []string{"droplet"}, "low"
	case EventChunkStarted:
		return []string{"arrow_forward", "robot"}, "default"
	case EventModelSwitch:
		return []string{"twisted_rightwards_arrows", "arrows_counterclockwise"}, "high"
	case EventChunkPassed:
		return []string{"white_check_mark", "tada"}, "default"
	case EventChunkHeld:
		return []string{"hand", "pause_button"}, "high"
	case EventQueueBlocked:
		return []string{"x", "rotating_light"}, "high"
	case EventQueueStalled:
		return []string{"hourglass", "warning"}, "high"
	case EventChunkStalled:
		return []string{"hourglass", "eyes"}, "high"
	case EventSprintDone:
		return []string{"trophy", "tada"}, "high"
	case EventQuotaCritical:
		return []string{"chart_with_downwards_trend", "warning"}, "high"
	case EventPlanError:
		return []string{"warning", "page_facing_up"}, "high"
	case EventRemoteCommand:
		return []string{"satellite_antenna"}, "high"
	default:
		return []string{"bell"}, "default"
	}
}

func defaultDesktopNotify(title, message string) error {
	switch runtime.GOOS {
	case "linux":
		if path, err := exec.LookPath("notify-send"); err == nil {
			return exec.Command(path, title, message).Run()
		}
	case "darwin":
		if path, err := exec.LookPath("osascript"); err == nil {
			script := fmt.Sprintf(`display notification %q with title %q`, message, title)
			return exec.Command(path, "-e", script).Run()
		}
	}
	return nil
}

// ShouldSend checks whether a given event type is enabled in the configuration.
func (d *Dispatcher) ShouldSend(eventType string) bool {
	if len(d.cfg.Events) == 0 {
		return true // default: all events enabled if channels configured
	}
	for _, e := range d.cfg.Events {
		if strings.EqualFold(e, eventType) || e == "*" || e == "all" {
			return true
		}
	}
	return false
}

// Send broadcasts a notification to all configured channels if the event type is enabled.
func (d *Dispatcher) Send(ctx context.Context, eventType, title, message string) error {
	tags, priority := defaultTagsAndPriority(eventType)
	return d.SendWithDetails(ctx, eventType, title, message, tags, priority)
}

// SendWithDetails broadcasts a notification with custom tags and priority.
func (d *Dispatcher) SendWithDetails(ctx context.Context, eventType, title, message string, tags []string, priority string) error {
	if !d.ShouldSend(eventType) {
		return nil
	}

	fullMsg := fmt.Sprintf("[%s] %s: %s", strings.ToUpper(eventType), title, message)

	var (
		errs []string
		mu   sync.Mutex
		wg   sync.WaitGroup
	)

	addErr := func(e string) {
		mu.Lock()
		errs = append(errs, e)
		mu.Unlock()
	}

	// 1. Ntfy (HTTP API with rich headers)
	if ntfyURL := d.NtfyURL(); ntfyURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.postNtfy(ctx, ntfyURL, title, message, priority, tags); err != nil {
				addErr(fmt.Sprintf("ntfy: %v", err))
			}
		}()
	}

	// 2. Custom Notify Command (if not already handled by postNtfy)
	if d.cfg.NotifyCmd != "" && !d.isNtfyCmd {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.CommandContext(ctx, "sh", "-c", d.cfg.NotifyCmd)
			cmd.Stdin = strings.NewReader(fullMsg)
			if err := cmd.Run(); err != nil {
				addErr(fmt.Sprintf("notify_cmd: %v", err))
			}
		}()
	}

	// 3. Slack Webhook
	if d.cfg.SlackWebhookURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := map[string]string{"text": fullMsg}
			if err := d.postJSON(ctx, d.cfg.SlackWebhookURL, payload); err != nil {
				addErr(fmt.Sprintf("slack: %v", err))
			}
		}()
	}

	// 4. Discord Webhook
	if d.cfg.DiscordWebhookURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := map[string]string{"content": fullMsg}
			if err := d.postJSON(ctx, d.cfg.DiscordWebhookURL, payload); err != nil {
				addErr(fmt.Sprintf("discord: %v", err))
			}
		}()
	}

	// 5. Desktop Notification
	if d.cfg.Desktop && d.sendDesktop != nil {
		if err := d.sendDesktop(title, message); err != nil {
			addErr(fmt.Sprintf("desktop: %v", err))
		}
	}

	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("notification failures: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (d *Dispatcher) postNtfy(ctx context.Context, ntfyURL, title, message, priority string, tags []string) error {
	d.ntfyMu.Lock()
	defer d.ntfyMu.Unlock()
	if wait := ntfyGap - time.Since(d.ntfyLast); wait > 0 && !d.ntfyLast.IsZero() {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() { d.ntfyLast = time.Now() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ntfyURL, strings.NewReader(message))
	if err != nil {
		return err
	}
	if title != "" {
		req.Header.Set("Title", title)
	}
	if priority != "" {
		req.Header.Set("Priority", priority)
	}
	req.Header.Set("Tags", strings.Join(append(append([]string(nil), tags...), OutboundTag), ","))
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if d.cfg.NtfyTokenEnv != "" {
		if tok := strings.TrimSpace(os.Getenv(d.cfg.NtfyTokenEnv)); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy HTTP %d", resp.StatusCode)
	}
	return nil
}

func (d *Dispatcher) postJSON(ctx context.Context, url string, data any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
