package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCA_GenerateAndPinning(t *testing.T) {
	rootCA, err := GenerateCA("Shepherd Test CA")
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	if len(rootCA.KeyHash) != 64 {
		t.Fatalf("expected 64-char sha256 hex hash, got: %s", rootCA.KeyHash)
	}

	// Verify pinned hash passes for valid CA
	if err := VerifyPinnedHash(rootCA.Cert, rootCA.KeyHash); err != nil {
		t.Fatalf("VerifyPinnedHash failed for valid CA: %v", err)
	}

	// Abuse test (STRIDE S1): different/forged CA rejected
	otherCA, err := GenerateCA("Attacker Rogue CA")
	if err != nil {
		t.Fatal(err)
	}
	err = VerifyPinnedHash(otherCA.Cert, rootCA.KeyHash)
	if err == nil || !strings.Contains(err.Error(), "CA public-key hash mismatch") {
		t.Fatalf("expected pinning rejection (STRIDE S1), got: %v", err)
	}
}

func TestCA_SaveAndLoadPermissions(t *testing.T) {
	rootCA, err := GenerateCA("Shepherd Persist CA")
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "ca.crt")
	keyPath := filepath.Join(tmpDir, "ca.key")

	if err := rootCA.Save(certPath, keyPath); err != nil {
		t.Fatalf("Save CA failed: %v", err)
	}

	// Assert mode 0600 on private key
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("CA private key permission = %o, expected 0600", fi.Mode().Perm())
	}

	loaded, err := LoadCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadCA failed: %v", err)
	}
	if loaded.KeyHash != rootCA.KeyHash {
		t.Errorf("loaded hash %s != original %s", loaded.KeyHash, rootCA.KeyHash)
	}
}

func TestCA_SignCSRAndLifetime(t *testing.T) {
	rootCA, err := GenerateCA("Shepherd Signer CA")
	if err != nil {
		t.Fatal(err)
	}

	// Generate worker key pair & CSR
	workerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName: "worker-node-1",
		},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, workerKey)
	if err != nil {
		t.Fatal(err)
	}

	// Sign CSR
	certDER, err := rootCA.SignCSR(csrDER, "worker", nil, nil)
	if err != nil {
		t.Fatalf("SignCSR failed: %v", err)
	}

	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	// Verify certificate signed by rootCA
	pool := x509.NewCertPool()
	pool.AddCert(rootCA.Cert)
	opts := x509.VerifyOptions{
		Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := leaf.Verify(opts); err != nil {
		t.Fatalf("leaf certificate verification against root CA failed: %v", err)
	}

	// Verify 7-day lifetime (ADR-0003)
	maxAllowedExpiry := time.Now().Add(DefaultCertLifetime + 5*time.Minute)
	if leaf.NotAfter.After(maxAllowedExpiry) {
		t.Fatalf("certificate lifetime exceeds 7 days: notAfter = %v", leaf.NotAfter)
	}

	// Abuse test: corrupted CSR signature rejected
	corruptedCSR := append([]byte{}, csrDER...)
	corruptedCSR[len(corruptedCSR)-5] ^= 0xFF
	_, err = rootCA.SignCSR(corruptedCSR, "worker", nil, nil)
	if err == nil {
		t.Fatal("expected error for corrupted CSR signature")
	}
}
