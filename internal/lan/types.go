// Package lan implements the mTLS remote worker transport, authentication middleware,
// and worker API per ADR-0003 and the Shepherd threat model (STRIDE S1, S2, S3, S4, T5, I3, D2, D3, E4).
package lan

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"time"
)

// Request and response payloads for worker API endpoints.

type LeaseRequest struct {
	WorkerID     string `json:"worker_id"`
	ChunkID      string `json:"chunk_id"`
	Pool         string `json:"pool"`
	EstimatedPct int    `json:"estimated_pct"`
	Force        bool   `json:"force"`
}

type LeaseResponse struct {
	LeaseID      string    `json:"lease_id"`
	WorkerID     string    `json:"worker_id"`
	ChunkID      string    `json:"chunk_id"`
	Pool         string    `json:"pool"`
	EstimatedPct int       `json:"estimated_pct"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type HeartbeatRequest struct {
	LeaseID string `json:"lease_id"`
}

type FetchRequest struct {
	WorkerID string `json:"worker_id"`
	ChunkID  string `json:"chunk_id"`
}

type FetchResponse struct {
	ChunkID     string `json:"chunk_id"`
	WorkerID    string `json:"worker_id,omitempty"`
	Title       string `json:"title"`
	BaseCommit  string `json:"base_commit"`
	BranchName  string `json:"branch_name"`
	Nonce       string `json:"nonce"`
	WorkOrderMD string `json:"work_order_md"`
	Tier        string `json:"tier"`
	Pool        string `json:"pool"`
}

type ResultRequest struct {
	ChunkID    string `json:"chunk_id"`
	BaseCommit string `json:"base_commit"`
	Nonce      string `json:"nonce"`
	Status     string `json:"status"` // done, blocked, failed
	LeaseID    string `json:"lease_id"`
	SpendPct   int    `json:"spend_pct"`
	Patch      string `json:"patch,omitempty"`
	Output     string `json:"output,omitempty"`
	Signature  string `json:"signature_hex"` // ECDSA signature over SHA256(chunk_id + ":" + base_commit + ":" + nonce)
}

type RenewRequest struct {
	CSRDER []byte `json:"csr_der"`
}

type RenewResponse struct {
	CertDER []byte `json:"cert_der"`
}

// ResultDigest generates the canonical SHA-256 digest of (chunk_id, base_commit, nonce) per STRIDE T5.
func ResultDigest(chunkID, baseCommit, nonce string) [32]byte {
	msg := fmt.Sprintf("%s:%s:%s", chunkID, baseCommit, nonce)
	return sha256.Sum256([]byte(msg))
}

// SignResult signs the (chunk_id, base_commit, nonce) digest with worker private key (STRIDE T5).
func SignResult(privKey *ecdsa.PrivateKey, chunkID, baseCommit, nonce string) (string, error) {
	digest := ResultDigest(chunkID, baseCommit, nonce)
	sig, err := ecdsa.SignASN1(rand.Reader, privKey, digest[:])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig), nil
}

// VerifyResultSignature validates the worker's signature over (chunk_id, base_commit, nonce) (STRIDE T5).
func VerifyResultSignature(pubKey *ecdsa.PublicKey, chunkID, baseCommit, nonce, sigHex string) bool {
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	digest := ResultDigest(chunkID, baseCommit, nonce)
	return ecdsa.VerifyASN1(pubKey, digest[:], sig)
}

// ValidateListenerAddr checks that the address specifies an explicit interface and is not wildcard (ADR-0003 item 7).
func ValidateListenerAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsUnspecified() {
			return fmt.Errorf("LAN listener requires an explicit interface address; wildcard addresses (%s) are rejected (ADR-0003)", host)
		}
	} else if host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("LAN listener requires an explicit interface address; wildcard addresses (%s) are rejected (ADR-0003)", host)
	}
	return nil
}
