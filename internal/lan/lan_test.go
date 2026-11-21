package lan_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/ca"
	"github.com/erinwnash/Shepherd/internal/config"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/lan"
	"github.com/erinwnash/Shepherd/internal/leases"
	"github.com/erinwnash/Shepherd/internal/pairing"
)

type testHarness struct {
	server     *lan.Server
	rootCA     *ca.CA
	store      *db.Store
	leasesMgr  *leases.Manager
	pairingMgr *pairing.Manager
	provider   *lan.InMemChunkProvider
	addr       string
}

func setupTestLAN(t *testing.T) *testHarness {
	t.Helper()
	dir := t.TempDir()

	rootCA, err := ca.GenerateCA("Shepherd Test CA")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	store, err := db.Open(filepath.Join(dir, "shepherd.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	pairingMgr := pairing.NewManager(rootCA, store)
	pools := map[string]config.PoolConfig{
		"default": {
			Mode:           "polled",
			FloorWeeklyPct: 20, // 80% max spend
		},
	}
	provider := lan.NewInMemChunkProvider()
	leasesMgr := leases.NewManager(store, pools, &leases.FixedUsageProvider{
		Weekly: map[string]int{"default": 50},
	})

	// Bind to 127.0.0.1 on random port
	srv, err := lan.NewServer("127.0.0.1:0", rootCA, store, pairingMgr, leasesMgr, provider)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("Start LAN server: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Close()
	})

	return &testHarness{
		server:     srv,
		rootCA:     rootCA,
		store:      store,
		leasesMgr:  leasesMgr,
		pairingMgr: pairingMgr,
		provider:   provider,
		addr:       srv.Addr().String(),
	}
}

func issueTestWorker(t *testing.T, h *testHarness, workerID, role string) (*lan.Client, tls.Certificate) {
	t.Helper()

	workerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: workerID},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, workerKey)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}

	certDER, err := h.rootCA.SignCSR(csrDER, role, nil, nil)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}

	pubkeyHash, err := ca.ComputeKeyHash(&workerKey.PublicKey)
	if err != nil {
		t.Fatalf("ComputeKeyHash: %v", err)
	}

	err = h.store.UpsertWorker(db.WorkerRecord{
		ID:         workerID,
		PubkeyHash: pubkeyHash,
		Role:       role,
		Status:     "active",
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(7 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("UpsertWorker: %v", err)
	}

	tlsCert := tls.Certificate{
		Certificate: [][]byte{certDER, h.rootCA.Cert.Raw},
		PrivateKey:  workerKey,
	}

	client, err := lan.NewClient(h.addr, h.rootCA.KeyHash, workerID, tlsCert)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	return client, tlsCert
}

func TestLAN_WildcardReject_ADR0003(t *testing.T) {
	rootCA, _ := ca.GenerateCA("Wildcard Test CA")
	_, err := lan.NewServer("0.0.0.0:8443", rootCA, nil, nil, nil, nil)
	if err == nil {
		t.Fatalf("Expected error for 0.0.0.0 bind, got nil")
	}

	_, err = lan.NewServer("[::]:8443", rootCA, nil, nil, nil, nil)
	if err == nil {
		t.Fatalf("Expected error for [::] bind, got nil")
	}
}

func TestLAN_ClientCertAuth_STRIDE_S2(t *testing.T) {
	h := setupTestLAN(t)

	// Make request without client certificate
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	rawClient := &http.Client{Transport: tr}

	resp, err := rawClient.Post(fmt.Sprintf("https://%s/api/v1/worker/lease", h.addr), "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Expected 401 Unauthorized without client cert, got %d", resp.StatusCode)
	}
}

func TestLAN_PinnedCA_STRIDE_S1(t *testing.T) {
	h := setupTestLAN(t)

	// Worker client configured with wrong pinned CA key hash
	_, tlsCert := issueTestWorker(t, h, "worker-1", "worker")
	badClient, err := lan.NewClient(h.addr, "deadbeef1234567890", "worker-1", tlsCert)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx := context.Background()
	_, err = badClient.AcquireLease(ctx, lan.LeaseRequest{ChunkID: "c1", Pool: "default", EstimatedPct: 5})
	if err == nil {
		t.Fatalf("Expected TLS error on mismatched pinned CA hash, got nil")
	}
	if !strings.Contains(err.Error(), "pinned CA hash") {
		t.Fatalf("Expected pinned CA hash error message, got: %v", err)
	}
}

func TestLAN_WorkerRevocation_STRIDE_S4(t *testing.T) {
	h := setupTestLAN(t)
	client, _ := issueTestWorker(t, h, "worker-revoked", "worker")
	ctx := context.Background()

	// Initial acquire succeeds
	l, err := client.AcquireLease(ctx, lan.LeaseRequest{ChunkID: "c1", Pool: "default", EstimatedPct: 5})
	if err != nil {
		t.Fatalf("Initial acquire failed: %v", err)
	}
	if l == nil || l.LeaseID == "" {
		t.Fatalf("Empty lease ID")
	}

	// Revoke worker in database mid-session
	if err := h.store.SetWorkerStatus("worker-revoked", "revoked"); err != nil {
		t.Fatalf("SetWorkerStatus: %v", err)
	}

	// Subsequent request must be rejected with 403 Forbidden
	_, err = client.AcquireLease(ctx, lan.LeaseRequest{ChunkID: "c2", Pool: "default", EstimatedPct: 5})
	if err == nil {
		t.Fatalf("Expected 403 Forbidden for revoked worker, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("Expected HTTP 403 for revoked worker, got: %v", err)
	}
}

func TestLAN_WorkerRoleSeparation_STRIDE_E4(t *testing.T) {
	h := setupTestLAN(t)
	_, workerCert := issueTestWorker(t, h, "worker-unprivileged", "worker")

	// Attempt to call admin endpoint with worker client cert
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates:       []tls.Certificate{workerCert},
			InsecureSkipVerify: true,
		},
	}
	client := &http.Client{Transport: tr}

	resp, err := client.Post(fmt.Sprintf("https://%s/api/v1/admin/config", h.addr), "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Admin POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Expected 403 Forbidden for worker role accessing admin API, got %d", resp.StatusCode)
	}
}

func TestLAN_WorkerIsolation_STRIDE_I3(t *testing.T) {
	h := setupTestLAN(t)
	w1, _ := issueTestWorker(t, h, "worker-1", "worker")
	w2, _ := issueTestWorker(t, h, "worker-2", "worker")

	// Chunk c1 assigned to worker-1
	h.provider.Assignments["c1"] = lan.FetchResponse{
		ChunkID:    "c1",
		Title:      "Feature 1",
		WorkerID:   "worker-1",
		BaseCommit: "commit-abc",
	}

	ctx := context.Background()

	// Worker 1 fetches successfully
	resp1, err := w1.FetchAssignment(ctx, "c1")
	if err != nil {
		t.Fatalf("Worker 1 fetch failed: %v", err)
	}
	if resp1.Nonce == "" {
		t.Fatalf("Nonce was not issued")
	}

	// Worker 2 attempts to fetch worker 1's chunk - must be rejected (STRIDE I3)
	_, err = w2.FetchAssignment(ctx, "c1")
	if err == nil {
		t.Fatalf("Worker 2 should not be able to fetch worker 1's chunk (STRIDE I3)")
	}

	// Worker 2 attempts to submit result for worker 1's chunk - must be rejected (STRIDE I3)
	err = w2.SubmitResult(ctx, "c1", "commit-abc", resp1.Nonce, "done", "", 0, "", "")
	if err == nil {
		t.Fatalf("Worker 2 should not be able to submit result for worker 1's nonce (STRIDE I3)")
	}
}

func TestLAN_ResultSigningAndReplay_STRIDE_T5(t *testing.T) {
	h := setupTestLAN(t)
	client, _ := issueTestWorker(t, h, "worker-sig", "worker")

	h.provider.Assignments["c-sig"] = lan.FetchResponse{
		ChunkID:    "c-sig",
		Title:      "Signing Test",
		BaseCommit: "head-123",
	}

	ctx := context.Background()

	// 1. Acquire lease
	lease, err := client.AcquireLease(ctx, lan.LeaseRequest{ChunkID: "c-sig", Pool: "default", EstimatedPct: 10})
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	// 2. Fetch assignment to get nonce
	assign, err := client.FetchAssignment(ctx, "c-sig")
	if err != nil {
		t.Fatalf("FetchAssignment: %v", err)
	}
	if assign.Nonce == "" {
		t.Fatalf("Expected non-empty nonce")
	}

	// 3. Submit result with valid signature
	err = client.SubmitResult(ctx, "c-sig", assign.BaseCommit, assign.Nonce, "done", lease.LeaseID, 8, "diff", "output")
	if err != nil {
		t.Fatalf("Valid SubmitResult failed: %v", err)
	}

	// Verify result was accepted by provider
	res, ok := h.provider.Results["c-sig"]
	if !ok {
		t.Fatalf("Result not recorded in provider")
	}
	if res.Status != "done" || res.SpendPct != 8 {
		t.Fatalf("Unexpected result record: %+v", res)
	}

	// 4. Replay attack with same nonce should be rejected (STRIDE T5)
	err = client.SubmitResult(ctx, "c-sig", assign.BaseCommit, assign.Nonce, "done", "", 0, "diff", "output")
	if err == nil {
		t.Fatalf("Expected replay submission to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "consumed") && !strings.Contains(err.Error(), "400") {
		t.Fatalf("Expected 400 consumed error on replay, got: %v", err)
	}

	// 5. Submit with tampered signature should be rejected (STRIDE T5)
	// Fetch new nonce
	assign2, err := client.FetchAssignment(ctx, "c-sig")
	if err != nil {
		t.Fatalf("FetchAssignment 2: %v", err)
	}

	// Direct call with tampered base commit (invalidates signature check)
	err = client.SubmitResult(ctx, "c-sig", "wrong-base-commit", assign2.Nonce, "done", "", 0, "", "")
	if err == nil {
		t.Fatalf("Expected submission with wrong base commit to fail signature check")
	}
}

func TestLAN_PayloadCap_STRIDE_D2(t *testing.T) {
	h := setupTestLAN(t)
	_, workerCert := issueTestWorker(t, h, "worker-cap", "worker")

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates:       []tls.Certificate{workerCert},
			InsecureSkipVerify: true,
		},
	}
	client := &http.Client{Transport: tr}

	// Send 2 MiB payload (> 1 MiB limit)
	hugePayload := strings.Repeat("A", 2*1024*1024)
	resp, err := client.Post(fmt.Sprintf("https://%s/api/v1/worker/heartbeat", h.addr), "application/json", strings.NewReader(hugePayload))
	if err != nil {
		// Server may drop or close connection on MaxBytesReader overflow
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 400 {
		t.Fatalf("Expected error status for >1MiB payload, got %d", resp.StatusCode)
	}
}

func TestLAN_PairingFlow_STRIDE_S3(t *testing.T) {
	h := setupTestLAN(t)
	ctx := context.Background()

	// 1. Controller creates pairing token
	pairingStr, err := h.pairingMgr.CreateToken(h.addr)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	workerDir := t.TempDir()
	certFile := filepath.Join(workerDir, "worker.crt")
	keyFile := filepath.Join(workerDir, "worker.key")

	// 2. Remote worker connects and pairs
	pairResp, err := pairing.ClientPair(ctx, pairingStr, "remote-box-1", certFile, keyFile)
	if err != nil {
		t.Fatalf("ClientPair failed: %v", err)
	}
	if pairResp == nil || pairResp.WorkerID != "remote-box-1" {
		t.Fatalf("Unexpected pairing response: %+v", pairResp)
	}

	// 3. Worker record must exist in controller DB
	wRecord, err := h.store.GetWorker("remote-box-1")
	if err != nil {
		t.Fatalf("GetWorker failed: %v", err)
	}
	if wRecord.Status != "active" || wRecord.Role != "worker" {
		t.Fatalf("Unexpected worker record: %+v", wRecord)
	}

	// 4. Replay of same pairing token must fail (single-use per STRIDE S3)
	_, err = pairing.ClientPair(ctx, pairingStr, "remote-box-1", filepath.Join(workerDir, "c2.crt"), filepath.Join(workerDir, "k2.key"))
	if err == nil {
		t.Fatalf("Expected token reuse to be rejected, got nil")
	}
}

