package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"wallet-system/pkg/auth"
)

// JSONWebKey represents an RFC 7517 cryptographic key.
type JSONWebKey struct {
	Kty string `json:"kty"` // Key Type, e.g. "RSA"
	Kid string `json:"kid"` // Key ID
	Use string `json:"use"` // Key use, e.g. "sig"
	Alg string `json:"alg"` // Algorithm, e.g. "RS256"
	N   string `json:"n"`   // RSA Modulus (base64url)
	E   string `json:"e"`   // RSA Exponent (base64url)
}

// JSONWebKeySet represents a set of JSONWebKeys from an IdP certs endpoint.
type JSONWebKeySet struct {
	Keys []JSONWebKey `json:"keys"`
}

// JWKSCache provides a thread-safe, TTL-cached in-memory store for remote JWKS keys.
type JWKSCache struct {
	mu           sync.RWMutex
	jwksURL      string
	httpClient   *http.Client
	keys         map[string]*rsa.PublicKey
	keysList     []*rsa.PublicKey // fallback if kid is missing
	lastFetched  time.Time
	ttl          time.Duration
	minRefresh   time.Duration // rate limit rapid refreshes on cache misses
	lastAttempt  time.Time
}

// NewJWKSCache creates a JWKS cache targeting an external OIDC JWKS endpoint.
func NewJWKSCache(jwksURL string, ttl time.Duration) *JWKSCache {
	if ttl <= 0 {
		ttl = 1 * time.Hour
	}
	return &JWKSCache{
		jwksURL:    jwksURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		keys:       make(map[string]*rsa.PublicKey),
		ttl:        ttl,
		minRefresh: 5 * time.Second,
	}
}

// Refresh fetches and parses keys from the remote JWKS URL.
func (c *JWKSCache) Refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Rate limit check: avoid hammering Keycloak on invalid tokens
	if time.Since(c.lastAttempt) < c.minRefresh && len(c.keys) > 0 {
		return nil
	}
	c.lastAttempt = time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("jwks: request create error: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jwks: fetch failed from %s: %w", c.jwksURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jwks: unexpected status %d from %s: %s", resp.StatusCode, c.jwksURL, string(body))
	}

	var jwks JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("jwks: decode error: %w", err)
	}

	newKeys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	var newList []*rsa.PublicKey

	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue // currently support RSA signatures
		}
		pubKey, err := parseRSAPublicKey(k.N, k.E)
		if err != nil {
			log.Printf("[JWKS] Warning: failed to parse RSA key kid=%q: %v", k.Kid, err)
			continue
		}
		if k.Kid != "" {
			newKeys[k.Kid] = pubKey
		}
		newList = append(newList, pubKey)
	}

	if len(newKeys) == 0 && len(newList) == 0 {
		return fmt.Errorf("jwks: no valid RSA public keys found at %s", c.jwksURL)
	}

	c.keys = newKeys
	c.keysList = newList
	c.lastFetched = time.Now()
	log.Printf("[JWKS] Successfully refreshed %d public key(s) from %s", len(c.keys), c.jwksURL)
	return nil
}

// GetKey returns an RSA public key matching the given kid.
// If kid is not found or cache is expired, it attempts a refresh.
func (c *JWKSCache) GetKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	expired := time.Since(c.lastFetched) > c.ttl
	key, ok := c.keys[kid]
	c.mu.RUnlock()

	if ok && !expired {
		return key, nil
	}

	// Key not found or cache expired: perform refresh
	if err := c.Refresh(ctx); err != nil {
		// If refresh fails but we still had the key, return it as fallback
		if ok {
			return key, nil
		}
		return nil, fmt.Errorf("jwks: key %q not found and refresh failed: %w", kid, err)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	if key, ok = c.keys[kid]; ok {
		return key, nil
	}

	// If kid was empty and exactly one key is available, use it
	if kid == "" && len(c.keysList) == 1 {
		return c.keysList[0], nil
	}

	return nil, fmt.Errorf("jwks: key with kid=%q not found in JWKS", kid)
}

// VerifyToken validates the signature and standard timestamps of an RS256 JWT from Keycloak.
func (c *JWKSCache) VerifyToken(ctx context.Context, tokenStr string) (*KeycloakTokenClaims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, auth.ErrInvalidToken
	}
	hB64, cB64, sigB64 := parts[0], parts[1], parts[2]

	// Decode Header
	hBytes, err := decodeBase64URL(hB64)
	if err != nil {
		return nil, auth.ErrInvalidToken
	}
	var hdr tokenHeader
	if err := json.Unmarshal(hBytes, &hdr); err != nil {
		return nil, auth.ErrInvalidToken
	}
	if hdr.Alg != "RS256" {
		return nil, fmt.Errorf("jwks: unsupported token algorithm %q (expected RS256)", hdr.Alg)
	}

	// Retrieve Public Key from JWKS
	pubKey, err := c.GetKey(ctx, hdr.Kid)
	if err != nil {
		return nil, fmt.Errorf("jwks: cannot obtain public key for kid=%q: %w", hdr.Kid, err)
	}

	// Verify Signature
	signingInput := hB64 + "." + cB64
	sig, err := decodeBase64URL(sigB64)
	if err != nil {
		return nil, auth.ErrInvalidSignature
	}

	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, digest[:], sig); err != nil {
		return nil, auth.ErrInvalidSignature
	}

	// Decode Claims
	cBytes, err := decodeBase64URL(cB64)
	if err != nil {
		return nil, auth.ErrInvalidClaims
	}
	var claims KeycloakTokenClaims
	if err := json.Unmarshal(cBytes, &claims); err != nil {
		return nil, auth.ErrInvalidClaims
	}

	now := time.Now().UTC().Unix()
	if claims.ExpiresAt > 0 && now > claims.ExpiresAt {
		return nil, auth.ErrTokenExpired
	}
	if claims.NotBefore > 0 && now < claims.NotBefore {
		return nil, errors.New("token not yet valid (nbf)")
	}

	return &claims, nil
}

// parseRSAPublicKey constructs an *rsa.PublicKey from base64url-encoded modulus (N) and exponent (E).
func parseRSAPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := decodeBase64URL(nB64)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	eBytes, err := decodeBase64URL(eB64)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}

	var eInt int
	for _, b := range eBytes {
		eInt = (eInt << 8) | int(b)
	}
	if eInt == 0 {
		return nil, errors.New("invalid RSA exponent (zero)")
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: eInt,
	}, nil
}

// decodeBase64URL decodes base64 strings with or without padding.
func decodeBase64URL(s string) ([]byte, error) {
	// First try RawURLEncoding (no padding)
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	// Fall back to standard URLEncoding (with padding)
	return base64.URLEncoding.DecodeString(s)
}
