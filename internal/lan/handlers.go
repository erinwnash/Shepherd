package lan

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/erinwnash/Shepherd/internal/ca"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/leases"
	"github.com/erinwnash/Shepherd/internal/pairing"
)

func (s *Server) handlePairing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Body cap 1 MiB (STRIDE D2)
	r.Body = http.MaxBytesReader(w, r.Body, DefaultMaxBodySize)

	var req pairing.PairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json payload: %v", err), http.StatusBadRequest)
		return
	}

	if s.pairingMgr == nil {
		http.Error(w, "pairing manager not configured on controller", http.StatusInternalServerError)
		return
	}

	resp, err := s.pairingMgr.HandlePairing(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleWorkerLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	workerRecord, _ := r.Context().Value(workerContextKey).(*db.WorkerRecord)
	if workerRecord != nil && req.WorkerID != "" && req.WorkerID != workerRecord.ID {
		http.Error(w, "worker ID in payload does not match client certificate", http.StatusForbidden)
		return
	}
	if req.WorkerID == "" && workerRecord != nil {
		req.WorkerID = workerRecord.ID
	}

	if s.leasesMgr == nil {
		http.Error(w, "leases manager not initialized", http.StatusInternalServerError)
		return
	}

	lease, err := s.leasesMgr.Acquire(r.Context(), leases.Request{
		WorkerID:     req.WorkerID,
		ChunkID:      req.ChunkID,
		Pool:         req.Pool,
		EstimatedPct: req.EstimatedPct,
		Force:        req.Force,
	})
	if err != nil {
		if errors.Is(err, leases.ErrFloorExceeded) {
			http.Error(w, err.Error(), http.StatusConflict) // 409 or 429
			return
		}
		if errors.Is(err, leases.ErrWorkerRevoked) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := LeaseResponse{
		LeaseID:      lease.ID,
		WorkerID:     lease.WorkerID,
		ChunkID:      lease.ChunkID,
		Pool:         lease.Pool,
		EstimatedPct: lease.EstimatedPct,
		ExpiresAt:    lease.ExpiresAt,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	if s.leasesMgr == nil {
		http.Error(w, "leases manager not initialized", http.StatusInternalServerError)
		return
	}

	if err := s.leasesMgr.Heartbeat(r.Context(), req.LeaseID); err != nil {
		if errors.Is(err, leases.ErrLeaseNotFound) {
			http.Error(w, "lease not found or expired", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleWorkerFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req FetchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	workerRecord, _ := r.Context().Value(workerContextKey).(*db.WorkerRecord)
	if workerRecord != nil && req.WorkerID != "" && req.WorkerID != workerRecord.ID {
		http.Error(w, "cannot fetch work order for another worker (STRIDE I3)", http.StatusForbidden)
		return
	}
	workerID := ""
	if workerRecord != nil {
		workerID = workerRecord.ID
	} else {
		workerID = req.WorkerID
	}

	assignment, err := s.provider.FetchAssignment(r.Context(), workerID, req.ChunkID)
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch assignment: %v", err), http.StatusForbidden)
		return
	}

	// Generate 128-bit random nonce for replay protection (STRIDE T5)
	nonceBytes := make([]byte, 16)
	_, _ = rand.Read(nonceBytes)
	nonce := hex.EncodeToString(nonceBytes)

	// Save active nonce
	s.nonces.Store(req.ChunkID, &NonceInfo{
		Nonce:      nonce,
		WorkerID:   workerID,
		BaseCommit: assignment.BaseCommit,
		Consumed:   false,
		CreatedAt:  time.Now(),
	})

	assignment.Nonce = nonce

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(assignment)
}

func (s *Server) handleWorkerResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	workerRecord, _ := r.Context().Value(workerContextKey).(*db.WorkerRecord)
	workerID := ""
	if workerRecord != nil {
		workerID = workerRecord.ID
	}

	// 1. Validate Nonce (STRIDE T5)
	val, ok := s.nonces.Load(req.ChunkID)
	if !ok {
		http.Error(w, "no active nonce for chunk (STRIDE T5)", http.StatusBadRequest)
		return
	}
	info := val.(*NonceInfo)
	if info.Consumed {
		http.Error(w, "nonce already consumed; replayed result rejected (STRIDE T5)", http.StatusBadRequest)
		return
	}
	if info.Nonce != req.Nonce {
		http.Error(w, "nonce mismatch for chunk (STRIDE T5)", http.StatusBadRequest)
		return
	}
	if info.BaseCommit != "" && req.BaseCommit != info.BaseCommit {
		http.Error(w, "base commit mismatch for chunk (STRIDE T5)", http.StatusBadRequest)
		return
	}
	if workerID != "" && info.WorkerID != workerID {
		http.Error(w, "worker ID mismatch for nonce assignment (STRIDE I3)", http.StatusForbidden)
		return
	}

	// 2. Validate ECDSA Signature (STRIDE T5)
	if len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "peer certificate required", http.StatusUnauthorized)
		return
	}
	clientPubKey, ok := r.TLS.PeerCertificates[0].PublicKey.(*ecdsa.PublicKey)
	if !ok {
		http.Error(w, "peer certificate public key is not ECDSA", http.StatusBadRequest)
		return
	}

	if !VerifyResultSignature(clientPubKey, req.ChunkID, req.BaseCommit, req.Nonce, req.Signature) {
		http.Error(w, "invalid worker cryptographic signature over result (STRIDE T5)", http.StatusForbidden)
		return
	}

	// 3. Mark nonce consumed
	info.Consumed = true

	// 4. Release quota lease if specified
	if req.LeaseID != "" && s.leasesMgr != nil {
		_ = s.leasesMgr.Release(r.Context(), req.LeaseID, req.SpendPct)
	}

	// 5. Submit result
	if err := s.provider.SubmitResult(r.Context(), workerID, req); err != nil {
		http.Error(w, fmt.Sprintf("submit result: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

func (s *Server) handleWorkerRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RenewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	workerRecord, _ := r.Context().Value(workerContextKey).(*db.WorkerRecord)
	if workerRecord == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Renew cert using root CA
	newCertDER, err := s.ca.SignCSR(req.CSRDER, workerRecord.Role, nil, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("sign CSR: %v", err), http.StatusBadRequest)
		return
	}

	// Update DB expiration
	if s.store != nil {
		workerRecord.ExpiresAt = time.Now().Add(ca.DefaultCertLifetime)
		_ = s.store.UpsertWorker(*workerRecord)
	}

	resp := RenewResponse{
		CertDER: newCertDER,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
