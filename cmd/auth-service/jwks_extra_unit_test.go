package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wallet-system/pkg/auth"
)

func TestJWKSCache_RS256Verify_Full(t *testing.T) {
	// 1. Generate RSA key pair
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey failed: %v", err)
	}
	pubKey := &privKey.PublicKey

	// Base64url encode N and E
	nB64 := base64.RawURLEncoding.EncodeToString(pubKey.N.Bytes())
	eBytes := big.NewInt(int64(pubKey.E)).Bytes()
	eB64 := base64.RawURLEncoding.EncodeToString(eBytes)

	// 2. Mock JWKS HTTP Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JSONWebKeySet{
			Keys: []JSONWebKey{
				{
					Kty: "RSA",
					Kid: "key-rs256-1",
					Use: "sig",
					Alg: "RS256",
					N:   nB64,
					E:   eB64,
				},
				{
					Kty: "EC", // Should be skipped
					Kid: "key-ec-1",
				},
			},
		})
	}))
	defer server.Close()

	jwksCache := NewJWKSCache(server.URL, 5*time.Minute)

	// 3. Create & Sign RS256 JWT
	hdrJSON := `{"alg":"RS256","kid":"key-rs256-1"}`
	now := time.Now().UTC().Unix()
	claimsJSON := fmt.Sprintf(`{"iss":"http://localhost:8080","sub":"alice","exp":%d,"nbf":%d}`, now+3600, now-60)

	hdrB64 := base64.RawURLEncoding.EncodeToString([]byte(hdrJSON))
	claimsB64 := base64.RawURLEncoding.EncodeToString([]byte(claimsJSON))
	signingInput := hdrB64 + "." + claimsB64

	digest := sha256.Sum256([]byte(signingInput))
	sigBytes, err := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("rsa.SignPKCS1v15 failed: %v", err)
	}
	sigB64 := base64.RawURLEncoding.EncodeToString(sigBytes)

	tokenStr := signingInput + "." + sigB64

	// 4. Verify token
	ctx := context.Background()
	kcClaims, err := jwksCache.VerifyToken(ctx, tokenStr)
	if err != nil || kcClaims.Subject != "alice" {
		t.Fatalf("VerifyToken RS256 failed: %v, claims=%v", err, kcClaims)
	}

	// 5. Test GetKey with empty kid (returns single key)
	singleKeyCache := &JWKSCache{
		keys:        map[string]*rsa.PublicKey{"": pubKey},
		keysList:    []*rsa.PublicKey{pubKey},
		lastFetched: time.Now(),
		ttl:         5 * time.Minute,
	}
	k, err := singleKeyCache.GetKey(ctx, "")
	if err != nil || k != pubKey {
		t.Errorf("Expected fallback single key for empty kid")
	}

	// 6. Test Error Branches
	// Expired token
	expClaimsJSON := fmt.Sprintf(`{"iss":"http://localhost:8080","sub":"alice","exp":%d}`, now-3600)
	expClaimsB64 := base64.RawURLEncoding.EncodeToString([]byte(expClaimsJSON))
	expInput := hdrB64 + "." + expClaimsB64
	expDigest := sha256.Sum256([]byte(expInput))
	expSig, _ := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, expDigest[:])
	expToken := expInput + "." + base64.RawURLEncoding.EncodeToString(expSig)

	if _, err := jwksCache.VerifyToken(ctx, expToken); !errors.Is(err, auth.ErrTokenExpired) {
		t.Errorf("Expected ErrTokenExpired, got %v", err)
	}

	// Future nbf token
	nbfClaimsJSON := fmt.Sprintf(`{"iss":"http://localhost:8080","sub":"alice","exp":%d,"nbf":%d}`, now+3600, now+1000)
	nbfClaimsB64 := base64.RawURLEncoding.EncodeToString([]byte(nbfClaimsJSON))
	nbfInput := hdrB64 + "." + nbfClaimsB64
	nbfDigest := sha256.Sum256([]byte(nbfInput))
	nbfSig, _ := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, nbfDigest[:])
	nbfToken := nbfInput + "." + base64.RawURLEncoding.EncodeToString(nbfSig)

	if _, err := jwksCache.VerifyToken(ctx, nbfToken); err == nil || !strings.Contains(err.Error(), "nbf") {
		t.Errorf("Expected nbf error, got %v", err)
	}

	// parseRSAPublicKey invalid
	if _, err := parseRSAPublicKey("!!!invalid!!!", eB64); err == nil {
		t.Errorf("Expected decode modulus error")
	}
	if _, err := parseRSAPublicKey(nB64, "!!!invalid!!!"); err == nil {
		t.Errorf("Expected decode exponent error")
	}
	if _, err := parseRSAPublicKey(nB64, base64.RawURLEncoding.EncodeToString([]byte{0})); err == nil {
		t.Errorf("Expected zero exponent error")
	}
}
