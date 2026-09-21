package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wallet-system/pkg/auth"
)

func TestMapKeycloakClaims(t *testing.T) {
	kc := &KeycloakTokenClaims{
		Subject:           "user-uuid-12345",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Issuer:            "http://keycloak:8080/realms/wallet-realm",
		Audience:          "wallet-api",
		ExpiresAt:         time.Now().Add(1 * time.Hour).Unix(),
		IssuedAt:          time.Now().Unix(),
		TokenID:           "jti-9876",
		Scope:             "openid profile email wallet:read wallet:transfer",
		RealmAccess: KeycloakRealmAccess{
			Roles: []string{"user", "offline_access", "uma_authorization"},
		},
		ResourceAccess: map[string]KeycloakResourceAccess{
			"wallet-api": {
				Roles: []string{"wallet-manager", "wallet:transfer"},
			},
			"account": {
				Roles: []string{"view-profile"},
			},
		},
	}

	unified := MapKeycloakClaims(kc, "wallet-api")

	if unified.Subject != "alice" {
		t.Errorf("expected Subject 'alice', got %q", unified.Subject)
	}
	if unified.TokenID != "jti-9876" {
		t.Errorf("expected TokenID 'jti-9876', got %q", unified.TokenID)
	}
	if unified.Email != "alice@example.com" {
		t.Errorf("expected Email 'alice@example.com', got %q", unified.Email)
	}

	// Roles check: "user", "wallet-manager", "wallet:transfer" should be present; "offline_access" filtered
	roleMap := make(map[string]bool)
	for _, r := range unified.Roles {
		roleMap[r] = true
	}
	if !roleMap["user"] {
		t.Error("expected role 'user' to be present")
	}
	if !roleMap["wallet-manager"] {
		t.Error("expected client role 'wallet-manager' to be present")
	}
	if roleMap["offline_access"] {
		t.Error("expected noise role 'offline_access' to be filtered out")
	}

	// Scopes check: "wallet:read" and "wallet:transfer" should be present; "openid" filtered
	scopeMap := make(map[string]bool)
	for _, s := range unified.Scopes {
		scopeMap[s] = true
	}
	if !scopeMap["wallet:read"] || !scopeMap["wallet:transfer"] {
		t.Error("expected application scopes 'wallet:read' and 'wallet:transfer' to be present")
	}
	if scopeMap["openid"] || scopeMap["profile"] {
		t.Error("expected noise scopes 'openid' and 'profile' to be filtered out")
	}
}

func TestJWKSCache_VerifyToken(t *testing.T) {
	// 1. Generate RSA key pair
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	pubKey := &privKey.PublicKey

	// Encode modulus and exponent to base64url
	nB64 := base64.RawURLEncoding.EncodeToString(pubKey.N.Bytes())
	eBytes := []byte{1, 0, 1} // 65537
	eB64 := base64.RawURLEncoding.EncodeToString(eBytes)

	kid := "test-key-id-1"

	// 2. Start mock JWKS HTTP server
	jwksHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := JSONWebKeySet{
			Keys: []JSONWebKey{
				{
					Kty: "RSA",
					Kid: kid,
					Use: "sig",
					Alg: "RS256",
					N:   nB64,
					E:   eB64,
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	ts := httptest.NewServer(jwksHandler)
	defer ts.Close()

	// 3. Create JWKSCache targeting mock server
	cache := NewJWKSCache(ts.URL, 5*time.Minute)

	// 4. Forge a Keycloak RS256 token signed with private key
	kcClaims := KeycloakTokenClaims{
		Subject:           "user-kc-123",
		PreferredUsername: "bob",
		Issuer:            "http://mock-keycloak/realms/test",
		ExpiresAt:         time.Now().Add(10 * time.Minute).Unix(),
		IssuedAt:          time.Now().Unix(),
		TokenID:           "test-token-1",
		Scope:             "wallet:read",
		RealmAccess: KeycloakRealmAccess{
			Roles: []string{auth.RoleAdmin},
		},
	}

	h := tokenHeader{Alg: "RS256", Typ: "JWT", Kid: kid}
	hBytes, _ := json.Marshal(h)
	cBytes, _ := json.Marshal(kcClaims)
	hB64 := base64.RawURLEncoding.EncodeToString(hBytes)
	cB64 := base64.RawURLEncoding.EncodeToString(cBytes)
	signingInput := hB64 + "." + cB64

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign failed: %v", err)
	}
	tokenStr := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	// 5. Verify token with JWKSCache
	verifiedClaims, err := cache.VerifyToken(context.Background(), tokenStr)
	if err != nil {
		t.Fatalf("VerifyToken failed: %v", err)
	}

	if verifiedClaims.PreferredUsername != "bob" {
		t.Errorf("expected preferred_username 'bob', got %q", verifiedClaims.PreferredUsername)
	}
	if verifiedClaims.Subject != "user-kc-123" {
		t.Errorf("expected subject 'user-kc-123', got %q", verifiedClaims.Subject)
	}
}

func TestHybridProvider_TokenRouting(t *testing.T) {
	// 1. Setup local TokenEngine
	secret := "test-secret-key-1234567890"
	localEngine := NewHS256Engine(secret)

	localProvider := &LocalProvider{
		engine: localEngine,
	}

	// 2. Setup mock Keycloak RS256 provider
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	pubKey := &privKey.PublicKey
	nB64 := base64.RawURLEncoding.EncodeToString(pubKey.N.Bytes())
	eB64 := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
	kid := "kc-key-1"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JSONWebKeySet{
			Keys: []JSONWebKey{{Kty: "RSA", Kid: kid, Alg: "RS256", N: nB64, E: eB64}},
		})
	}))
	defer ts.Close()

	kcCache := NewJWKSCache(ts.URL, time.Hour)
	keycloakProvider := NewKeycloakProvider(nil, kcCache, "wallet-api", "http://keycloak:8080/realms/wallet-realm")

	// 3. Create HybridProvider
	hybrid := NewHybridProvider(localProvider, keycloakProvider)

	// Issue local HS256 token
	localToken, err := localEngine.IssueAccessToken(auth.Claims{
		Subject:   "local-alice",
		Roles:     []string{auth.RoleUser},
		Scopes:    []string{"wallet:read"},
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("issue local token failed: %v", err)
	}

	// Issue Keycloak RS256 token
	kcPayload := KeycloakTokenClaims{
		Subject:           "kc-user-uuid",
		PreferredUsername: "kc-bob",
		Issuer:            "http://keycloak:8080/realms/wallet-realm",
		ExpiresAt:         time.Now().Add(10 * time.Minute).Unix(),
		Scope:             "wallet:transfer",
		RealmAccess:       KeycloakRealmAccess{Roles: []string{auth.RoleAdmin}},
	}
	hBytes, _ := json.Marshal(tokenHeader{Alg: "RS256", Typ: "JWT", Kid: kid})
	cBytes, _ := json.Marshal(kcPayload)
	signingInput := base64.RawURLEncoding.EncodeToString(hBytes) + "." + base64.RawURLEncoding.EncodeToString(cBytes)
	digest := sha256.Sum256([]byte(signingInput))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, digest[:])
	kcToken := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	// 4. Validate local token through HybridProvider
	claims1, err := hybrid.ValidateToken(context.Background(), localToken)
	if err != nil {
		t.Fatalf("hybrid validation for local token failed: %v", err)
	}
	if claims1.Subject != "local-alice" {
		t.Errorf("expected subject 'local-alice', got %q", claims1.Subject)
	}

	// 5. Validate Keycloak token through HybridProvider
	claims2, err := hybrid.ValidateToken(context.Background(), kcToken)
	if err != nil {
		t.Fatalf("hybrid validation for Keycloak token failed: %v", err)
	}
	if claims2.Subject != "kc-bob" {
		t.Errorf("expected subject 'kc-bob', got %q", claims2.Subject)
	}
	if !hasScope(claims2.Scopes, "wallet:transfer") {
		t.Errorf("expected scope 'wallet:transfer' on kc token, got %v", claims2.Scopes)
	}
}
