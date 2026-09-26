package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestOIDCClientAndKeycloakProvider(t *testing.T) {
	// Setup Mock OIDC Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(OIDCDiscoveryResponse{
				Issuer:                r.Host,
				AuthorizationEndpoint: "http://" + r.Host + "/auth",
				TokenEndpoint:         "http://" + r.Host + "/token",
				JwksURI:               "http://" + r.Host + "/jwks",
				RevocationEndpoint:    "http://" + r.Host + "/revoke",
			})
		case "/token":
			if r.FormValue("username") == "bad_user" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(OIDCTokenResponse{Error: "invalid_grant", ErrorDescription: "Invalid credentials"})
				return
			}
			_ = json.NewEncoder(w).Encode(OIDCTokenResponse{
				AccessToken:      "mock_access_token",
				RefreshToken:     "mock_refresh_token",
				TokenType:        "Bearer",
				ExpiresIn:        3600,
				RefreshExpiresIn: 7200,
				Scope:            "openid profile",
			})
		case "/revoke":
			w.WriteHeader(http.StatusOK)
		case "/jwks":
			_, _ = w.Write([]byte(`{"keys":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()

	// 1. Initialize OIDC Client via discovery
	client, err := NewOIDCClient(ctx, server.URL, "test-client", "test-secret", "")
	if err != nil {
		t.Fatalf("failed to create OIDC client: %v", err)
	}

	if client.Issuer() == "" || client.JwksURI() == "" {
		t.Fatalf("expected non-empty issuer and jwksURI")
	}

	// 2. Authenticate Password
	tokResp, err := client.AuthenticatePassword(ctx, "kuser", "secret", []string{"openid"})
	if err != nil || tokResp.AccessToken != "mock_access_token" {
		t.Fatalf("failed password authentication: %v", err)
	}

	// 3. Bad User Authentication
	_, err = client.AuthenticatePassword(ctx, "bad_user", "secret", nil)
	if err == nil {
		t.Fatalf("expected error for bad_user")
	}

	// 4. Token Refresh
	refResp, err := client.RefreshToken(ctx, "mock_refresh_token")
	if err != nil || refResp.AccessToken != "mock_access_token" {
		t.Fatalf("failed token refresh: %v", err)
	}

	// 5. Explicit JWKS fallback when discovery fails
	badServerURL := "http://127.0.0.1:59999"
	clientFallback, err := NewOIDCClient(ctx, badServerURL, "test-client", "secret", "http://explicit.jwks")
	if err != nil || clientFallback.config.JwksURI != "http://explicit.jwks" {
		t.Fatalf("expected client creation with explicit JWKS fallback")
	}

	// 6. Test KeycloakProvider
	jwksCache := NewJWKSCache(server.URL+"/jwks", 1*time.Hour)
	kp := NewKeycloakProvider(client, jwksCache, "test-client", server.URL)
	if kp.Name() != "keycloak" {
		t.Fatalf("expected keycloak provider Name, got %s", kp.Name())
	}
}

func TestHybridProviderFallback(t *testing.T) {
	ctx := context.Background()

	userStore, _ := NewUserStore(nil)
	revStore, _ := NewRevocationStore(nil)
	eng := NewHS256Engine("test-secret-123")

	localProv := NewLocalProvider(userStore, eng, revStore)
	hybrid := NewHybridProvider(localProv, nil)

	if hybrid.Name() != "hybrid" {
		t.Fatalf("expected hybrid provider Name")
	}

	// Validate token error branch
	_, err := hybrid.ValidateToken(ctx, "invalid_jwt_token")
	if err == nil {
		t.Fatalf("expected error validating invalid token")
	}
}

func TestBuildTokenEngineExtra(t *testing.T) {
	os.Unsetenv("AUTH_PRIVATE_KEY_PATH")
	os.Unsetenv("AUTH_PUBLIC_KEY_PATH")
	t.Setenv("JWT_SECRET", "test-secret-123")

	eng, err := buildTokenEngine()
	if err != nil || eng == nil {
		t.Fatalf("expected valid HS256 engine")
	}

	// RSA key loading failure branch
	t.Setenv("AUTH_PRIVATE_KEY_PATH", "non-existent-priv.pem")
	t.Setenv("AUTH_PUBLIC_KEY_PATH", "non-existent-pub.pem")
	_, err = buildTokenEngine()
	if err == nil {
		t.Fatalf("expected error for non-existent RSA key paths")
	}
}

func TestParseAuthConfigAndSetupProvider(t *testing.T) {
	os.Unsetenv("AUTH_SERVICE_PORT")
	os.Unsetenv("MONGO_URI")
	os.Unsetenv("ENVIRONMENT")
	os.Unsetenv("AUTH_PROVIDER")
	os.Unsetenv("KEYCLOAK_URL")

	cfg := parseAuthConfig()
	if cfg.port != "50054" || cfg.environment != "local" || cfg.authProviderType != "local" {
		t.Fatalf("expected default config values")
	}

	t.Setenv("KEYCLOAK_URL", "http://keycloak:8080")
	cfgKeycloak := parseAuthConfig()
	if cfgKeycloak.authProviderType != "hybrid" {
		t.Fatalf("expected hybrid provider type when KEYCLOAK_URL set")
	}

	// Test setupIdentityProvider
	localProv := NewLocalProvider(nil, NewHS256Engine("secret"), nil)
	pLocal := setupIdentityProvider("local", localProv)
	if pLocal == nil || pLocal.Name() != "local" {
		t.Fatalf("expected local provider")
	}

	os.Unsetenv("KEYCLOAK_URL")
	pKeycloakFallback := setupIdentityProvider("keycloak", localProv)
	if pKeycloakFallback == nil || pKeycloakFallback.Name() != "local" {
		t.Fatalf("expected fallback to local provider")
	}

	// With explicit JWKS
	t.Setenv("KEYCLOAK_JWKS_URL", "http://127.0.0.1:59999/jwks")
	t.Setenv("KEYCLOAK_REALM", "my-realm")
	pHybrid := setupIdentityProvider("hybrid", localProv)
	if pHybrid == nil {
		t.Fatalf("expected non-nil hybrid provider with explicit JWKS")
	}
}
