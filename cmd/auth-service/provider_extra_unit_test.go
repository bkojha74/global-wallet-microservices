package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wallet-system/pkg/auth"
)

func TestKeycloakAndHybridProvider_Unit(t *testing.T) {
	// 1. Mock Keycloak OIDC HTTP Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"issuer":                 "http://localhost:8080/realms/wallet",
				"authorization_endpoint": "http://localhost:8080/realms/wallet/auth",
				"token_endpoint":         "http://localhost:8080/realms/wallet/token",
				"jwks_uri":               "http://localhost:8080/realms/wallet/jwks",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	oidcClient, _ := NewOIDCClient(ctx, server.URL, "client-123", "secret-456", "")

	// Create test token engine & local provider
	engine := NewHS256Engine("test-secret-key-32-bytes-minimum")
	store, _ := NewUserStore(nil)
	revStore, _ := NewRevocationStore(nil)
	localProv := NewLocalProvider(store, engine, revStore)

	// Create KeycloakProvider
	jwksCache := NewJWKSCache(server.URL+"/jwks", 5*time.Minute)
	kcProv := NewKeycloakProvider(oidcClient, jwksCache, "client-123", server.URL)

	if kcProv.Name() != "keycloak" {
		t.Errorf("Expected name 'keycloak', got %s", kcProv.Name())
	}

	// Test KeycloakProvider nil oidcClient
	nilKcProv := NewKeycloakProvider(nil, jwksCache, "client-123", "")
	if _, _, err := nilKcProv.Authenticate(context.Background(), "user", "pass", nil); err == nil {
		t.Errorf("Expected error when oidcClient is nil in Authenticate")
	}
	if _, _, err := nilKcProv.RefreshToken(context.Background(), "ref-tok"); err == nil {
		t.Errorf("Expected error when oidcClient is nil in RefreshToken")
	}

	// Test HybridProvider
	hybrid := NewHybridProvider(localProv, kcProv)
	if hybrid.Name() != "hybrid" {
		t.Errorf("Expected name 'hybrid', got %s", hybrid.Name())
	}

	// Issue valid local token
	localToken, err := engine.IssueAccessToken(auth.Claims{
		Subject:   "alice",
		Roles:     []string{auth.RoleUser},
		Scopes:    []string{"wallet:read"},
		Issuer:    auth.DefaultIssuer,
		Audience:  auth.DefaultAudience,
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("Failed to issue local token: %v", err)
	}

	// Validate local token through HybridProvider
	uClaims, err := hybrid.ValidateToken(ctx, localToken)
	if err != nil || uClaims.Subject != "alice" {
		t.Errorf("Expected valid claims for local token in HybridProvider, got %v, err=%v", uClaims, err)
	}

	// Test HybridProvider with external issuer matching token (will attempt external, fail, fallback to local)
	hdrJSON := `{"alg":"HS256","kid":"key-1"}`
	payloadJSON := fmt.Sprintf(`{"iss":"%s","sub":"alice","exp":%d}`, server.URL, time.Now().Add(1*time.Hour).Unix())
	hdrB64 := base64.RawURLEncoding.EncodeToString([]byte(hdrJSON))
	payloadB64 := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	mockExtToken := hdrB64 + "." + payloadB64 + ".mock-signature"

	// ValidateToken should attempt Keycloak first (fails JWKS verify) then fallback to local (fails HS256 verify) -> returns error
	_, err = hybrid.ValidateToken(ctx, mockExtToken)
	if err == nil {
		t.Errorf("Expected error validating mock external token with invalid signature")
	}

	// Test HybridProvider Authenticate fallback when local fails
	_, _, err = hybrid.Authenticate(context.Background(), "invalid", "invalid", nil)
	if err == nil {
		t.Errorf("Expected error on invalid user authentication in HybridProvider")
	}

	// Test HybridProvider RefreshToken fallback
	_, _, err = hybrid.RefreshToken(context.Background(), "invalid-refresh")
	if err == nil {
		t.Errorf("Expected error on invalid refresh token in HybridProvider")
	}

	// Test peekTokenMetadata helper function
	iss, kid := peekTokenMetadata("not.a.validjwt")
	if iss != "" || kid != "" {
		t.Errorf("Expected empty iss and kid for invalid JWT, got iss=%q kid=%q", iss, kid)
	}

	// Test peekTokenMetadata with valid base64url encoded header & claims
	issExt, kidExt := peekTokenMetadata(mockExtToken)
	if issExt != server.URL || kidExt != "key-1" {
		t.Errorf("Expected iss=%q kid=%q, got iss=%q kid=%q", server.URL, "key-1", issExt, kidExt)
	}
}
