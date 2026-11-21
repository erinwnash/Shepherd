package redact_test

import (
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/redact"
)

func TestRedact_ExactValues_STRIDE_I1(t *testing.T) {
	r := redact.New()
	plantedToken := "very-confidential-pairing-secret-12345"
	r.RegisterSecret(plantedToken)

	input := "Output log: worker presented secret=" + plantedToken + " during handshake"
	cleaned := r.Redact(input)

	if strings.Contains(cleaned, plantedToken) {
		t.Fatalf("Planted secret leaked into output: %s", cleaned)
	}
	if !strings.Contains(cleaned, redact.RedactionMarker) {
		t.Fatalf("Expected redaction marker, got: %s", cleaned)
	}
}

func TestRedact_PatternTokens_STRIDE_I1(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"openai_key", "Found key sk-abcdef12345678901234567890 in env"},
		{"anthropic_key", "Authorization: sk-ant-api03-abcdef12345678901234567890"},
		{"github_pat", "token is ghp_11223344556677889900aabbccddeeff1234"},
		{"bearer_auth", "Header: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"},
		{"private_key", "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEI...secretbytes...\n-----END EC PRIVATE KEY-----"},
		{"password_assign", "DB_PASSWORD=superSecretPassword123!"},
	}

	r := redact.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleaned := r.Redact(tc.input)
			if !strings.Contains(cleaned, redact.RedactionMarker) {
				t.Fatalf("Pattern %s was not redacted: got %q", tc.name, cleaned)
			}
		})
	}
}

func TestRedact_GlobalHelpers(t *testing.T) {
	redact.RegisterSecret("super-secret-token-xyz")
	out := redact.Sanitize("Logging token: super-secret-token-xyz")
	if strings.Contains(out, "super-secret-token-xyz") {
		t.Fatalf("Global Sanitize failed to redact secret: %s", out)
	}
}
