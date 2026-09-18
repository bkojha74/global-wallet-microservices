package auth

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateAndValidateToken(t *testing.T) {
	secret := "test-super-secret-key-12345"
	claims := Claims{
		Subject: "alice",
		Roles:   []string{RoleUser},
		Scopes:  []string{ScopeWalletRead, ScopeWalletTransfer},
	}

	token, err := GenerateToken(claims, secret)
	if err != nil {
		t.Fatalf("unexpected error generating token: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	parsed, err := ValidateToken(token, secret)
	if err != nil {
		t.Fatalf("unexpected error validating token: %v", err)
	}

	if parsed.Subject != "alice" {
		t.Fatalf("expected subject alice, got %s", parsed.Subject)
	}
	if !parsed.HasRole(RoleUser) {
		t.Fatal("expected role user")
	}
	if !parsed.HasScope(ScopeWalletTransfer) {
		t.Fatal("expected scope wallet:transfer")
	}
	if parsed.HasScope(ScopeClusterAdmin) {
		t.Fatal("user should not have cluster:admin scope")
	}
}

func TestAdminHasAllScopes(t *testing.T) {
	adminClaims := Claims{
		Subject: "ops-admin",
		Roles:   []string{RoleAdmin},
	}

	if !adminClaims.HasScope(ScopeClusterAdmin) {
		t.Fatal("admin should implicitly have cluster:admin scope")
	}
	if !adminClaims.HasScope(ScopeWalletTransfer) {
		t.Fatal("admin should implicitly have wallet:transfer scope")
	}
}

func TestValidateTokenRejectsTamperedSignature(t *testing.T) {
	secret := "test-secret"
	token, err := GenerateToken(Claims{Subject: "alice"}, secret)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	// Try validating with wrong secret
	_, err = ValidateToken(token, "wrong-secret")
	if err != ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}

	// Tamper with payload
	parts := strings.Split(token, ".")
	tampered := parts[0] + ".e30." + parts[2]
	_, err = ValidateToken(tampered, secret)
	if err != ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature for tampered token, got %v", err)
	}
}

func TestValidateTokenRejectsExpiredToken(t *testing.T) {
	secret := "test-secret"
	claims := Claims{
		Subject:   "alice",
		IssuedAt:  time.Now().Add(-2 * time.Hour).Unix(),
		ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(),
	}

	token, err := GenerateToken(claims, secret)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	_, err = ValidateToken(token, secret)
	if err != ErrTokenExpired {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		name        string
		header      string
		expected    string
		expectedErr error
	}{
		{
			name:        "valid bearer header",
			header:      "Bearer my-token-123",
			expected:    "my-token-123",
			expectedErr: nil,
		},
		{
			name:        "case insensitive bearer",
			header:      "bearer my-token-123",
			expected:    "my-token-123",
			expectedErr: nil,
		},
		{
			name:        "missing header",
			header:      "",
			expected:    "",
			expectedErr: ErrMissingAuthHeader,
		},
		{
			name:        "invalid format",
			header:      "Basic dXNlcjpwYXNz",
			expected:    "",
			expectedErr: ErrInvalidAuthFormat,
		},
		{
			name:        "empty token",
			header:      "Bearer   ",
			expected:    "",
			expectedErr: ErrInvalidAuthFormat,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			token, err := ExtractBearerToken(tc.header)
			if err != tc.expectedErr {
				t.Fatalf("expected error %v, got %v", tc.expectedErr, err)
			}
			if token != tc.expected {
				t.Fatalf("expected token %q, got %q", tc.expected, token)
			}
		})
	}
}
