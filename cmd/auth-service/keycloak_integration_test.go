package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	authv1 "wallet-system/proto/auth"
)

// TestKeycloakIntegration runs the end-to-end verification suite against a live Keycloak container.
// If Keycloak is not currently running (e.g. CI environments without Docker), it skips gracefully.
func TestKeycloakIntegration(t *testing.T) {
	keycloakHost := os.Getenv("KEYCLOAK_TEST_HOST")
	if keycloakHost == "" {
		keycloakHost = "http://localhost:8085"
	}

	realm := "wallet-realm"
	clientID := "wallet-api"
	clientSecret := "wallet-client-secret-12345"
	issuerURL := fmt.Sprintf("%s/realms/%s", strings.TrimRight(keycloakHost, "/"), realm)

	// Check connectivity
	resp, err := http.Get(issuerURL + "/.well-known/openid-configuration")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skipf("Keycloak is not running at %s (%v), skipping live integration tests. Run 'docker compose up -d keycloak' to execute.", keycloakHost, err)
		return
	}
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── LEVEL 1: AC-02 - OIDC Discovery & JWKS Verification ──────────────────
	t.Run("Level1_OIDC_Discovery", func(t *testing.T) {
		oidcClient, err := NewOIDCClient(ctx, issuerURL, clientID, clientSecret, "")
		if err != nil {
			t.Fatalf("OIDC discovery failed: %v", err)
		}
		if oidcClient.JwksURI() == "" {
			t.Error("expected valid jwks_uri in discovery document")
		}

		jwksCache := NewJWKSCache(oidcClient.JwksURI(), time.Hour)
		if err := jwksCache.Refresh(ctx); err != nil {
			t.Fatalf("JWKS refresh failed from %s: %v", oidcClient.JwksURI(), err)
		}
		if len(jwksCache.keys) == 0 {
			t.Fatal("expected at least 1 RSA public key in Keycloak JWKS")
		}
		t.Logf("Successfully verified Keycloak JWKS: %d public key(s) discovered", len(jwksCache.keys))
	})

	// ── LEVEL 2: AC-01 & AC-02 - Direct Access Grant (User Login) ───────────
	var aliceAccessToken string
	var adminAccessToken string

	t.Run("Level2_DirectAccessGrant_Alice", func(t *testing.T) {
		tokenURL := fmt.Sprintf("%s/protocol/openid-connect/token", issuerURL)
		data := url.Values{}
		data.Set("grant_type", "password")
		data.Set("client_id", clientID)
		data.Set("client_secret", clientSecret)
		data.Set("username", "alice")
		data.Set("password", "alice123")
		data.Set("scope", "openid wallet:read wallet:transfer")

		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		client := &http.Client{Timeout: 10 * time.Second}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("token request failed: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			var errMap map[string]interface{}
			_ = json.NewDecoder(res.Body).Decode(&errMap)
			t.Fatalf("Keycloak login for Alice failed with status %d: %v", res.StatusCode, errMap)
		}

		var tokenResp OIDCTokenResponse
		if err := json.NewDecoder(res.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}

		if tokenResp.AccessToken == "" {
			t.Fatal("empty access_token returned by Keycloak")
		}
		aliceAccessToken = tokenResp.AccessToken
		t.Log("Successfully obtained Keycloak access token for user 'alice'")
	})

	t.Run("Level2_DirectAccessGrant_Admin", func(t *testing.T) {
		tokenURL := fmt.Sprintf("%s/protocol/openid-connect/token", issuerURL)
		data := url.Values{}
		data.Set("grant_type", "password")
		data.Set("client_id", clientID)
		data.Set("client_secret", clientSecret)
		data.Set("username", "admin")
		data.Set("password", "admin123")
		data.Set("scope", "openid cluster:admin ledger:audit wallet:read")

		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		client := &http.Client{Timeout: 10 * time.Second}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("token request failed: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("Keycloak login for Admin failed with status %d", res.StatusCode)
		}

		var tokenResp OIDCTokenResponse
		_ = json.NewDecoder(res.Body).Decode(&tokenResp)
		adminAccessToken = tokenResp.AccessToken
		t.Log("Successfully obtained Keycloak access token for user 'admin'")
	})

	// ── LEVEL 3: AC-03 & AC-05 - Auth Service JWKS Validation & Normalization ─
	t.Run("Level3_AuthService_Validation_Alice", func(t *testing.T) {
		if aliceAccessToken == "" {
			t.Skip("aliceAccessToken not available")
		}

		oidcClient, _ := NewOIDCClient(ctx, issuerURL, clientID, clientSecret, "")
		jwksCache := NewJWKSCache(oidcClient.JwksURI(), time.Hour)
		kcProvider := NewKeycloakProvider(oidcClient, jwksCache, clientID, issuerURL)

		parts := strings.Split(aliceAccessToken, ".")
		if len(parts) == 3 {
			b, _ := decodeBase64URL(parts[1])
			t.Logf("RAW ALICE TOKEN PAYLOAD: %s", string(b))
		}

		claims, err := kcProvider.ValidateToken(ctx, aliceAccessToken)
		if err != nil {
			t.Fatalf("KeycloakProvider.ValidateToken failed: %v", err)
		}

		if claims.Subject != "alice" {
			t.Errorf("expected Subject 'alice', got %q", claims.Subject)
		}

		// Verify roles
		hasUserRole := false
		for _, r := range claims.Roles {
			if strings.EqualFold(r, "user") {
				hasUserRole = true
				break
			}
		}
		if !hasUserRole {
			t.Errorf("expected role 'user' in claims, got %v", claims.Roles)
		}

		// Verify scopes
		if !hasScope(claims.Scopes, "wallet:read") {
			t.Errorf("expected scope 'wallet:read' in normalized scopes, got %v", claims.Scopes)
		}
		t.Logf("Alice claims successfully normalized: sub=%s, roles=%v, scopes=%v", claims.Subject, claims.Roles, claims.Scopes)
	})

	// ── LEVEL 4: AC-07 - Fine-grained Authorization Decisions ────────────────
	t.Run("Level4_Authorization_Decisions", func(t *testing.T) {
		oidcClient, _ := NewOIDCClient(ctx, issuerURL, clientID, clientSecret, "")
		jwksCache := NewJWKSCache(oidcClient.JwksURI(), time.Hour)
		kcProvider := NewKeycloakProvider(oidcClient, jwksCache, clientID, issuerURL)

		// Create in-memory dummy user store
		server := &authServer{
			provider: kcProvider,
			store:    &UserStore{}, // empty
		}

		// Test Alice attempting cluster admin -> DENY
		aliceClaims, err := kcProvider.ValidateToken(ctx, aliceAccessToken)
		if err != nil {
			t.Fatalf("validate alice token: %v", err)
		}

		authResp, err := server.Authorize(ctx, &authv1.AuthorizeRequest{
			Subject:  aliceClaims.Subject,
			Roles:    aliceClaims.Roles,
			Scopes:   aliceClaims.Scopes,
			Resource: "cluster:failover",
			Action:   "execute",
		})
		if err != nil {
			t.Fatalf("Authorize error: %v", err)
		}
		if authResp.Allowed {
			t.Error("expected Alice to be DENIED for cluster:failover")
		} else {
			t.Logf("AC-07 verified: Alice correctly denied cluster failover (%s)", authResp.Reason)
		}

		// Test Admin attempting cluster admin -> ALLOW
		adminClaims, err := kcProvider.ValidateToken(ctx, adminAccessToken)
		if err != nil {
			t.Fatalf("validate admin token: %v", err)
		}

		adminAuthResp, err := server.Authorize(ctx, &authv1.AuthorizeRequest{
			Subject:  adminClaims.Subject,
			Roles:    adminClaims.Roles,
			Scopes:   adminClaims.Scopes,
			Resource: "cluster:failover",
			Action:   "execute",
		})
		if err != nil {
			t.Fatalf("Authorize error: %v", err)
		}
		if !adminAuthResp.Allowed {
			t.Errorf("expected Admin to be ALLOWED for cluster:failover, got: %s", adminAuthResp.Reason)
		} else {
			t.Logf("AC-07 verified: Admin allowed for cluster failover (%s)", adminAuthResp.Reason)
		}
	})

	// ── LEVEL 5: AC-05 - Negative Security Tests (Tampering & Expiry) ─────────
	t.Run("Level5_Security_Tampered_Token", func(t *testing.T) {
		if aliceAccessToken == "" {
			t.Skip("aliceAccessToken not available")
		}

		oidcClient, _ := NewOIDCClient(ctx, issuerURL, clientID, clientSecret, "")
		jwksCache := NewJWKSCache(oidcClient.JwksURI(), time.Hour)
		kcProvider := NewKeycloakProvider(oidcClient, jwksCache, clientID, issuerURL)

		// Tamper with payload
		parts := strings.Split(aliceAccessToken, ".")
		if len(parts) == 3 {
			tamperedPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"mallory","preferred_username":"mallory"}`))
			tamperedToken := parts[0] + "." + tamperedPayload + "." + parts[2]

			_, err := kcProvider.ValidateToken(ctx, tamperedToken)
			if err == nil {
				t.Error("expected tampered token to fail signature validation, but it succeeded")
			} else {
				t.Logf("AC-05 verified: Tampered token correctly rejected: %v", err)
			}
		}
	})
}
