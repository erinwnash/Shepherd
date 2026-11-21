package pairing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erinwnash/Shepherd/internal/ca"
	"github.com/erinwnash/Shepherd/internal/db"
)

func setupTestPairingEnv(t *testing.T) (*Manager, *ca.CA, *db.Store) {
	t.Helper()
	rootCA, err := ca.GenerateCA("Test Root CA")
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	store, err := db.Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	mgr := NewManager(rootCA, store)
	return mgr, rootCA, store
}

func TestPairing_SuccessfulFlowAndSingleUse(t *testing.T) {
	mgr, rootCA, store := setupTestPairingEnv(t)

	// Setup HTTPS test server mimicking controller pairing endpoint
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/pairing", func(w http.ResponseWriter, r *http.Request) {
		var req PairRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resp, err := mgr.HandlePairing(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	// Server TLS cert signed by rootCA
	serverKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "127.0.0.1"},
	}
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, csrTemplate, serverKey)
	serverCertDER, err := rootCA.SignCSR(csrDER, "admin", []net.IP{net.ParseIP("127.0.0.1")}, []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}

	serverCert := tls.Certificate{
		Certificate: [][]byte{serverCertDER, rootCA.Cert.Raw},
		PrivateKey:  serverKey,
	}

	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
	}
	ts.StartTLS()
	defer ts.Close()

	serverAddr := ts.Listener.Addr().String()

	// 1. Controller creates pairing token
	pairingStr, err := mgr.CreateToken(serverAddr)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// 2. Client pairs
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "worker.crt")
	keyPath := filepath.Join(tmpDir, "worker.key")

	ctx := context.Background()
	pairResp, err := ClientPair(ctx, pairingStr, "worker-node-alpha", certPath, keyPath)
	if err != nil {
		t.Fatalf("ClientPair failed: %v", err)
	}

	if pairResp.WorkerID != "worker-node-alpha" {
		t.Errorf("expected workerID 'worker-node-alpha', got: %s", pairResp.WorkerID)
	}

	// Verify worker saved in DB
	wRec, err := store.GetWorker("worker-node-alpha")
	if err != nil || wRec.Role != "worker" || wRec.Status != "active" {
		t.Fatalf("worker not properly recorded in DB: %+v (err: %v)", wRec, err)
	}

	// 3. Single-use test (STRIDE S3 replay): reusing same pairing string must fail
	_, err = ClientPair(ctx, pairingStr, "worker-node-beta", certPath, keyPath)
	if err == nil || !strings.Contains(err.Error(), "no active pairing challenge") {
		t.Fatalf("expected replay rejection (STRIDE S3), got: %v", err)
	}
}

func TestPairing_Abuse_WrongHMAC(t *testing.T) {
	mgr, _, _ := setupTestPairingEnv(t)
	_, err := mgr.CreateToken("127.0.0.1:8443")
	if err != nil {
		t.Fatal(err)
	}

	fakeCSR := []byte("fake-csr-bytes")
	wrongMAC := []byte("01234567890123456789012345678901")

	_, err = mgr.HandlePairing(PairRequest{
		CSRDER: fakeCSR,
		HMAC:   wrongMAC,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid pairing HMAC authentication") {
		t.Fatalf("expected HMAC rejection, got: %v", err)
	}
}

func TestPairing_Abuse_ExpiredToken(t *testing.T) {
	mgr, _, _ := setupTestPairingEnv(t)
	_, _ = mgr.CreateToken("127.0.0.1:8443")

	// Artificially expire token
	mgr.activeToken.ExpiresAt = time.Now().Add(-1 * time.Minute)

	mac := hmac.New(sha256.New, mgr.activeToken.Secret)
	mac.Write([]byte("csr"))
	sig := mac.Sum(nil)

	_, err := mgr.HandlePairing(PairRequest{
		CSRDER: []byte("csr"),
		HMAC:   sig,
	})
	if err == nil || !strings.Contains(err.Error(), "pairing challenge expired") {
		t.Fatalf("expected expiration rejection, got: %v", err)
	}
}

func TestPairing_Abuse_RateLimitLockout(t *testing.T) {
	mgr, _, _ := setupTestPairingEnv(t)
	_, _ = mgr.CreateToken("127.0.0.1:8443")

	// 5 failed attempts
	for i := 0; i < MaxFailedAttempts; i++ {
		_, _ = mgr.HandlePairing(PairRequest{
			CSRDER: []byte("bad"),
			HMAC:   []byte("bad"),
		})
	}

	// 6th attempt must report lockout
	_, err := mgr.HandlePairing(PairRequest{
		CSRDER: []byte("any"),
		HMAC:   []byte("any"),
	})
	if err == nil || !strings.Contains(err.Error(), "pairing endpoint locked") {
		t.Fatalf("expected lockout error, got: %v", err)
	}
}
