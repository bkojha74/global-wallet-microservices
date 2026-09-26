package main

import (
	"context"
	"testing"
	"time"
)

func TestRunAuthServer_CancelledContext(t *testing.T) {
	// 1. Connection failure path
	t.Setenv("TEST_MOCK_DB", "false")
	t.Setenv("MONGO_URI", "mongodb://127.0.0.1:27019/?connectTimeoutMS=100")
	t.Setenv("AUTH_SERVICE_PORT", "59091")
	t.Setenv("ENVIRONMENT", "test")
	t.Setenv("AUTH_PROVIDER", "local")

	ctx1, cancel1 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel1()
	_ = runAuthServer(ctx1)

	// 2. Mock DB clean startup and shutdown path
	t.Setenv("TEST_MOCK_DB", "true")
	t.Setenv("AUTH_SERVICE_PORT", "59092")

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()

	err := runAuthServer(ctx2)
	if err != nil {
		t.Errorf("expected clean shutdown, got error: %v", err)
	}
}

func TestParseAuthConfig_DefaultsAndEnv(t *testing.T) {
	t.Setenv("AUTH_SERVICE_PORT", "")
	t.Setenv("MONGO_URI", "")
	t.Setenv("ENVIRONMENT", "")
	t.Setenv("AUTH_PROVIDER", "")
	t.Setenv("KEYCLOAK_URL", "")

	cfg := parseAuthConfig()
	if cfg.port != "50054" {
		t.Errorf("expected default port 50054, got %s", cfg.port)
	}
	if cfg.authProviderType != "local" {
		t.Errorf("expected default authProviderType local, got %s", cfg.authProviderType)
	}

	t.Setenv("KEYCLOAK_URL", "http://localhost:8080")
	cfg2 := parseAuthConfig()
	if cfg2.authProviderType != "hybrid" {
		t.Errorf("expected authProviderType hybrid when KEYCLOAK_URL is set, got %s", cfg2.authProviderType)
	}
}

func TestSetupIdentityProviderBranches(t *testing.T) {
	local := &LocalProvider{}
	t.Setenv("KEYCLOAK_URL", "http://localhost:8080")
	t.Setenv("KEYCLOAK_REALM", "my-realm")
	t.Setenv("KEYCLOAK_CLIENT_ID", "my-client")

	// 1. mode="keycloak"
	pKC := setupIdentityProvider("keycloak", local)
	if pKC == nil {
		t.Fatal("expected non-nil provider for mode=keycloak")
	}

	// 2. mode="hybrid"
	pHybrid := setupIdentityProvider("hybrid", local)
	if pHybrid == nil {
		t.Fatal("expected non-nil provider for mode=hybrid")
	}

	// 3. mode="unknown"
	pDefault := setupIdentityProvider("unknown", local)
	if pDefault != local {
		t.Fatal("expected local provider fallback for unknown mode")
	}

	// 4. empty KEYCLOAK_URL with mode="keycloak"
	t.Setenv("KEYCLOAK_URL", "")
	t.Setenv("KEYCLOAK_JWKS_URL", "")
	pWarn := setupIdentityProvider("keycloak", local)
	if pWarn != local {
		t.Fatal("expected local provider fallback when KEYCLOAK_URL is empty")
	}
}

func TestSeedDefaultAdminAndBuildTokenEngine(t *testing.T) {
	// seedDefaultAdmin nil check
	seedDefaultAdmin(context.Background(), nil)
	seedDefaultAdmin(context.Background(), &UserStore{db: nil})

	// buildTokenEngine invalid RSA path
	t.Setenv("AUTH_PRIVATE_KEY_PATH", "invalid-path.pem")
	t.Setenv("AUTH_PUBLIC_KEY_PATH", "invalid-pub.pem")
	_, err := buildTokenEngine()
	if err == nil {
		t.Fatal("expected error for invalid RSA key file path")
	}
}
