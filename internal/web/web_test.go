package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/web"
)

func TestWebServer_IndexHTML(t *testing.T) {
	srv := web.NewServer("127.0.0.1:0", nil, ".", "docs/plan")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "ShepherdAI") || !strings.Contains(body, "dag-svg") {
		t.Errorf("index.html missing ShepherdAI brand or DAG visualizer")
	}
}

func TestWebServer_StatusAndQueue_Disconnected(t *testing.T) {
	srv := web.NewServer("127.0.0.1:0", nil, ".", "docs/plan")

	// Status endpoint with nil client returns IDLE status
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code: %d", w.Code)
	}
	var st control.DaemonStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if st.State != "IDLE" {
		t.Errorf("expected IDLE state, got %s", st.State)
	}

	// Queue endpoint returns valid JSON array
	qReq := httptest.NewRequest(http.MethodGet, "/api/queue", nil)
	qW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(qW, qReq)
	if qW.Code != http.StatusOK {
		t.Fatalf("queue code: %d", qW.Code)
	}
}

func TestWebServer_ActionRequiresPOST(t *testing.T) {
	srv := web.NewServer("127.0.0.1:0", nil, ".", "docs/plan")
	req := httptest.NewRequest(http.MethodGet, "/api/action", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 Method Not Allowed for GET /api/action, got %d", w.Code)
	}
}

func TestWebServer_ActionExecution(t *testing.T) {
	srv := web.NewServer("127.0.0.1:0", nil, ".", "docs/plan")

	// Post action with nil client returns 503
	body := bytes.NewBufferString(`{"action":"pause"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/action", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable when client is nil, got %d", w.Code)
	}
}

func TestWebServer_EventsSSE(t *testing.T) {
	srv := web.NewServer("127.0.0.1:0", nil, ".", "docs/plan")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", w.Header().Get("Content-Type"))
	}
}

func TestWebServer_CSRFAndTokenAuth(t *testing.T) {
	srv := web.NewServer("127.0.0.1:8080", nil, ".", "docs/plan")

	// 1. Cross-Origin POST from evil.example.com must be rejected with 403 Forbidden
	req1 := httptest.NewRequest(http.MethodPost, "/api/action", bytes.NewBufferString(`{"action":"pause"}`))
	req1.Header.Set("Origin", "https://evil.example.com")
	w1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w1, req1)
	if w1.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for cross-origin POST, got %d", w1.Code)
	}

	// 2. Sec-Fetch-Site: cross-site must be rejected with 403 Forbidden
	req2 := httptest.NewRequest(http.MethodPost, "/api/action", bytes.NewBufferString(`{"action":"pause"}`))
	req2.Header.Set("Sec-Fetch-Site", "cross-site")
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for Sec-Fetch-Site: cross-site, got %d", w2.Code)
	}

	// 3. When AuthToken is configured, missing/wrong token returns 401 Unauthorized
	srv.AuthToken = "super-secret-web-token"
	req3 := httptest.NewRequest(http.MethodPost, "/api/action", bytes.NewBufferString(`{"action":"pause"}`))
	req3.Header.Set("Origin", "http://127.0.0.1:8080")
	w3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w3, req3)
	if w3.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized when AuthToken is set and missing from request, got %d", w3.Code)
	}

	// 4. Correct Bearer token passes auth check (reaches 503 because nil client)
	req4 := httptest.NewRequest(http.MethodPost, "/api/action", bytes.NewBufferString(`{"action":"pause"}`))
	req4.Header.Set("Origin", "http://127.0.0.1:8080")
	req4.Header.Set("Authorization", "Bearer super-secret-web-token")
	w4 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w4, req4)
	if w4.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after passing auth check, got %d", w4.Code)
	}
}

