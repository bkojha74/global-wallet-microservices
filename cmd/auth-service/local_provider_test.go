package main

import (
	"context"
	"testing"
	"time"

	"wallet-system/pkg/auth"
	"wallet-system/pkg/db"
)

func TestLocalProviderComplete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 1)
	if err != nil {
		t.Skipf("MongoDB unavailable: %v", err)
		return
	}
	defer func() { _ = client.Disconnect(ctx) }()

	authDB := client.Database("auth_db_test_local_prov")
	_ = authDB.Drop(ctx)

	store, err := NewUserStore(authDB)
	if err != nil {
		t.Fatalf("NewUserStore failed: %v", err)
	}

	engine := NewHS256Engine("test-secret-key-32-bytes-minimum")
	revStore, err := NewRevocationStore(authDB)
	if err != nil {
		t.Fatalf("NewRevocationStore failed: %v", err)
	}

	prov := NewLocalProvider(store, engine, revStore)
	if prov.Name() != "local" {
		t.Fatalf("expected name 'local', got %s", prov.Name())
	}

	// 1. Create User
	if err := store.CreateUser(ctx, "charlie", "charlie@test.com", "pass123456", []string{auth.RoleUser}, []string{"wallet:read", "wallet:transfer"}); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	// 2. Authenticate
	uClaims, tokens, errAuth := prov.Authenticate(ctx, "charlie", "pass123456", []string{"wallet:read"})
	if errAuth != nil || uClaims == nil || tokens == nil {
		t.Fatalf("LocalProvider.Authenticate failed: %v", errAuth)
	}

	// 3. ValidateToken
	valClaims, errVal := prov.ValidateToken(ctx, tokens.AccessToken)
	if errVal != nil || valClaims.Subject != "charlie" {
		t.Fatalf("LocalProvider.ValidateToken failed: %v", errVal)
	}

	// 4. RefreshToken
	refClaims, refTokens, errRef := prov.RefreshToken(ctx, tokens.RefreshToken)
	if errRef != nil || refClaims == nil || refTokens == nil {
		t.Fatalf("LocalProvider.RefreshToken failed: %v", errRef)
	}

	// 5. Authenticate invalid credentials
	_, _, errBad := prov.Authenticate(ctx, "charlie", "wrongpass", nil)
	if errBad == nil {
		t.Fatal("expected error on invalid credentials")
	}
}
