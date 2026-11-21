// Package ca implements the private ECDSA P-256 Certificate Authority and public-key hash pinning per ADR-0003.
// Handles certificate issuance, 7-day lifetimes, role extensions, and strict pin validation without system trust stores.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DefaultCertLifetime is 7 days per ADR-0003.
const DefaultCertLifetime = 7 * 24 * time.Hour

// CA represents a Shepherd private root Certificate Authority.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	KeyHash string // SHA-256 hex digest of DER public key
}

// ComputeKeyHash computes the SHA-256 hex digest over DER-encoded public key bytes.
func ComputeKeyHash(pubKey any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pubKey)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// GenerateCA creates a new root Certificate Authority using ECDSA P-256.
func GenerateCA(commonName string) (*CA, error) {
	if commonName == "" {
		commonName = "Shepherd Root CA"
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ECDSA key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"Shepherd"},
		},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years for root CA
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	hash, err := ComputeKeyHash(&key.PublicKey)
	if err != nil {
		return nil, err
	}

	return &CA{
		Cert:    cert,
		Key:     key,
		KeyHash: hash,
	}, nil
}

// Save writes the CA certificate and private key to disk (private key mode 0600 per ADR-0003).
func (c *CA) Save(certPath, keyPath string) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: c.Cert.Raw,
	})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return fmt.Errorf("write ca cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(c.Key)
	if err != nil {
		return fmt.Errorf("marshal EC private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("write ca key: %w", err)
	}

	return nil
}

// LoadCA loads an existing CA from PEM files.
func LoadCA(certPath, keyPath string) (*CA, error) {
	certData, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read ca cert: %w", err)
	}
	certBlock, _ := pem.Decode(certData)
	if certBlock == nil {
		return nil, errors.New("invalid ca cert PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse ca cert: %w", err)
	}

	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read ca key: %w", err)
	}
	keyBlock, _ := pem.Decode(keyData)
	if keyBlock == nil {
		return nil, errors.New("invalid ca key PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse ca EC private key: %w", err)
	}

	hash, err := ComputeKeyHash(&key.PublicKey)
	if err != nil {
		return nil, err
	}

	return &CA{
		Cert:    cert,
		Key:     key,
		KeyHash: hash,
	}, nil
}

// SignCSR signs a client Certificate Signing Request, enforcing 7-day lifetime (ADR-0003).
func (c *CA) SignCSR(csrDER []byte, role string, ips []net.IP, dnsNames []string) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("invalid CSR signature: %w", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   csr.Subject.CommonName,
			Organization: []string{"Shepherd"},
			ExtraNames: []pkix.AttributeTypeAndValue{
				{
					Type:  []int{1, 3, 6, 1, 4, 1, 99999, 1}, // custom OID for Shepherd role
					Value: role,
				},
			},
		},
		NotBefore:   time.Now().Add(-5 * time.Minute),
		NotAfter:    time.Now().Add(DefaultCertLifetime),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		IPAddresses: ips,
		DNSNames:    dnsNames,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, c.Cert, csr.PublicKey, c.Key)
	if err != nil {
		return nil, fmt.Errorf("sign certificate: %w", err)
	}

	return certDER, nil
}

// VerifyPinnedHash asserts that the leaf or issuing cert matches the pinned public key hash (STRIDE S1).
func VerifyPinnedHash(cert *x509.Certificate, pinnedHash string) error {
	hash, err := ComputeKeyHash(cert.PublicKey)
	if err != nil {
		return err
	}
	if hash != pinnedHash {
		return fmt.Errorf("CA public-key hash mismatch: got %s, want pinned %s (STRIDE S1)", hash, pinnedHash)
	}
	return nil
}
