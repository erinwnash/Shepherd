package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const MaxRequestBodyBytes = 1024 * 1024 // 1 MiB limit per ADR-0003

type Server struct {
	backend    Backend
	socketPath string
	listener   net.Listener
	httpServer *http.Server
	mu         sync.Mutex
	closed     bool
}

type peerCheckingListener struct {
	net.Listener
}

func (l *peerCheckingListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := verifyPeerCred(conn); err != nil {
			_ = conn.Close()
			continue
		}
		return conn, nil
	}
}

// NewServer initializes a new unix control socket server.
func NewServer(backend Backend, socketPath string) *Server {
	if socketPath == "" {
		socketPath = DefaultSocketPath()
	}
	return &Server{
		backend:    backend,
		socketPath: socketPath,
	}
}

// Start opens the unix domain socket and begins serving HTTP requests.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Dir(s.socketPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	// Clean up any stale socket file
	_ = os.Remove(s.socketPath)

	rawListener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket %s: %w", s.socketPath, err)
	}

	// Mode 0600 on socket file
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		_ = rawListener.Close()
		return fmt.Errorf("failed to chmod socket file: %w", err)
	}

	s.listener = &peerCheckingListener{Listener: rawListener}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		_ = s.httpServer.Serve(s.listener)
	}()

	return nil
}

// Close gracefully stops the control server and removes the socket.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	var err error
	if s.httpServer != nil {
		err = s.httpServer.Shutdown(context.Background())
	}
	_ = os.Remove(s.socketPath)
	return err
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/status", s.requireRole(RoleViewer, s.handleStatus))
	mux.HandleFunc("POST /api/v1/start", s.requireRole(RoleOperator, s.handleStart))
	mux.HandleFunc("POST /api/v1/pause", s.requireRole(RoleOperator, s.handlePause))
	mux.HandleFunc("POST /api/v1/resume", s.requireRole(RoleOperator, s.handleResume))
	mux.HandleFunc("POST /api/v1/stop", s.requireRole(RoleOperator, s.handleStop))
	mux.HandleFunc("POST /api/v1/halt", s.requireRole(RoleOperator, s.handleHalt))
	mux.HandleFunc("POST /api/v1/drain", s.requireRole(RoleOperator, s.handleDrain))
	mux.HandleFunc("POST /api/v1/lockdown", s.requireRole(RoleAdmin, s.handleLockdown))
	mux.HandleFunc("POST /api/v1/unlock", s.requireRole(RoleAdmin, s.handleUnlock))
	mux.HandleFunc("POST /api/v1/notify/rotate-topic", s.requireRole(RoleAdmin, s.handleRotateNtfyTopic))

	mux.HandleFunc("GET /api/v1/config", s.requireRole(RoleViewer, s.handleConfigGet))
	mux.HandleFunc("POST /api/v1/config/set", s.requireRole(RoleAdmin, s.handleConfigSet))
	mux.HandleFunc("POST /api/v1/config/reload", s.requireRole(RoleAdmin, s.handleConfigReload))

	mux.HandleFunc("GET /api/v1/queue", s.requireRole(RoleViewer, s.handleQueueList))
	mux.HandleFunc("POST /api/v1/queue/skip", s.requireRole(RoleOperator, s.handleQueueSkip))
	mux.HandleFunc("POST /api/v1/queue/retry", s.requireRole(RoleOperator, s.handleQueueRetry))
	mux.HandleFunc("POST /api/v1/queue/hold", s.requireRole(RoleOperator, s.handleQueueHold))
	mux.HandleFunc("POST /api/v1/queue/release", s.requireRole(RoleOperator, s.handleQueueRelease))
	mux.HandleFunc("POST /api/v1/queue/pin", s.requireRole(RoleOperator, s.handleQueuePin))
	mux.HandleFunc("POST /api/v1/queue/active", s.requireRole(RoleOperator, s.handleQueueActive))
	mux.HandleFunc("POST /api/v1/queue/active-all", s.requireRole(RoleOperator, s.handleQueueActiveAll))
	mux.HandleFunc("POST /api/v1/queue/assign", s.requireRole(RoleOperator, s.handleQueueAssign))
	mux.HandleFunc("POST /api/v1/queue/refresh", s.requireRole(RoleOperator, s.handleQueueRefresh))
	mux.HandleFunc("POST /api/v1/frontrun/start", s.requireRole(RoleOperator, s.handleFrontRunStart))
	mux.HandleFunc("POST /api/v1/frontrun/stop", s.requireRole(RoleOperator, s.handleFrontRunStop))
	mux.HandleFunc("POST /api/v1/queue/check", s.requireRole(RoleOperator, s.handleQueueCheck))
	mux.HandleFunc("GET /api/v1/queue/diff", s.requireRole(RoleViewer, s.handleQueueDiff))
	mux.HandleFunc("POST /api/v1/queue/approve", s.requireRole(RoleOperator, s.handleQueueApprove))
	mux.HandleFunc("POST /api/v1/queue/reject", s.requireRole(RoleOperator, s.handleQueueReject))

	mux.HandleFunc("GET /api/v1/quota", s.requireRole(RoleViewer, s.handleQuotaGet))
	mux.HandleFunc("POST /api/v1/quota/set-floor", s.requireRole(RoleOperator, s.handleQuotaSetFloor))
	mux.HandleFunc("POST /api/v1/quota/set-reserve", s.requireRole(RoleOperator, s.handleQuotaSetReserve))
	mux.HandleFunc("POST /api/v1/quota/tier", s.requireRole(RoleOperator, s.handleQuotaTier))
	mux.HandleFunc("POST /api/v1/quota/reset-bucket", s.requireRole(RoleOperator, s.handleQuotaResetBucket))

	mux.HandleFunc("GET /api/v1/audit", s.requireRole(RoleViewer, s.handleAudit))
}

func roleLevel(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

func (s *Server) requireRole(minRole string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)

		// Default to RoleAdmin for peer-authenticated local owner
		role := RoleAdmin

		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			resolvedRole, err := s.backend.ValidateToken(r.Context(), token)
			if err != nil {
				writeJSONError(w, http.StatusUnauthorized, "invalid bearer token")
				return
			}
			role = resolvedRole
		}

		if roleLevel(role) < roleLevel(minRole) {
			writeJSONError(w, http.StatusForbidden, fmt.Sprintf("role %q does not satisfy required role %q", role, minRole))
			return
		}

		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, val any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(val)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.backend.Status(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.Start(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.Pause(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.Resume(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.Stop(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

func (s *Server) handleHalt(w http.ResponseWriter, r *http.Request) {
	var req HaltRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := s.backend.Halt(r.Context(), req.Reason); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "halted"})
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	var req DrainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.backend.Drain(r.Context(), req.Enabled, req.Until); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "drain mode updated", "drain": req.Enabled})
}

func (s *Server) handleLockdown(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.Lockdown(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "lockdown active"})
}

func (s *Server) handleRotateNtfyTopic(w http.ResponseWriter, r *http.Request) {
	topic, err := s.backend.RotateNtfyTopic(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ntfy_topic": topic})
}

func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.Unlock(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unlocked"})
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.backend.ConfigGet(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	var req ConfigSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		writeJSONError(w, http.StatusBadRequest, "key and value required")
		return
	}
	if err := s.backend.ConfigSet(r.Context(), req.Key, req.Value); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "override scheduled for next chunk boundary"})
}

func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.ConfigReload(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "config reloaded"})
}

func (s *Server) handleQueueList(w http.ResponseWriter, r *http.Request) {
	chunks, err := s.backend.QueueList(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, chunks)
}

func (s *Server) handleQueueSkip(w http.ResponseWriter, r *http.Request) {
	var req QueueActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id required")
		return
	}
	if err := s.backend.QueueSkip(r.Context(), req.ChunkID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "chunk skipped"})
}

func (s *Server) handleQueueRetry(w http.ResponseWriter, r *http.Request) {
	var req QueueActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id required")
		return
	}
	if err := s.backend.QueueRetry(r.Context(), req.ChunkID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "chunk retrying"})
}

func (s *Server) handleQueueHold(w http.ResponseWriter, r *http.Request) {
	var req QueueActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id required")
		return
	}
	if err := s.backend.QueueHold(r.Context(), req.ChunkID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "chunk held"})
}

func (s *Server) handleQueueRelease(w http.ResponseWriter, r *http.Request) {
	var req QueueActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id required")
		return
	}
	if err := s.backend.QueueRelease(r.Context(), req.ChunkID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "chunk released"})
}

func (s *Server) handleQueuePin(w http.ResponseWriter, r *http.Request) {
	var req QueuePinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id required")
		return
	}
	if err := s.backend.QueuePin(r.Context(), req.ChunkID, req.Tier, req.Pool); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "chunk pinned"})
}

func (s *Server) handleQueueActive(w http.ResponseWriter, r *http.Request) {
	var req QueueActiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id required")
		return
	}
	if err := s.backend.QueueSetActive(r.Context(), req.ChunkID, req.Active); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "chunk updated"})
}

func (s *Server) handleQueueActiveAll(w http.ResponseWriter, r *http.Request) {
	var req QueueActiveAllRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	n, err := s.backend.QueueSetAllActive(r.Context(), req.Active)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, QueueActiveAllResult{Changed: n})
}

func (s *Server) handleQueueAssign(w http.ResponseWriter, r *http.Request) {
	var req QueueAssignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id required")
		return
	}
	if err := s.backend.QueueAssign(r.Context(), req.ChunkID, req.Tier, req.Model); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "chunk assigned"})
}

func (s *Server) handleQueueRefresh(w http.ResponseWriter, r *http.Request) {
	res, err := s.backend.QueueRefresh(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleFrontRunStart(w http.ResponseWriter, r *http.Request) {
	var req FrontRunStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Tier == "" {
		writeJSONError(w, http.StatusBadRequest, "tier required")
		return
	}
	if err := s.backend.FrontRunStart(r.Context(), req.Tier, req.Model); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "front-run started"})
}

func (s *Server) handleFrontRunStop(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.FrontRunStop(r.Context()); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "front-run stopped"})
}

func (s *Server) handleQueueCheck(w http.ResponseWriter, r *http.Request) {
	var req QueueActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id required")
		return
	}
	res, err := s.backend.QueueCheck(r.Context(), req.ChunkID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleQueueDiff(w http.ResponseWriter, r *http.Request) {
	chunkID := r.URL.Query().Get("chunk_id")
	if chunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id query parameter required")
		return
	}
	rb, ok := s.backend.(ReviewBackend)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{"chunk_id": chunkID, "diff": ""})
		return
	}
	diff, err := rb.QueueDiff(r.Context(), chunkID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"chunk_id": chunkID, "diff": diff})
}

func (s *Server) handleQueueApprove(w http.ResponseWriter, r *http.Request) {
	var req QueueActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id required")
		return
	}
	if rb, ok := s.backend.(ReviewBackend); ok {
		if err := rb.QueueApprove(r.Context(), req.ChunkID); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "chunk approved and committed"})
		return
	}
	if err := s.backend.QueueRelease(r.Context(), req.ChunkID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "chunk approved"})
}

func (s *Server) handleQueueReject(w http.ResponseWriter, r *http.Request) {
	var req QueueRejectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "chunk_id required")
		return
	}
	if rb, ok := s.backend.(ReviewBackend); ok {
		if err := rb.QueueReject(r.Context(), req.ChunkID, req.Reason); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "chunk rejected"})
		return
	}
	if err := s.backend.QueueHold(r.Context(), req.ChunkID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "chunk rejected"})
}

func (s *Server) handleQuotaGet(w http.ResponseWriter, r *http.Request) {
	q, err := s.backend.QuotaGet(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) handleQuotaSetFloor(w http.ResponseWriter, r *http.Request) {
	var req QuotaSetPctRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Pool == "" {
		writeJSONError(w, http.StatusBadRequest, "pool and pct required")
		return
	}
	if err := s.backend.QuotaSetFloor(r.Context(), req.Pool, req.Pct); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "floor updated"})
}

func (s *Server) handleQuotaSetReserve(w http.ResponseWriter, r *http.Request) {
	var req QuotaSetPctRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Pool == "" {
		writeJSONError(w, http.StatusBadRequest, "pool and pct required")
		return
	}
	if err := s.backend.QuotaSetReserve(r.Context(), req.Pool, req.Pct); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reserve updated"})
}

func (s *Server) handleQuotaTier(w http.ResponseWriter, r *http.Request) {
	var req QuotaTierRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Tier == "" {
		writeJSONError(w, http.StatusBadRequest, "tier and enabled required")
		return
	}
	if err := s.backend.QuotaTierToggle(r.Context(), req.Tier, req.Enabled); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "tier updated"})
}

func (s *Server) handleQuotaResetBucket(w http.ResponseWriter, r *http.Request) {
	var req QuotaResetBucketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Pool == "" {
		writeJSONError(w, http.StatusBadRequest, "pool required")
		return
	}
	if err := s.backend.QuotaResetBucket(r.Context(), req.Pool); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "bucket reset"})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if limStr := r.URL.Query().Get("limit"); limStr != "" {
		if parsed, err := strconv.Atoi(limStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	events, err := s.backend.Audit(r.Context(), limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, events)
}
