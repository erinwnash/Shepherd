package lan

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/ca"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/leases"
	"github.com/erinwnash/Shepherd/internal/pairing"
)

type contextKey string

const workerContextKey contextKey = "shepherd.worker"

// DefaultMaxBodySize is 1 MiB per ADR-0003 and STRIDE D2.
const DefaultMaxBodySize = 1024 * 1024

// WorkerChunkProvider coordinates work assignments and result submission for remote workers.
type WorkerChunkProvider interface {
	FetchAssignment(ctx context.Context, workerID, chunkID string) (*FetchResponse, error)
	SubmitResult(ctx context.Context, workerID string, req ResultRequest) error
}

// InMemChunkProvider is a default implementation of WorkerChunkProvider for testing or standalone execution.
type InMemChunkProvider struct {
	mu          sync.Mutex
	Assignments map[string]FetchResponse // chunkID -> details
	Results     map[string]ResultRequest // chunkID -> submitted result
}

// NewInMemChunkProvider creates a test provider.
func NewInMemChunkProvider() *InMemChunkProvider {
	return &InMemChunkProvider{
		Assignments: make(map[string]FetchResponse),
		Results:     make(map[string]ResultRequest),
	}
}

func (p *InMemChunkProvider) FetchAssignment(ctx context.Context, workerID, chunkID string) (*FetchResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	resp, ok := p.Assignments[chunkID]
	if !ok {
		return nil, errors.New("chunk assignment not found")
	}
	if resp.WorkerID != "" && workerID != "" && resp.WorkerID != workerID {
		return nil, fmt.Errorf("chunk %s is assigned to %s, not %s (STRIDE I3)", chunkID, resp.WorkerID, workerID)
	}
	return &resp, nil
}

func (p *InMemChunkProvider) SubmitResult(ctx context.Context, workerID string, req ResultRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.Results[req.ChunkID] = req
	return nil
}

// NonceInfo tracks issued nonces for (chunk_id, worker_id).
type NonceInfo struct {
	Nonce      string
	WorkerID   string
	BaseCommit string
	Consumed   bool
	CreatedAt  time.Time
}

// Server coordinates the LAN mTLS listener and worker endpoints.
type Server struct {
	bindAddr      string
	ca            *ca.CA
	store         *db.Store
	pairingMgr    *pairing.Manager
	leasesMgr     *leases.Manager
	provider      WorkerChunkProvider
	listener      net.Listener
	httpServer    *http.Server
	serverCert    *tls.Certificate
	mu            sync.Mutex
	nonces        sync.Map // chunkID -> *NonceInfo
	closed        bool
	activeWorkers sync.Map // pubkeyHash -> workerID
}

// NewServer initializes a LAN server with strict address validation and TLS 1.3 configuration.
func NewServer(bindAddr string, rootCA *ca.CA, store *db.Store, pairingMgr *pairing.Manager, leasesMgr *leases.Manager, provider WorkerChunkProvider) (*Server, error) {
	// 1. Validate listener address per ADR-0003 item 7: reject wildcard addresses.
	if err := ValidateListenerAddr(bindAddr); err != nil {
		return nil, err
	}

	if provider == nil {
		provider = NewInMemChunkProvider()
	}

	// 2. Generate controller leaf certificate signed by root CA
	host, _, err := net.SplitHostPort(bindAddr)
	if err != nil {
		host = bindAddr
	}
	serverCert, err := GenerateServerCert(rootCA, []string{host, "localhost", "127.0.0.1"})
	if err != nil {
		return nil, fmt.Errorf("generate server cert: %w", err)
	}

	return &Server{
		bindAddr:   bindAddr,
		ca:         rootCA,
		store:      store,
		pairingMgr: pairingMgr,
		leasesMgr:  leasesMgr,
		provider:   provider,
		serverCert: serverCert,
	}, nil
}

// GenerateServerCert creates a TLS server certificate signed by the root CA.
func GenerateServerCert(rootCA *ca.CA, hosts []string) (*tls.Certificate, error) {
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	var ips []net.IP
	var dnsNames []string
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else if h != "" {
			dnsNames = append(dnsNames, h)
		}
	}

	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   "shepherd-controller",
			Organization: []string{"Shepherd"},
		},
		IPAddresses: ips,
		DNSNames:    dnsNames,
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, serverKey)
	if err != nil {
		return nil, err
	}

	certDER, err := rootCA.SignCSR(csrDER, "controller", ips, dnsNames)
	if err != nil {
		return nil, err
	}

	tlsCert := &tls.Certificate{
		Certificate: [][]byte{certDER, rootCA.Cert.Raw},
		PrivateKey:  serverKey,
	}
	return tlsCert, nil
}

// Start opens the TLS listener on the configured address.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errors.New("server is closed")
	}

	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13, // TLS 1.3 only per ADR-0003
		Certificates: []tls.Certificate{*s.serverCert},
		ClientAuth:   tls.RequestClientCert, // Client cert requested; verified by middleware
	}

	ln, err := tls.Listen("tcp", s.bindAddr, tlsConfig)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.bindAddr, err)
	}
	s.listener = ln

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	go func() {
		_ = s.httpServer.Serve(ln)
	}()

	return nil
}

// Addr returns the listener address.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr()
	}
	return nil
}

// Close gracefully terminates the LAN server.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	if s.httpServer != nil {
		return s.httpServer.Close()
	}
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// 1. Pairing endpoint (STRIDE S3: unauthenticated initially, rate-limited, single-use HMAC token)
	mux.HandleFunc("/api/v1/pairing", s.handlePairing)

	// 2. Worker endpoints (STRIDE S2, S4, T5, I3, D3, E4: mTLS required, verified against CA and DB)
	mux.Handle("/api/v1/worker/lease", s.requireWorkerAuth(http.HandlerFunc(s.handleWorkerLease)))
	mux.Handle("/api/v1/worker/heartbeat", s.requireWorkerAuth(http.HandlerFunc(s.handleWorkerHeartbeat)))
	mux.Handle("/api/v1/worker/fetch", s.requireWorkerAuth(http.HandlerFunc(s.handleWorkerFetch)))
	mux.Handle("/api/v1/worker/result", s.requireWorkerAuth(http.HandlerFunc(s.handleWorkerResult)))
	mux.Handle("/api/v1/worker/renew", s.requireWorkerAuth(http.HandlerFunc(s.handleWorkerRenew)))

	// 3. Admin / operator endpoints: workers MUST NOT reach these (STRIDE E4)
	mux.Handle("/api/v1/admin/config", s.requireRole("admin", http.HandlerFunc(s.handleAdminPlaceholder)))
}

// requireWorkerAuth enforces mTLS, DB identity lookup, active status (STRIDE S4), and 1 MiB body cap (STRIDE D2).
func (s *Server) requireWorkerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Cap request body to 1 MiB (STRIDE D2)
		r.Body = http.MaxBytesReader(w, r.Body, DefaultMaxBodySize)

		// 2. Require client TLS certificate (STRIDE S2)
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required (STRIDE S2)", http.StatusUnauthorized)
			return
		}
		leaf := r.TLS.PeerCertificates[0]

		// 3. Verify certificate is signed by our Root CA
		roots := x509.NewCertPool()
		roots.AddCert(s.ca.Cert)
		opts := x509.VerifyOptions{
			Roots:     roots,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		if _, err := leaf.Verify(opts); err != nil {
			http.Error(w, fmt.Sprintf("invalid client certificate: %v", err), http.StatusUnauthorized)
			return
		}

		// 4. Compute public key hash and check database (STRIDE S2, S4)
		pubkeyHash, err := ca.ComputeKeyHash(leaf.PublicKey)
		if err != nil {
			http.Error(w, "invalid client public key", http.StatusUnauthorized)
			return
		}

		if s.store != nil {
			workerRecord, err := s.store.GetWorkerByPubkey(pubkeyHash)
			if err != nil {
				http.Error(w, "worker certificate not recognized in database (STRIDE S2)", http.StatusUnauthorized)
				return
			}

			// Revocation check on every request (STRIDE S4)
			if workerRecord.Status == "revoked" {
				w.Header().Set("Connection", "close")
				http.Error(w, "worker identity has been revoked (STRIDE S4)", http.StatusForbidden)
				return
			}

			if time.Now().After(workerRecord.ExpiresAt) {
				http.Error(w, "worker certificate has expired; please re-pair", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), workerContextKey, workerRecord)
			r = r.WithContext(ctx)
		}

		next.ServeHTTP(w, r)
	})
}

// requireRole asserts the authenticated certificate has a specific role (STRIDE E4).
func (s *Server) requireRole(allowedRole string, next http.Handler) http.Handler {
	return s.requireWorkerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record, ok := r.Context().Value(workerContextKey).(*db.WorkerRecord)
		if !ok || record == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if record.Role != allowedRole {
			http.Error(w, fmt.Sprintf("role %q is forbidden from accessing %s (STRIDE E4)", record.Role, r.URL.Path), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) handleAdminPlaceholder(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
