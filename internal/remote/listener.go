package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/eventlog"
	"github.com/erinwnash/Shepherd/internal/notify"
)

// MaxMessageBytes caps a command message; longer messages are ignored unread.
const MaxMessageBytes = 256

// OutboundTag marks Shepherd's own ntfy notifications so the listener never obeys its own echo.
const OutboundTag = notify.OutboundTag

// idleTimeout: ntfy sends keepalives every ~45s; silence longer than this means a dead stream.
const idleTimeout = 2 * time.Minute

// Backend is the slice of the controller the ntfy channel may drive.
type Backend interface {
	Start(ctx context.Context) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Stop(ctx context.Context) error
	Halt(ctx context.Context, reason string) error
	Lockdown(ctx context.Context) error
	Drain(ctx context.Context, enabled bool, until *time.Time) error
	QueueHold(ctx context.Context, chunkID string) error
	QueueRelease(ctx context.Context, chunkID string) error
	QueueSkip(ctx context.Context, chunkID string) error
	QueueRetry(ctx context.Context, chunkID string) error
	Status(ctx context.Context) (control.DaemonStatus, error)
	NotifyEvent(ctx context.Context, eventType, title, message string)
}

// Auditor records remote commands in the hash-chained event log.
type Auditor interface {
	Append(eventType, actor, payload string) (eventlog.Event, error)
}

type ntfyMessage struct {
	ID      string   `json:"id"`
	Time    int64    `json:"time"`
	Event   string   `json:"event"`
	Message string   `json:"message"`
	Tags    []string `json:"tags"`
}

// Listener subscribes to an ntfy topic and turns keyword messages into control actions.
type Listener struct {
	cfg     *Commands
	url     string
	backend Backend
	audit   Auditor
	client  *http.Client
	Logf    func(format string, args ...any)

	start      time.Time
	now        func() time.Time
	backoffMin time.Duration
	backoffMax time.Duration

	mu        sync.Mutex
	subCancel context.CancelFunc // cancels the live subscription (topic switch)
	switched  bool               // set by SetURL so Run reconnects without backoff
	lastID    string
	seen     map[string]struct{}
	seenRing []string
	window   []time.Time
}

// NewListener validates the topic URL and builds a listener. Call Run to start it.
func NewListener(cfg *Commands, topicURL string, backend Backend, audit Auditor) (*Listener, error) {
	if cfg.URL != "" {
		topicURL = cfg.URL
	}
	topicURL = strings.TrimRight(topicURL, "/")
	if topicURL == "" {
		return nil, errors.New("no ntfy topic configured: set [notify] ntfy_topic in shepherd.toml or url in " + FileName)
	}
	if err := ValidateURL(topicURL); err != nil {
		return nil, err
	}
	return &Listener{
		cfg:        cfg,
		url:        topicURL,
		backend:    backend,
		audit:      audit,
		client:     &http.Client{}, // streaming: no overall timeout; idle watchdog instead
		Logf:       func(string, ...any) {},
		start:      time.Now(),
		now:        time.Now,
		backoffMin: time.Second,
		backoffMax: time.Minute,
		seen:       map[string]struct{}{},
	}, nil
}

// SetURL moves the listener to a new topic (live rotation): the current subscription is dropped
// and Run resubscribes immediately. Messages sent to the new topic before the switch are ignored.
func (l *Listener) SetURL(topicURL string) error {
	topicURL = strings.TrimRight(topicURL, "/")
	if err := ValidateURL(topicURL); err != nil {
		return err
	}
	l.mu.Lock()
	l.url = topicURL
	l.lastID = ""
	l.start = l.now()
	l.switched = true
	cancel := l.subCancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	l.Logf("ntfy: now watching %s for commands", redactTopic(topicURL))
	l.auditf("remote.listening", "topic=%s keywords=%s (topic rotated)", redactTopic(topicURL), strings.Join(l.cfg.Keywords(), ", "))
	return nil
}

func (l *Listener) takeSwitched() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	sw := l.switched
	l.switched = false
	return sw
}

// Run subscribes until ctx is cancelled, reconnecting with backoff and resuming from the last
// message id so a keyword sent during a reconnect is not lost.
func (l *Listener) Run(ctx context.Context) {
	kws := strings.Join(l.cfg.Keywords(), ", ")
	l.mu.Lock()
	topic := redactTopic(l.url)
	l.mu.Unlock()
	l.Logf("ntfy: watching %s for commands: %s", topic, kws)
	l.auditf("remote.listening", "topic=%s keywords=%s", topic, kws)

	backoff := l.backoffMin
	for ctx.Err() == nil {
		gotData, err := l.subscribe(ctx)
		if ctx.Err() != nil {
			return
		}
		if l.takeSwitched() {
			backoff = l.backoffMin
			continue
		}
		if gotData {
			backoff = l.backoffMin
		}
		if err != nil {
			l.Logf("ntfy: subscription error (retrying in %s): %v", backoff, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > l.backoffMax {
			backoff = l.backoffMax
		}
	}
}

func (l *Listener) subscribe(ctx context.Context) (bool, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	q := url.Values{}
	l.mu.Lock()
	topicURL := l.url
	l.subCancel = cancel
	if l.lastID != "" {
		q.Set("since", l.lastID)
	} else {
		q.Set("since", fmt.Sprint(l.start.Unix()))
	}
	l.mu.Unlock()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, topicURL+"/json?"+q.Encode(), nil)
	if err != nil {
		return false, err
	}
	if tok, err := l.cfg.Token(); err == nil {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("ntfy HTTP %d (check topic and %s)", resp.StatusCode, tokenHint(l.cfg))
	}

	watchdog := time.AfterFunc(idleTimeout, cancel)
	defer watchdog.Stop()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 4096), 64*1024) // ntfy caps messages at 4 KiB; anything bigger is hostile
	gotData := false
	for sc.Scan() {
		watchdog.Reset(idleTimeout)
		gotData = true
		var m ntfyMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if m.Event != "message" {
			continue
		}
		l.handle(ctx, m)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil && reqCtx.Err() == nil {
		return gotData, err
	}
	return gotData, nil
}

// handle applies one ntfy message. Exposed to tests via the package.
func (l *Listener) handle(ctx context.Context, m ntfyMessage) {
	if !l.markSeen(m.ID) {
		return
	}
	for _, t := range m.Tags {
		if t == OutboundTag {
			return
		}
	}
	l.mu.Lock()
	start := l.start
	l.mu.Unlock()
	if m.Time != 0 && m.Time < start.Unix() {
		return // replay of a message sent before this daemon started
	}
	if len(m.Message) > MaxMessageBytes {
		return
	}
	fields := strings.Fields(m.Message)
	if len(fields) == 0 {
		return
	}
	kw := strings.ToLower(fields[0])
	cmd, ok := l.cfg.lookup(kw)
	if !ok {
		return
	}
	spec := actions[cmd.Action]
	args := fields[1:]

	reject := func(why string) {
		l.auditf("remote.command", "keyword=%s action=%s result=rejected reason=%q", kw, cmd.Action, why)
		l.Logf("ntfy: rejected %q: %s", kw, why)
		l.backend.NotifyEvent(ctx, notify.EventRemoteCommand, "Remote Command Rejected", fmt.Sprintf("%s → %s: %s", kw, cmd.Action, why))
	}

	if !spec.emergency && !l.allowRate() {
		l.Logf("ntfy: rate limit exceeded, dropped %q", kw)
		return // no audit/ack: a flood must not flood the log or the topic
	}
	if spec.escalating && !l.cfg.AllowEscalating {
		reject("escalating actions are disabled (allow_escalating = false)")
		return
	}
	chunkID := ""
	if spec.needsChunk {
		if len(args) != 1 || !chunkIDRe.MatchString(args[0]) {
			reject("expected exactly one chunk id, e.g. `" + kw + " S01-c02`")
			return
		}
		chunkID = args[0]
	}

	reply, err := l.dispatch(ctx, cmd.Action, kw, chunkID)
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	l.auditf("remote.command", "keyword=%s action=%s chunk=%s result=%s", kw, cmd.Action, chunkID, result)
	l.Logf("ntfy: %s → %s %s: %s", kw, cmd.Action, chunkID, result)
	if reply == "" {
		reply = result
	}
	l.backend.NotifyEvent(ctx, notify.EventRemoteCommand, "Remote Command: "+kw, fmt.Sprintf("%s %s → %s", cmd.Action, chunkID, reply))
}

func (l *Listener) dispatch(ctx context.Context, action, kw, chunkID string) (string, error) {
	reason := "ntfy keyword " + kw
	switch action {
	case ActionLockdown:
		return "lockdown engaged; run `shepherd unlock` locally to resume", l.backend.Lockdown(ctx)
	case ActionHalt:
		return "", l.backend.Halt(ctx, reason)
	case ActionPause:
		return "", l.backend.Pause(ctx)
	case ActionStop:
		return "", l.backend.Stop(ctx)
	case ActionStart:
		return "", l.backend.Start(ctx)
	case ActionResume:
		return "", l.backend.Resume(ctx)
	case ActionDrainOn:
		return "", l.backend.Drain(ctx, true, nil)
	case ActionDrainOff:
		return "", l.backend.Drain(ctx, false, nil)
	case ActionHold:
		return "", l.backend.QueueHold(ctx, chunkID)
	case ActionRelease:
		return "", l.backend.QueueRelease(ctx, chunkID)
	case ActionSkip:
		return "", l.backend.QueueSkip(ctx, chunkID)
	case ActionRetry:
		return "", l.backend.QueueRetry(ctx, chunkID)
	case ActionStatus:
		st, err := l.backend.Status(ctx)
		if err != nil {
			return "", err
		}
		s := fmt.Sprintf("state=%s pending=%d running=%d blocked=%d done=%d", st.State, st.QueuePending, st.QueueRunning, st.QueueBlocked, st.QueueCompleted)
		if st.RunningChunkID != "" {
			s += " chunk=" + st.RunningChunkID
		}
		if st.Lockdown {
			s += " LOCKDOWN"
		}
		return s, nil
	}
	return "", fmt.Errorf("unknown action %q", action)
}

// markSeen records a message id, returning false if it was already handled.
func (l *Listener) markSeen(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id == "" {
		return true
	}
	l.lastID = id
	if _, dup := l.seen[id]; dup {
		return false
	}
	l.seen[id] = struct{}{}
	l.seenRing = append(l.seenRing, id)
	if len(l.seenRing) > 512 {
		delete(l.seen, l.seenRing[0])
		l.seenRing = l.seenRing[1:]
	}
	return true
}

func (l *Listener) allowRate() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-time.Minute)
	keep := l.window[:0]
	for _, t := range l.window {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	l.window = keep
	if len(l.window) >= l.cfg.RateLimitPerMin {
		return false
	}
	l.window = append(l.window, now)
	return true
}

func (l *Listener) auditf(typ, format string, args ...any) {
	if l.audit != nil {
		_, _ = l.audit.Append(typ, "ntfy", fmt.Sprintf(format, args...))
	}
}

// redactTopic hides most of the topic name: on public ntfy it is the shared secret.
func redactTopic(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if n := len(parts); n > 0 && len(parts[n-1]) > 4 {
		parts[n-1] = parts[n-1][:4] + "…"
	}
	return u.Scheme + "://" + u.Host + "/" + strings.Join(parts, "/")
}

func tokenHint(c *Commands) string {
	if c.TokenEnv == "" {
		return "token_env if the topic is protected"
	}
	return "the token in $" + c.TokenEnv
}
