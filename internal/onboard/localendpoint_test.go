package onboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFirstLiveRouter_PicksTheServerThatAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5-coder"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	targets := []routerProbeTarget{
		{ID: "dead", BaseURL: "http://127.0.0.1:1/v1"},
		{ID: "live", BaseURL: srv.URL + "/v1"},
	}
	got := FirstLiveRouter(context.Background(), targets, 500*time.Millisecond)
	if got != srv.URL+"/v1" {
		t.Fatalf("got %q, want %q", got, srv.URL+"/v1")
	}
}

func TestFirstLiveRouter_NoneAnswering(t *testing.T) {
	targets := []routerProbeTarget{{ID: "dead", BaseURL: "http://127.0.0.1:1/v1"}}
	if got := FirstLiveRouter(context.Background(), targets, 200*time.Millisecond); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
