package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/eventlog"
)

type fakeBackend struct {
	mu    sync.Mutex
	calls []string
	notes []string
}

func (f *fakeBackend) rec(s string) error {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Start(context.Context) error            { return f.rec("start") }
func (f *fakeBackend) Pause(context.Context) error            { return f.rec("pause") }
func (f *fakeBackend) Resume(context.Context) error           { return f.rec("resume") }
func (f *fakeBackend) Stop(context.Context) error             { return f.rec("stop") }
func (f *fakeBackend) Lockdown(context.Context) error         { return f.rec("lockdown") }
func (f *fakeBackend) Halt(_ context.Context, r string) error { return f.rec("halt:" + r) }
func (f *fakeBackend) Drain(_ context.Context, on bool, _ *time.Time) error {
	return f.rec(fmt.Sprintf("drain:%v", on))
}
func (f *fakeBackend) QueueHold(_ context.Context, id string) error    { return f.rec("hold:" + id) }
func (f *fakeBackend) QueueRelease(_ context.Context, id string) error { return f.rec("release:" + id) }
func (f *fakeBackend) QueueSkip(_ context.Context, id string) error    { return f.rec("skip:" + id) }
func (f *fakeBackend) QueueRetry(_ context.Context, id string) error   { return f.rec("retry:" + id) }
func (f *fakeBackend) Status(context.Context) (control.DaemonStatus, error) {
	_ = f.rec("status")
	return control.DaemonStatus{State: "running", QueuePending: 3}, nil
}
func (f *fakeBackend) NotifyEvent(_ context.Context, _, title, msg string) {
	f.mu.Lock()
	f.notes = append(f.notes, title+": "+msg)
	f.mu.Unlock()
}
func (f *fakeBackend) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type fakeAudit struct {
	mu     sync.Mutex
	events []string
}

func (a *fakeAudit) Append(typ, actor, payload string) (eventlog.Event, error) {
	a.mu.Lock()
	a.events = append(a.events, typ+"|"+actor+"|"+payload)
	a.mu.Unlock()
	return eventlog.Event{}, nil
}
func (a *fakeAudit) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.events...)
}

var t0 = time.Unix(1_800_000_000, 0)

func newTestListener(t *testing.T, cfgTOML string) (*Listener, *fakeBackend, *fakeAudit) {
	t.Helper()
	cfg := DefaultCommands()
	if cfgTOML != "" {
		var err error
		cfg, err = ParseCommands([]byte(cfgTOML))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
	}
	be, au := &fakeBackend{}, &fakeAudit{}
	l, err := NewListener(cfg, "https://ntfy.example/topic", be, au)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	l.start = t0
	l.now = func() time.Time { return t0.Add(time.Minute) }
	return l, be, au
}

func msg(id, text string, tags ...string) ntfyMessage {
	return ntfyMessage{ID: id, Event: "message", Time: t0.Unix() + 5, Message: text, Tags: tags}
}

func TestObliviate_EngagesLockdown(t *testing.T) {
	l, be, au := newTestListener(t, "")
	for i, text := range []string{"obliviate", "  OBLIVIATE  ", "Obliviate now please"} {
		be.calls = nil
		l.handle(context.Background(), msg(fmt.Sprint(i), text))
		if got := be.got(); len(got) != 1 || got[0] != "lockdown" {
			t.Fatalf("%q: want lockdown, got %v", text, got)
		}
	}
	if ev := au.all(); len(ev) != 3 || !strings.Contains(ev[0], "keyword=obliviate") || !strings.Contains(ev[0], "action=lockdown") {
		t.Fatalf("audit events: %v", ev)
	}
}

func TestAbuse_SelfEchoIgnored(t *testing.T) {
	l, be, _ := newTestListener(t, "")
	// Shepherd's own outbound notification echoed back on the same topic.
	l.handle(context.Background(), msg("a", "obliviate", OutboundTag))
	if got := be.got(); len(got) != 0 {
		t.Fatalf("self-echo executed: %v", got)
	}
}

func TestAbuse_KeywordMustBeFirstWord(t *testing.T) {
	l, be, _ := newTestListener(t, "")
	l.handle(context.Background(), msg("a", "Execution halted: obliviate (chunk c1 rolled back)"))
	l.handle(context.Background(), msg("b", "obliviated"))
	if got := be.got(); len(got) != 0 {
		t.Fatalf("non-command text executed: %v", got)
	}
}

func TestAbuse_ReplayOfOldMessageIgnored(t *testing.T) {
	l, be, _ := newTestListener(t, "")
	m := msg("old", "obliviate")
	m.Time = t0.Unix() - 3600 // sent before the daemon started
	l.handle(context.Background(), m)
	if got := be.got(); len(got) != 0 {
		t.Fatalf("stale message executed: %v", got)
	}
}

func TestAbuse_DuplicateIDExecutesOnce(t *testing.T) {
	l, be, _ := newTestListener(t, "")
	l.handle(context.Background(), msg("dup", "obliviate"))
	l.handle(context.Background(), msg("dup", "obliviate"))
	if got := be.got(); len(got) != 1 {
		t.Fatalf("duplicate executed %d times", len(got))
	}
}

func TestAbuse_OversizeMessageIgnored(t *testing.T) {
	l, be, _ := newTestListener(t, "")
	l.handle(context.Background(), msg("big", "obliviate "+strings.Repeat("x", MaxMessageBytes)))
	if got := be.got(); len(got) != 0 {
		t.Fatalf("oversize executed: %v", got)
	}
}

func TestAbuse_UnknownKeywordNotAudited(t *testing.T) {
	l, be, au := newTestListener(t, "")
	l.handle(context.Background(), msg("u", "hello world"))
	l.handle(context.Background(), msg("p", "pause")) // not configured in defaults
	if len(be.got()) != 0 || len(au.all()) != 0 {
		t.Fatalf("unknown keyword acted on: calls=%v audit=%v", be.got(), au.all())
	}
}

const demoish = `
schema_version = 1
enabled = true
[[command]]
keyword = "sheep-pause"
action = "pause"
[[command]]
keyword = "sheep-resume"
action = "resume"
[[command]]
keyword = "sheep-hold"
action = "hold"
[[command]]
keyword = "sheep-status"
action = "status"
`

// demoWith inserts top-level keys before the [[command]] tables.
func demoWith(extra string) string {
	return strings.Replace(demoish, "enabled = true\n", "enabled = true\n"+extra+"\n", 1)
}

func TestAbuse_EscalatingActionNeedsOptIn(t *testing.T) {
	l, be, au := newTestListener(t, demoish)
	l.handle(context.Background(), msg("r", "sheep-resume"))
	if got := be.got(); len(got) != 0 {
		t.Fatalf("resume executed without allow_escalating: %v", got)
	}
	if ev := au.all(); len(ev) != 1 || !strings.Contains(ev[0], "rejected") {
		t.Fatalf("rejection not audited: %v", ev)
	}

	l2, be2, _ := newTestListener(t, demoWith("allow_escalating = true"))
	l2.handle(context.Background(), msg("r", "sheep-resume"))
	if got := be2.got(); len(got) != 1 || got[0] != "resume" {
		t.Fatalf("resume with opt-in: %v", got)
	}
}

func TestAbuse_ChunkArgumentValidated(t *testing.T) {
	l, be, _ := newTestListener(t, demoish)
	for i, bad := range []string{"sheep-hold ../x", "sheep-hold -rf", "sheep-hold", "sheep-hold a;b", "sheep-hold a b"} {
		l.handle(context.Background(), msg(fmt.Sprint("b", i), bad))
	}
	if got := be.got(); len(got) != 0 {
		t.Fatalf("bad chunk ids accepted: %v", got)
	}
	l.handle(context.Background(), msg("ok", "sheep-hold S01-c02.auth"))
	if got := be.got(); len(got) != 1 || got[0] != "hold:S01-c02.auth" {
		t.Fatalf("valid hold: %v", got)
	}
}

func TestAbuse_RateLimitSparesObliviate(t *testing.T) {
	l, be, _ := newTestListener(t, demoWith("rate_limit_per_min = 2"))
	for i := 0; i < 5; i++ {
		l.handle(context.Background(), msg(fmt.Sprint("p", i), "sheep-pause"))
	}
	l.handle(context.Background(), msg("z", "obliviate"))
	got := be.got()
	if len(got) != 3 || got[2] != "lockdown" {
		t.Fatalf("want 2 pauses + lockdown, got %v", got)
	}
}

func TestStatusRepliesViaNotify(t *testing.T) {
	l, be, _ := newTestListener(t, demoish)
	l.handle(context.Background(), msg("s", "sheep-status"))
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.notes) != 1 || !strings.Contains(be.notes[0], "state=running") {
		t.Fatalf("status reply: %v", be.notes)
	}
}

func TestParseCommands_Rejects(t *testing.T) {
	cases := map[string]string{
		"unknown key":       "schema_version = 1\nbogus = 1\n",
		"bad schema":        "schema_version = 9\n",
		"remote unlock":     "schema_version = 1\n[[command]]\nkeyword = \"free\"\naction = \"unlock\"\n",
		"obliviate remap":   "schema_version = 1\n[[command]]\nkeyword = \"obliviate\"\naction = \"pause\"\n",
		"duplicate keyword": "schema_version = 1\n[[command]]\nkeyword = \"zz\"\naction = \"pause\"\n[[command]]\nkeyword = \"zz\"\naction = \"stop\"\n",
		"bad keyword":       "schema_version = 1\n[[command]]\nkeyword = \"a b\"\naction = \"pause\"\n",
		"plain http":        "schema_version = 1\nurl = \"http://ntfy.sh/topic\"\n",
		"bad token env":     "schema_version = 1\ntoken_env = \"X; rm\"\n",
	}
	for name, src := range cases {
		if _, err := ParseCommands([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// CRLF + BOM tolerated; obliviate always present even if not listed.
	cfg, err := ParseCommands([]byte("\xef\xbb\xbfschema_version = 1\r\nenabled = true\r\n"))
	if err != nil {
		t.Fatalf("crlf: %v", err)
	}
	if _, ok := cfg.lookup("obliviate"); !ok {
		t.Fatal("obliviate missing")
	}
	// http allowed for loopback/LAN self-hosted ntfy.
	if _, err := ParseCommands([]byte("schema_version = 1\nurl = \"http://127.0.0.1:2586/t\"\n")); err != nil {
		t.Fatalf("loopback http: %v", err)
	}
}

func TestDemoAndDefaultFilesParse(t *testing.T) {
	if _, err := ParseCommands(defaultCommandsTOML); err != nil {
		t.Fatalf("embedded default: %v", err)
	}
	demo, err := LoadCommands("../../examples/ntfy/ntfy-commands.demo.toml")
	if err != nil {
		t.Fatalf("demo: %v", err)
	}
	if demo.Enabled {
		t.Fatal("demo config must ship inactive (enabled = false)")
	}
}

// End-to-end: subscribe to a fake ntfy stream, receive obliviate, reconnect with since=<last id>.
func TestListener_StreamAndReconnect(t *testing.T) {
	var (
		mu     sync.Mutex
		sinces []string
		conns  int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/topic/json") {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tk_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		conns++
		n := conns
		sinces = append(sinces, r.URL.Query().Get("since"))
		mu.Unlock()
		fl := w.(http.Flusher)
		now := time.Now().Unix()
		fmt.Fprintf(w, `{"id":"o%d","time":%d,"event":"open","topic":"topic"}`+"\n", n, now)
		if n == 1 {
			fmt.Fprintf(w, `{"id":"m1","time":%d,"event":"message","topic":"topic","message":"obliviate"}`+"\n", now+1)
		}
		fl.Flush()
		// close stream to force a reconnect
	}))
	defer srv.Close()

	t.Setenv("SHEPHERD_TEST_NTFY_TOKEN", "tk_test")
	cfg, err := ParseCommands([]byte("schema_version = 1\ntoken_env = \"SHEPHERD_TEST_NTFY_TOKEN\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	be, au := &fakeBackend{}, &fakeAudit{}
	l, err := NewListener(cfg, srv.URL+"/topic", be, au)
	if err != nil {
		t.Fatal(err)
	}
	l.backoffMin, l.backoffMax = 10*time.Millisecond, 20*time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		c := conns
		mu.Unlock()
		if c >= 2 && len(be.got()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if got := be.got(); len(got) != 1 || got[0] != "lockdown" {
		t.Fatalf("want one lockdown, got %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sinces) < 2 || sinces[1] != "m1" {
		t.Fatalf("reconnect should resume since last message id, got %v", sinces)
	}
	found := false
	for _, e := range au.all() {
		if strings.HasPrefix(e, "remote.listening|") && strings.Contains(e, "obliviate") {
			found = true
		}
	}
	if !found {
		t.Fatalf("listening keywords not logged: %v", au.all())
	}
}

// After a live topic rotation the listener must drop the old subscription and follow the new topic.
func TestListener_SetURLSwitchesTopic(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.(http.Flusher).Flush()
		<-r.Context().Done() // hold the stream open like ntfy does
	}))
	defer srv.Close()

	be, au := &fakeBackend{}, &fakeAudit{}
	l, err := NewListener(DefaultCommands(), srv.URL+"/old", be, au)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()

	waitFor := func(p string) bool {
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			mu.Lock()
			for _, x := range paths {
				if x == p {
					mu.Unlock()
					return true
				}
			}
			mu.Unlock()
		}
		return false
	}
	if !waitFor("/old/json") {
		t.Fatal("never subscribed to old topic")
	}
	if err := l.SetURL(srv.URL + "/new"); err != nil {
		t.Fatal(err)
	}
	if !waitFor("/new/json") {
		t.Fatalf("did not resubscribe to new topic: %v", paths)
	}
	if err := l.SetURL("http://ntfy.sh/plain"); err == nil {
		t.Fatal("SetURL must validate like NewListener")
	}
	cancel()
	<-done
}
