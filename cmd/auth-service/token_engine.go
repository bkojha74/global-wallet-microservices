package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"wallet-system/pkg/auth"
)

// Algorithm constants.
const (
	AlgHS256 = "HS256"
	AlgRS256 = "RS256"
)

// tokenHeader is the JWT header.
type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid,omitempty"` // key ID for RS256 rotation support
}

// TokenEngine handles JWT signing and verification.
// It supports both HS256 (symmetric, backward-compatible) and RS256 (asymmetric, preferred).
// When an RSA private key is configured, RS256 is used; otherwise it falls back to HS256.
type TokenEngine struct {
	hmacSecret string
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	algorithm  string
	keyID      string // optional "kid" for key rotation
}

// NewHS256Engine creates a token engine that uses HMAC-SHA256.
// This maintains backward compatibility with the existing pkg/auth JWT logic.
func NewHS256Engine(secret string) *TokenEngine {
	return &TokenEngine{
		hmacSecret: secret,
		algorithm:  AlgHS256,
	}
}

// NewRS256Engine creates a token engine that uses RSA-SHA256 (asymmetric).
// The auth-service holds the private key; verifiers only need the public key.
func NewRS256Engine(privateKey *rsa.PrivateKey, publicKey *rsa.PublicKey, keyID string) *TokenEngine {
	return &TokenEngine{
		privateKey: privateKey,
		publicKey:  publicKey,
		algorithm:  AlgRS256,
		keyID:      keyID,
	}
}

// IssueAccessToken creates a signed JWT access token for the given claims.
func (e *TokenEngine) IssueAccessToken(claims auth.Claims) (string, error) {
	now := time.Now().UTC()
	if claims.IssuedAt == 0 {
		claims.IssuedAt = now.Unix()
	}
	if claims.ExpiresAt == 0 {
		claims.ExpiresAt = now.Add(15 * time.Minute).Unix() // short-lived
	}
	if claims.Issuer == "" {
		claims.Issuer = auth.DefaultIssuer
	}
	if claims.Audience == "" {
		claims.Audience = auth.DefaultAudience
	}
	return e.sign(claims)
}

// IssueRefreshToken creates an opaque random refresh token string (not a JWT).
// The token is stored in MongoDB with a TTL and linked to the subject.
func (e *TokenEngine) IssueRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("token_engine: failed to generate refresh token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidateAccessToken verifies the token's signature, structure, and expiry.
func (e *TokenEngine) ValidateAccessToken(tokenStr string) (*auth.Claims, error) {
	switch e.algorithm {
	case AlgRS256:
		return e.verifyRS256(tokenStr)
	default:
		return auth.ValidateToken(tokenStr, e.hmacSecret)
	}
}

// sign produces a signed JWT string using the configured algorithm.
func (e *TokenEngine) sign(claims auth.Claims) (string, error) {
	switch e.algorithm {
	case AlgRS256:
		return e.signRS256(claims)
	default:
		return auth.GenerateToken(claims, e.hmacSecret)
	}
}

// signRS256 produces an RS256-signed JWT.
func (e *TokenEngine) signRS256(claims auth.Claims) (string, error) {
	if e.privateKey == nil {
		return "", errors.New("token_engine: RS256 private key not configured")
	}
	h := tokenHeader{Alg: AlgRS256, Typ: "JWT", Kid: e.keyID}
	hBytes, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("token_engine: header encode: %w", err)
	}
	cBytes, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("token_engine: claims encode: %w", err)
	}
	hB64 := base64.RawURLEncoding.EncodeToString(hBytes)
	cB64 := base64.RawURLEncoding.EncodeToString(cBytes)
	signingInput := hB64 + "." + cB64

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, e.privateKey, 0, digest[:])
	if err != nil {
		return "", fmt.Errorf("token_engine: RS256 sign: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// verifyRS256 validates an RS256-signed JWT.
func (e *TokenEngine) verifyRS256(tokenStr string) (*auth.Claims, error) {
	if e.publicKey == nil {
		return nil, errors.New("token_engine: RS256 public key not configured")
	}
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, auth.ErrInvalidToken
	}
	hB64, cB64, sigB64 := parts[0], parts[1], parts[2]
	signingInput := hB64 + "." + cB64

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, auth.ErrInvalidSignature
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(e.publicKey, 0, digest[:], sig); err != nil {
		return nil, auth.ErrInvalidSignature
	}

	cBytes, err := base64.RawURLEncoding.DecodeString(cB64)
	if err != nil {
		return nil, auth.ErrInvalidClaims
	}
	var claims auth.Claims
	if err := json.Unmarshal(cBytes, &claims); err != nil {
		return nil, auth.ErrInvalidClaims
	}
	if time.Now().UTC().Unix() > claims.ExpiresAt {
		return nil, auth.ErrTokenExpired
	}
	return &claims, nil
}

// TokenFingerprint returns an HMAC-SHA256 hex fingerprint of a token string.
// Used as a cache key so raw tokens are never stored in memory as map keys.
func TokenFingerprint(token, cacheSecret string) string {
	mac := hmac.New(sha256.New, []byte(cacheSecret))
	mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
