package web

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/eventlog"
	"github.com/erinwnash/Shepherd/internal/onboard"
	"github.com/erinwnash/Shepherd/internal/plan"
)

//go:embed static/index.html
var indexHTML []byte

// Server provides the embedded web dashboard and visual DAG graph.
type Server struct {
	Addr        string
	Client      *control.Client
	ProjectRoot string
	PlanRoot    string
	AuthToken   string

	httpServer *http.Server
	clientsMu  sync.Mutex
	sseChans   map[chan string]struct{}
}

// NewServer creates a new web dashboard server.
func NewServer(addr string, client *control.Client, projectRoot, planRoot string) *Server {
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	if projectRoot == "" {
		projectRoot = "."
	}
	if planRoot == "" {
		planRoot = "docs/plan"
	}

	s := &Server{
		Addr:        addr,
		Client:      client,
		ProjectRoot: projectRoot,
		PlanRoot:    planRoot,
		AuthToken:   strings.TrimSpace(os.Getenv("SHEPHERD_WEB_TOKEN")),
		sseChans:    make(map[chan string]struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/queue", s.handleQueue)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/diff", s.handleDiff)
	mux.HandleFunc("/api/events", s.handleEventsSSE)
	mux.HandleFunc("/api/action", s.handleAction)

	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	return s
}

// Handler returns the underlying http.Handler (useful for httptest).
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Start launches the HTTP server and blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.Addr, err)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutdownCtx)
	}()

	// Start background SSE broadcaster
	go s.broadcastLoop(ctx)

	if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) buildFallbackStack() []string {
	var stack []string
	hasLocalOC := false

	if cfg, err := config.LoadFile(filepath.Join(s.ProjectRoot, "shepherd.toml")); err == nil && cfg != nil {
		names := make([]string, 0, len(cfg.Tiers))
		for k := range cfg.Tiers {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			tc := cfg.Tiers[k]
			if k == "local" || k == "opencode" || tc.Adapter == "opencode" || tc.Adapter == "openai-compat" {
				hasLocalOC = true
			}
			desc := fmt.Sprintf("%s (%s", k, tc.Adapter)
			if tc.Model != "" {
				desc += ":" + tc.Model
			}
			if tc.URL != "" {
				desc += " @ " + tc.URL
			}
			desc += ")"
			stack = append(stack, desc)
		}
	}

	ocInfo := onboard.DetectOpenCodeInfo(s.ProjectRoot)
	if !hasLocalOC && (ocInfo.Installed || ocInfo.ConfigPath != "") {
		if summary := ocInfo.Summary(); summary != "" {
			stack = append([]string{"local (" + summary + ")"}, stack...)
		}
	}

	if len(stack) == 0 {
		env := onboard.DetectEnvironment(s.ProjectRoot)
		stack = append(stack, env.InstalledCLIs...)
	}
	return stack
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.Client == nil {
		_ = json.NewEncoder(w).Encode(control.DaemonStatus{
			State: "IDLE",
			Stack: s.buildFallbackStack(),
		})
		return
	}
	st, err := s.Client.Status(r.Context())
	if err != nil {
		_ = json.NewEncoder(w).Encode(control.DaemonStatus{
			State: "DISCONNECTED",
			Stack: s.buildFallbackStack(),
		})
		return
	}
	if len(st.Stack) == 0 {
		st.Stack = s.buildFallbackStack()
	}
	_ = json.NewEncoder(w).Encode(st)
}

type ChunkGraphNode struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Tier      string   `json:"tier,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	DependsOn []string `json:"depends_on"`
	Files     []string `json:"files,omitempty"`
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// 1. Get live status records from control socket if available
	var dbRecords []db.ChunkRecord
	if s.Client != nil {
		dbRecords, _ = s.Client.QueueList(r.Context())
	}
	dbMap := make(map[string]db.ChunkRecord)
	for _, rec := range dbRecords {
		dbMap[rec.ID] = rec
	}

	// 2. Load plan work orders to get titles, files, and dependency graph
	orders, _, _ := plan.Load(s.ProjectRoot, s.PlanRoot)
	var nodes []ChunkGraphNode

	seen := make(map[string]bool)
	for _, o := range orders {
		status := string(o.Status)
		tier := ""
		reason := ""

		if rec, exists := dbMap[o.ID]; exists {
			if rec.Status != "" {
				status = rec.Status
			}
			tier = rec.Tier
			reason = rec.BlockedReason
		}

		var files []string
		for _, f := range o.Files {
			files = append(files, f.Path)
		}

		nodes = append(nodes, ChunkGraphNode{
			ID:        o.ID,
			Title:     o.Title,
			Status:    status,
			Tier:      tier,
			Reason:    reason,
			DependsOn: o.DependsOn,
			Files:     files,
		})
		seen[o.ID] = true
	}

	// Include any DB-only chunks not in plan tree
	for _, rec := range dbRecords {
		if !seen[rec.ID] {
			nodes = append(nodes, ChunkGraphNode{
				ID:     rec.ID,
				Title:  rec.Title,
				Status: rec.Status,
				Tier:   rec.Tier,
				Reason: rec.BlockedReason,
			})
		}
	}

	_ = json.NewEncoder(w).Encode(nodes)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.Client == nil {
		_ = json.NewEncoder(w).Encode([]eventlog.Event{})
		return
	}
	events, err := s.Client.Audit(r.Context(), 100)
	if err != nil {
		_ = json.NewEncoder(w).Encode([]eventlog.Event{})
		return
	}
	_ = json.NewEncoder(w).Encode(events)
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	chunkID := strings.TrimSpace(r.URL.Query().Get("chunk_id"))
	if chunkID == "" {
		http.Error(w, "chunk_id is required", http.StatusBadRequest)
		return
	}
	if s.Client == nil {
		http.Error(w, "daemon client disconnected", http.StatusServiceUnavailable)
		return
	}
	diff, err := s.Client.QueueDiff(r.Context(), chunkID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{
		"chunk_id": chunkID,
		"diff":     diff,
	})
}

type ActionPayload struct {
	Action  string `json:"action"`
	ChunkID string `json:"chunk_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func isAllowedOrigin(r *http.Request) bool {
	if secSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); secSite == "cross-site" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	hostOnly := u.Hostname()
	return hostOnly == "localhost" || hostOnly == "127.0.0.1" || hostOnly == "::1"
}

func (s *Server) checkActionAuth(r *http.Request) bool {
	if s.AuthToken == "" {
		return true
	}
	tok := strings.TrimSpace(r.Header.Get("X-Shepherd-Token"))
	if tok == "" {
		authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(authHdr), "bearer ") {
			tok = strings.TrimSpace(authHdr[7:])
		}
	}
	if tok == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.AuthToken)) == 1
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !isAllowedOrigin(r) {
		http.Error(w, "forbidden: cross-origin request blocked", http.StatusForbidden)
		return
	}

	if !s.checkActionAuth(r) {
		http.Error(w, "unauthorized: valid bearer token required", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)

	var payload ActionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.Client == nil {
		http.Error(w, "daemon client disconnected", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()
	var actErr error

	switch strings.ToLower(payload.Action) {
	case "start":
		actErr = s.Client.Start(ctx)
	case "stop":
		actErr = s.Client.Stop(ctx)
	case "pause":
		actErr = s.Client.Pause(ctx)
	case "resume":
		actErr = s.Client.Resume(ctx)
	case "drain":
		actErr = s.Client.Drain(ctx, true, nil)
	case "halt":
		reason := payload.Reason
		if reason == "" {
			reason = "Web dashboard manual halt"
		}
		actErr = s.Client.Halt(ctx, reason)
	case "skip":
		actErr = s.Client.QueueSkip(ctx, payload.ChunkID)
	case "retry":
		actErr = s.Client.QueueRetry(ctx, payload.ChunkID)
	case "hold":
		actErr = s.Client.QueueHold(ctx, payload.ChunkID)
	case "release":
		actErr = s.Client.QueueRelease(ctx, payload.ChunkID)
	case "approve":
		actErr = s.Client.QueueApprove(ctx, payload.ChunkID)
	case "reject":
		actErr = s.Client.QueueReject(ctx, payload.ChunkID, payload.Reason)
	default:
		http.Error(w, "unsupported action", http.StatusBadRequest)
		return
	}

	if actErr != nil {
		http.Error(w, actErr.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleEventsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 10)
	s.clientsMu.Lock()
	s.sseChans[ch] = struct{}{}
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.sseChans, ch)
		s.clientsMu.Unlock()
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

func (s *Server) broadcastLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastAuditID int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.Client == nil {
				continue
			}
			events, err := s.Client.Audit(ctx, 10)
			if err == nil {
				for _, ev := range events {
					if ev.Seq > lastAuditID {
						lastAuditID = ev.Seq
						data, _ := json.Marshal(ev)
						s.broadcast(string(data))
					}
				}
			}
		}
	}
}

func (s *Server) broadcast(msg string) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()

	for ch := range s.sseChans {
		select {
		case ch <- msg:
		default:
		}
	}
}
