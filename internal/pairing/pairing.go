// Package pairing implements the one-time LAN worker pairing flow per ADR-0003 and STRIDE S3.
// Enforces single-use 128-bit secrets, 10-minute expiry, HMAC-SHA256 over CSR, rate-limited lockout,
// and pinned CA public-key hash verification.
package pairing

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/ca"
	"github.com/erinwnash/Shepherd/internal/db"
)

// PairingTokenLifetime is 10 minutes per ADR-0003.
const PairingTokenLifetime = 10 * time.Minute

// MaxFailedAttempts before locking out pairing.
const MaxFailedAttempts = 5

// LockoutDuration locks pairing on repeated failures.
const LockoutDuration = 5 * time.Minute

// Token holds a generated pairing challenge.
type Token struct {
	Secret    []byte
	CAHash    string
	Addr      string
	ExpiresAt time.Time
	Consumed  bool
}

// Manager manages pairing challenges and rate-limiting on the controller.
type Manager struct {
	mu             sync.Mutex
	ca             *ca.CA
	store          *db.Store
	activeToken    *Token
	failedAttempts int
	lockoutUntil   time.Time
}

// NewManager initializes a PairingManager.
func NewManager(rootCA *ca.CA, store *db.Store) *Manager {
	return &Manager{
		ca:    rootCA,
		store: store,
	}
}

// CreateToken generates a single-use pairing token valid for 10 minutes.
func (m *Manager) CreateToken(addr string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	secret := make([]byte, 16) // 128 bits
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate pairing secret: %w", err)
	}

	tok := &Token{
		Secret:    secret,
		CAHash:    m.ca.KeyHash,
		Addr:      addr,
		ExpiresAt: time.Now().Add(PairingTokenLifetime),
		Consumed:  false,
	}
	m.activeToken = tok

	pairingStr := fmt.Sprintf("%s.%s.%s", hex.EncodeToString(secret), tok.CAHash, addr)
	return pairingStr, nil
}

// PairRequest represents the payload sent by a new worker machine.
type PairRequest struct {
	CSRDER []byte `json:"csr_der"`
	HMAC   []byte `json:"hmac"`
}

// PairResponse contains the issued client certificate.
type PairResponse struct {
	CertDER    []byte `json:"cert_der"`
	WorkerID   string `json:"worker_id"`
	CACertPEM  string `json:"ca_cert_pem"`
	PubkeyHash string `json:"pubkey_hash"`
}

// HandlePairing processes a worker CSR and returns the signed certificate.
func (m *Manager) HandlePairing(req PairRequest) (*PairResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()

	// 1. Check lockout (STRIDE S3)
	if now.Before(m.lockoutUntil) {
		return nil, fmt.Errorf("pairing endpoint locked due to repeated failures; try again after %s", m.lockoutUntil.Format(time.RFC3339))
	}

	// 2. Validate active token
	if m.activeToken == nil || m.activeToken.Consumed {
		m.recordFailureLocked()
		return nil, errors.New("no active pairing challenge; run 'shepherd worker add' on controller")
	}

	if now.After(m.activeToken.ExpiresAt) {
		m.activeToken = nil
		m.recordFailureLocked()
		return nil, errors.New("pairing challenge expired (10 minute validity exceeded)")
	}

	// 3. Verify HMAC-SHA256(secret, CSR)
	mac := hmac.New(sha256.New, m.activeToken.Secret)
	mac.Write(req.CSRDER)
	expectedMAC := mac.Sum(nil)

	if !hmac.Equal(req.HMAC, expectedMAC) {
		m.recordFailureLocked()
		return nil, errors.New("invalid pairing HMAC authentication (STRIDE S3)")
	}

	// 4. Parse CSR & extract worker identity
	csr, err := x509.ParseCertificateRequest(req.CSRDER)
	if err != nil {
		m.recordFailureLocked()
		return nil, fmt.Errorf("invalid CSR: %w", err)
	}

	workerID := csr.Subject.CommonName
	if workerID == "" {
		workerID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}

	pubkeyHash, err := ca.ComputeKeyHash(csr.PublicKey)
	if err != nil {
		return nil, err
	}

	// 5. Sign certificate with role 'worker' (7-day validity)
	certDER, err := m.ca.SignCSR(req.CSRDER, "worker", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("sign worker certificate: %w", err)
	}

	// 6. Record worker identity in DB
	if m.store != nil {
		err = m.store.UpsertWorker(db.WorkerRecord{
			ID:         workerID,
			PubkeyHash: pubkeyHash,
			Role:       "worker",
			Status:     "active",
			CreatedAt:  now.UTC(),
			ExpiresAt:  now.Add(ca.DefaultCertLifetime).UTC(),
		})
		if err != nil {
			return nil, fmt.Errorf("record worker in database: %w", err)
		}
	}

	// 7. Single use: consume token
	m.activeToken.Consumed = true
	m.activeToken = nil
	m.failedAttempts = 0

	caPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: m.ca.Cert.Raw,
	})

	return &PairResponse{
		CertDER:    certDER,
		WorkerID:   workerID,
		CACertPEM:  string(caPEM),
		PubkeyHash: pubkeyHash,
	}, nil
}

func (m *Manager) recordFailureLocked() {
	m.failedAttempts++
	if m.failedAttempts >= MaxFailedAttempts {
		m.lockoutUntil = time.Now().Add(LockoutDuration)
		m.failedAttempts = 0
	}
}

// ClientPair connects from a remote worker to the controller to execute the pairing handshake.
func ClientPair(ctx context.Context, pairingString string, workerID string, outCertPath, outKeyPath string) (*PairResponse, error) {
	parts := strings.SplitN(pairingString, ".", 3)
	if len(parts) != 3 {
		return nil, errors.New("invalid pairing string format (want '<secret>.<cahash>.<addr>')")
	}

	secret, err := hex.DecodeString(parts[0])
	if err != nil || len(secret) != 16 {
		return nil, errors.New("invalid secret hex in pairing string")
	}
	caHash := parts[1]
	controllerAddr := parts[2]

	// 1. Generate local ECDSA P-256 key pair (private key never leaves worker)
	workerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate worker key: %w", err)
	}

	// 2. Build CSR
	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName: workerID,
		},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, workerKey)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}

	// 3. Compute HMAC-SHA256(secret, CSR)
	mac := hmac.New(sha256.New, secret)
	mac.Write(csrDER)
	sig := mac.Sum(nil)

	// 4. Connect over TLS verifying controller cert against pinned CA hash (STRIDE S1)
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, // We perform custom hash pinning instead of system trust store
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("no certificates presented by server")
			}
			var certs []*x509.Certificate
			for _, raw := range rawCerts {
				c, err := x509.ParseCertificate(raw)
				if err != nil {
					return err
				}
				certs = append(certs, c)
			}

			var matchedCA *x509.Certificate
			for _, c := range certs {
				if err := ca.VerifyPinnedHash(c, caHash); err == nil {
					matchedCA = c
					break
				}
			}
			if matchedCA == nil {
				return fmt.Errorf("no certificate in server TLS chain matches pinned CA hash %s (STRIDE S1)", caHash)
			}

			leaf := certs[0]
			if !bytes.Equal(leaf.Raw, matchedCA.Raw) {
				pool := x509.NewCertPool()
				pool.AddCert(matchedCA)
				if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
					return fmt.Errorf("server certificate not verified by pinned CA: %w", err)
				}
			}
			return nil
		},
	}

	tr := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   15 * time.Second,
	}

	reqBody := PairRequest{
		CSRDER: csrDER,
		HMAC:   sig,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("https://%s/api/v1/pairing", controllerAddr)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("pairing request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pairing rejected (HTTP %d): %s", resp.StatusCode, string(respBytes))
	}

	var pairResp PairResponse
	if err := json.Unmarshal(respBytes, &pairResp); err != nil {
		return nil, fmt.Errorf("unmarshal pairing response: %w", err)
	}

	// Save key and cert to disk (0600 on key)
	if outCertPath != "" && outKeyPath != "" {
		_ = os.MkdirAll(filepath.Dir(outCertPath), 0700)
		_ = os.MkdirAll(filepath.Dir(outKeyPath), 0700)

		certPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: pairResp.CertDER,
		})
		if err := os.WriteFile(outCertPath, certPEM, 0644); err != nil {
			return nil, fmt.Errorf("write worker cert: %w", err)
		}

		keyDER, err := x509.MarshalECPrivateKey(workerKey)
		if err != nil {
			return nil, err
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "EC PRIVATE KEY",
			Bytes: keyDER,
		})
		if err := os.WriteFile(outKeyPath, keyPEM, 0600); err != nil {
			return nil, fmt.Errorf("write worker key: %w", err)
		}
	}

	return &pairResp, nil
}
