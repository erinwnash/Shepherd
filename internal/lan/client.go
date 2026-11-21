package lan

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/erinwnash/Shepherd/internal/ca"
)

// Client communicates with the Shepherd controller over mTLS with pinned CA hash.
type Client struct {
	addr       string
	caHash     string
	workerID   string
	privKey    *ecdsa.PrivateKey
	httpClient *http.Client
}

// NewClient initializes a worker mTLS client with pinned CA verification (STRIDE S1).
func NewClient(addr string, caHash string, workerID string, cert tls.Certificate) (*Client, error) {
	privKey, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("worker certificate private key must be ECDSA")
	}

	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
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

			// Find certificate matching pinned CA key hash (STRIDE S1)
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

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}

	return &Client{
		addr:     addr,
		caHash:   caHash,
		workerID: workerID,
		privKey:  privKey,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
		},
	}, nil
}

// AcquireLease requests a quota reservation for a chunk.
func (c *Client) AcquireLease(ctx context.Context, req LeaseRequest) (*LeaseResponse, error) {
	req.WorkerID = c.workerID
	var resp LeaseResponse
	if err := c.postJSON(ctx, "/api/v1/worker/lease", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Heartbeat renews an active lease.
func (c *Client) Heartbeat(ctx context.Context, leaseID string) error {
	req := HeartbeatRequest{LeaseID: leaseID}
	return c.postJSON(ctx, "/api/v1/worker/heartbeat", req, nil)
}

// FetchAssignment pulls the work order and execution nonce for a chunk.
func (c *Client) FetchAssignment(ctx context.Context, chunkID string) (*FetchResponse, error) {
	req := FetchRequest{WorkerID: c.workerID, ChunkID: chunkID}
	var resp FetchResponse
	if err := c.postJSON(ctx, "/api/v1/worker/fetch", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SubmitResult signs the execution result and sends it to the controller (STRIDE T5).
func (c *Client) SubmitResult(ctx context.Context, chunkID, baseCommit, nonce, status, leaseID string, spendPct int, patch, output string) error {
	sig, err := SignResult(c.privKey, chunkID, baseCommit, nonce)
	if err != nil {
		return fmt.Errorf("sign result: %w", err)
	}

	req := ResultRequest{
		ChunkID:    chunkID,
		BaseCommit: baseCommit,
		Nonce:      nonce,
		Status:     status,
		LeaseID:    leaseID,
		SpendPct:   spendPct,
		Patch:      patch,
		Output:     output,
		Signature:  sig,
	}

	return c.postJSON(ctx, "/api/v1/worker/result", req, nil)
}

func (c *Client) postJSON(ctx context.Context, path string, payload any, dest any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://%s%s", c.addr, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	if dest != nil {
		if err := json.Unmarshal(bodyBytes, dest); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}
