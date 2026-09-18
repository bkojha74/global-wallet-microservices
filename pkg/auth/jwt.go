package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidToken       = errors.New("invalid token format")
	ErrInvalidSignature   = errors.New("invalid token signature")
	ErrTokenExpired       = errors.New("token has expired")
	ErrInvalidHeader      = errors.New("invalid token header")
	ErrInvalidClaims      = errors.New("invalid token claims")
	ErrMissingAuthHeader  = errors.New("authorization header required")
	ErrInvalidAuthFormat  = errors.New("authorization format must be Bearer <token>")
	ErrInsufficientPerms  = errors.New("insufficient permissions for resource")
	ErrIDORViolation      = errors.New("subject is not authorized to act on this wallet")
)

const (
	RoleUser  = "user"
	RoleAdmin = "admin"

	ScopeWalletRead     = "wallet:read"
	ScopeWalletTransfer = "wallet:transfer"
	ScopeClusterAdmin   = "cluster:admin"
	ScopeLedgerAudit    = "ledger:audit"

	DefaultIssuer   = "wallet-system"
	DefaultAudience = "wallet-api"
)

// Claims represents the standard and custom JWT claims.
type Claims struct {
	Subject   string   `json:"sub"`
	Roles     []string `json:"roles,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
	Issuer    string   `json:"iss,omitempty"`
	Audience  string   `json:"aud,omitempty"`
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
}

// HasRole returns true if the claims contain the given role.
func (c *Claims) HasRole(role string) bool {
	for _, r := range c.Roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

// HasScope returns true if the claims contain the given scope or if the user is an admin.
func (c *Claims) HasScope(scope string) bool {
	if c.HasRole(RoleAdmin) {
		return true
	}
	for _, s := range c.Scopes {
		if strings.EqualFold(s, scope) {
			return true
		}
	}
	return false
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// GenerateToken creates an HMAC-SHA256 signed JWT string for the given claims and secret.
func GenerateToken(claims Claims, secret string) (string, error) {
	if secret == "" {
		return "", errors.New("secret cannot be empty")
	}

	now := time.Now().UTC()
	if claims.IssuedAt == 0 {
		claims.IssuedAt = now.Unix()
	}
	if claims.ExpiresAt == 0 {
		// Default 1 hour validity
		claims.ExpiresAt = now.Add(1 * time.Hour).Unix()
	}
	if claims.Issuer == "" {
		claims.Issuer = DefaultIssuer
	}
	if claims.Audience == "" {
		claims.Audience = DefaultAudience
	}

	h := header{
		Alg: "HS256",
		Typ: "JWT",
	}

	hBytes, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("failed to encode header: %w", err)
	}

	cBytes, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("failed to encode claims: %w", err)
	}

	hB64 := base64.RawURLEncoding.EncodeToString(hBytes)
	cB64 := base64.RawURLEncoding.EncodeToString(cBytes)
	signingInput := hB64 + "." + cB64

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	sig := mac.Sum(nil)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return signingInput + "." + sigB64, nil
}

// ValidateToken verifies the HMAC-SHA256 signature and expiration of a JWT token.
func ValidateToken(tokenStr string, secret string) (*Claims, error) {
	if secret == "" {
		return nil, errors.New("secret cannot be empty")
	}

	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, ErrInvalidToken
	}

	hB64, cB64, sigB64 := parts[0], parts[1], parts[2]

	signingInput := hB64 + "." + cB64
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	expectedSig := mac.Sum(nil)

	actualSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, ErrInvalidSignature
	}

	if !hmac.Equal(expectedSig, actualSig) {
		return nil, ErrInvalidSignature
	}

	hBytes, err := base64.RawURLEncoding.DecodeString(hB64)
	if err != nil {
		return nil, ErrInvalidHeader
	}
	var h header
	if err := json.Unmarshal(hBytes, &h); err != nil || h.Alg != "HS256" || h.Typ != "JWT" {
		return nil, ErrInvalidHeader
	}

	cBytes, err := base64.RawURLEncoding.DecodeString(cB64)
	if err != nil {
		return nil, ErrInvalidClaims
	}
	var claims Claims
	if err := json.Unmarshal(cBytes, &claims); err != nil {
		return nil, ErrInvalidClaims
	}

	now := time.Now().UTC().Unix()
	if claims.ExpiresAt < now {
		return nil, ErrTokenExpired
	}

	return &claims, nil
}

// ExtractBearerToken parses the Bearer token from an Authorization header value.
func ExtractBearerToken(authHeader string) (string, error) {
	if authHeader == "" {
		return "", ErrMissingAuthHeader
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", ErrInvalidAuthFormat
	}

	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", ErrInvalidAuthFormat
	}

	return token, nil
}
