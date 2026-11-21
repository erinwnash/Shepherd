package notify_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/notify"
)

func TestDispatcher_Send_SlackAndDiscord(t *testing.T) {
	var slackReceived string
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		slackReceived = payload.Text
		w.WriteHeader(http.StatusOK)
	}))
	defer slackServer.Close()

	var discordReceived string
	discordServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		discordReceived = payload.Content
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discordServer.Close()

	desktopCalled := false
	cfg := notify.Config{
		Events:            []string{notify.EventChunkHeld, notify.EventQueueBlocked},
		SlackWebhookURL:   slackServer.URL,
		DiscordWebhookURL: discordServer.URL,
		Desktop:           true,
	}

	disp := notify.NewDispatcher(cfg)

	// Test event filter: sprint_done should not be sent
	err := disp.Send(context.Background(), notify.EventSprintDone, "Sprint Finished", "All 10 chunks complete")
	if err != nil {
		t.Fatalf("unexpected error on filtered event: %v", err)
	}
	if slackReceived != "" || discordReceived != "" {
		t.Errorf("filtered event should not have dispatched")
	}

	// Test enabled event: chunk_held
	err = disp.Send(context.Background(), notify.EventChunkHeld, "Chunk 03B", "Requires manual review")
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}

	if slackReceived == "" {
		t.Errorf("expected Slack payload, got empty")
	}
	if discordReceived == "" {
		t.Errorf("expected Discord payload, got empty")
	}
	_ = desktopCalled
}

func TestDispatcher_ShouldSend(t *testing.T) {
	disp := notify.NewDispatcher(notify.Config{
		Events: []string{"chunk_held", "queue_blocked"},
	})

	if !disp.ShouldSend("chunk_held") {
		t.Errorf("expected chunk_held to be sent")
	}
	if !disp.ShouldSend("queue_blocked") {
		t.Errorf("expected queue_blocked to be sent")
	}
	if disp.ShouldSend("sprint_done") {
		t.Errorf("sprint_done should not be sent")
	}

	// Wildcard
	dispWildcard := notify.NewDispatcher(notify.Config{
		Events: []string{"*"},
	})
	if !dispWildcard.ShouldSend("any_event") {
		t.Errorf("wildcard should allow any event")
	}
}

func TestDispatcher_Send_Ntfy(t *testing.T) {
	var receivedTitle, receivedPriority, receivedTags, receivedBody string
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTitle = r.Header.Get("Title")
		receivedPriority = r.Header.Get("Priority")
		receivedTags = r.Header.Get("Tags")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		receivedBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()

	disp := notify.NewDispatcher(notify.Config{
		NtfyURL: ntfyServer.URL,
	})

	// Send chunk started notification
	err := disp.Send(context.Background(), notify.EventChunkStarted, "Chunk Started: 01A", "Started 01A on tier local [model: gemma-4]")
	if err != nil {
		t.Fatalf("unexpected Send error: %v", err)
	}

	if receivedTitle != "Chunk Started: 01A" {
		t.Errorf("expected Title %q, got %q", "Chunk Started: 01A", receivedTitle)
	}
	if receivedBody != "Started 01A on tier local [model: gemma-4]" {
		t.Errorf("expected Body %q, got %q", "Started 01A on tier local [model: gemma-4]", receivedBody)
	}
	if receivedPriority != "default" {
		t.Errorf("expected Priority default, got %q", receivedPriority)
	}
	if receivedTags != "arrow_forward,robot,shepherd-out" {
		t.Errorf("expected Tags arrow_forward,robot, got %q", receivedTags)
	}

	// Send model switch notification
	err = disp.Send(context.Background(), notify.EventModelSwitch, "Model Switch: 01A", "Chunk 01A switched tier from local to flash [attempt 2]")
	if err != nil {
		t.Fatalf("unexpected Send error: %v", err)
	}
	if receivedPriority != "high" {
		t.Errorf("expected Priority high for model switch, got %q", receivedPriority)
	}
	if receivedTags != "twisted_rightwards_arrows,arrows_counterclockwise,shepherd-out" {
		t.Errorf("expected Tags for model switch, got %q", receivedTags)
	}
}

func TestDispatcher_AutoDetectEnv(t *testing.T) {
	t.Setenv("NOTIFY_CMD", "curl -s -d @- ntfy.sh/example-batch-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	disp := notify.NewDispatcher(notify.Config{})
	if disp.NtfyURL() != "https://ntfy.sh/example-batch-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Errorf("expected auto-extracted NtfyURL, got %q", disp.NtfyURL())
	}

	t.Setenv("NTFY_TOPIC", "my-custom-topic")
	t.Setenv("NOTIFY_CMD", "")
	disp2 := notify.NewDispatcher(notify.Config{})
	if disp2.NtfyURL() != "https://ntfy.sh/my-custom-topic" {
		t.Errorf("expected NtfyURL from NTFY_TOPIC, got %q", disp2.NtfyURL())
	}
}

func TestDispatcher_NtfyTokenFromEnv(t *testing.T) {
	var auth, tags string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, tags = r.Header.Get("Authorization"), r.Header.Get("Tags")
	}))
	defer srv.Close()
	t.Setenv("SHEPHERD_TEST_NTFY_TOKEN", "tk_abc")
	disp := notify.NewDispatcher(notify.Config{NtfyURL: srv.URL, NtfyTokenEnv: "SHEPHERD_TEST_NTFY_TOKEN"})
	if err := disp.Send(context.Background(), notify.EventDaemonStarted, "t", "m"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tk_abc" {
		t.Errorf("Authorization = %q", auth)
	}
	if tags != "rocket,shepherd,"+notify.OutboundTag {
		t.Errorf("Tags = %q", tags)
	}
}

// ntfy stamps messages with whole-second times and clients sort by them, so two posts in the
// same second (chunk passed, next chunk started) can display out of order. Posts must be spaced
// so each lands in a later second than the one before.
func TestDispatcher_NtfyPostsLandInDistinctSeconds(t *testing.T) {
	var (
		mu     sync.Mutex
		titles []string
		recvAt []time.Time
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		titles = append(titles, r.Header.Get("Title"))
		recvAt = append(recvAt, time.Now())
		mu.Unlock()
	}))
	defer srv.Close()

	disp := notify.NewDispatcher(notify.Config{NtfyURL: srv.URL})
	ctx := context.Background()
	if err := disp.Send(ctx, notify.EventChunkPassed, "Chunk Passed: A", "a"); err != nil {
		t.Fatal(err)
	}
	if err := disp.Send(ctx, notify.EventChunkStarted, "Chunk Started: B", "b"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(titles) != 2 || titles[0] != "Chunk Passed: A" || titles[1] != "Chunk Started: B" {
		t.Fatalf("order = %v", titles)
	}
	if gap := recvAt[1].Sub(recvAt[0]); gap < time.Second {
		t.Fatalf("second post arrived %v after the first; want >= 1s so ntfy timestamps differ", gap)
	}
}
