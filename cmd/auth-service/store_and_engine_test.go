package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"wallet-system/pkg/auth"
	"wallet-system/pkg/db"
)

func TestRS256TokenEngine(t *testing.T) {
	// Generate RSA key pair
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	engine := NewRS256Engine(privKey, &privKey.PublicKey, "test-kid-1")

	// 1. Issue RS256 token
	claims := auth.Claims{
		Subject: "alice",
		Roles:   []string{auth.RoleUser},
		Scopes:  []string{"wallet:read"},
	}
	tokenStr, err := engine.IssueAccessToken(claims)
	if err != nil {
		t.Fatalf("IssueAccessToken failed: %v", err)
	}

	// 2. Validate RS256 token
	validated, err := engine.ValidateAccessToken(tokenStr)
	if err != nil {
		t.Fatalf("ValidateAccessToken failed: %v", err)
	}
	if validated.Subject != "alice" {
		t.Fatalf("expected subject 'alice', got %s", validated.Subject)
	}

	// 3. Issue Refresh Token
	refToken, err := engine.IssueRefreshToken()
	if err != nil || refToken == "" {
		t.Fatalf("IssueRefreshToken failed: %v, token=%s", err, refToken)
	}
}

func TestRevocationStore_OfflineUnit(t *testing.T) {
	ctx := context.Background()
	revStore, err := NewRevocationStore(nil)
	if err != nil || revStore == nil {
		t.Fatalf("expected NewRevocationStore(nil) to succeed, got %v", err)
	}

	isRev, err := revStore.IsRevoked(ctx, "jti-1")
	if err != nil || isRev {
		t.Fatalf("expected IsRevoked(nil) to return false, nil")
	}

	if err := revStore.Revoke(ctx, "jti-1", "user", "logout", time.Now()); err != nil {
		t.Fatalf("expected Revoke(nil) to return nil")
	}

	deleted, err := revStore.PurgeExpired(ctx)
	if err != nil || deleted != 0 {
		t.Fatalf("expected PurgeExpired(nil) to return 0, nil")
	}

	var nilStore *RevocationStore
	if col := nilStore.col(); col != nil {
		t.Fatalf("expected nilStore.col() to return nil")
	}
}

func TestRevocationStoreLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 1)
	if err != nil {
		t.Skipf("MongoDB unavailable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	authDB := client.Database("auth_db_test_revocation")
	_ = authDB.Drop(ctx)

	revStore, err := NewRevocationStore(authDB)
	if err != nil {
		t.Fatalf("NewRevocationStore failed: %v", err)
	}

	// 1. Initially not revoked
	isRev, err := revStore.IsRevoked(ctx, "jti-123")
	if err != nil || isRev {
		t.Fatalf("expected false for unrevoked token, got %t, err=%v", isRev, err)
	}

	// 2. Revoke token
	if err := revStore.Revoke(ctx, "jti-123", "alice", "logout", time.Now().Add(1*time.Hour)); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}

	// 3. Verify revoked
	isRev2, err := revStore.IsRevoked(ctx, "jti-123")
	if err != nil || !isRev2 {
		t.Fatalf("expected true for revoked token, got %t, err=%v", isRev2, err)
	}

	// 4. Duplicate revocation (idempotent)
	if err := revStore.Revoke(ctx, "jti-123", "alice", "logout", time.Now().Add(1*time.Hour)); err != nil {
		t.Fatalf("duplicate Revoke should succeed, got %v", err)
	}
}

func TestUserStore_OfflineUnit(t *testing.T) {
	ctx := context.Background()
	store, err := NewUserStore(nil)
	if err != nil || store == nil {
		t.Fatalf("expected NewUserStore(nil) to succeed, got %v", err)
	}

	if err := store.CreateUser(ctx, "user", "e", "p", nil, nil); err == nil {
		t.Fatal("expected error for CreateUser on nil store")
	}
	if _, err := store.Authenticate(ctx, "user", "p"); err == nil {
		t.Fatal("expected error for Authenticate on nil store")
	}
	if err := store.StoreRefreshToken(ctx, "rt", "sub", nil, nil, 1*time.Hour); err == nil {
		t.Fatal("expected error for StoreRefreshToken on nil store")
	}
	if _, err := store.FindRefreshToken(ctx, "rt"); err == nil {
		t.Fatal("expected error for FindRefreshToken on nil store")
	}
	if err := store.RevokeRefreshToken(ctx, "rt"); err == nil {
		t.Fatal("expected error for RevokeRefreshToken on nil store")
	}
	if err := store.AddWalletToUser(ctx, "sub", "w1"); err == nil {
		t.Fatal("expected error for AddWalletToUser on nil store")
	}
	if owns, err := store.OwnsWallet(ctx, "sub", "w1"); err != nil || !owns {
		t.Fatalf("expected true, nil for OwnsWallet when db is nil, got %t, err=%v", owns, err)
	}

	var nilStore *UserStore
	if col := nilStore.users(); col != nil {
		t.Fatal("expected nilStore.users() to return nil")
	}
	if col := nilStore.refreshTokens(); col != nil {
		t.Fatal("expected nilStore.refreshTokens() to return nil")
	}
}

func TestUserStoreLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 1)
	if err != nil {
		t.Skipf("MongoDB unavailable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	authDB := client.Database("auth_db_test_userstore")
	_ = authDB.Drop(ctx)

	store, err := NewUserStore(authDB)
	if err != nil {
		t.Fatalf("NewUserStore failed: %v", err)
	}

	// 1. Create User
	err = store.CreateUser(ctx, "bob", "bob@test.com", "secretpass123", []string{auth.RoleUser}, []string{"wallet:read"})
	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	// 2. Authenticate Password Success
	u, err := store.Authenticate(ctx, "bob", "secretpass123")
	if err != nil || u == nil {
		t.Fatalf("Authenticate failed: %v", err)
	}

	// 3. Authenticate Password Failure
	_, errFail := store.Authenticate(ctx, "bob", "wrongpass")
	if errFail == nil {
		t.Fatal("expected error for wrong password")
	}

	// 4. Store and Find Refresh Token
	errStore := store.StoreRefreshToken(ctx, "ref-token-xyz", "bob", []string{auth.RoleUser}, []string{"wallet:read"}, 24*time.Hour)
	if errStore != nil {
		t.Fatalf("StoreRefreshToken failed: %v", errStore)
	}

	foundRef, errFind := store.FindRefreshToken(ctx, "ref-token-xyz")
	if errFind != nil || foundRef == nil {
		t.Fatalf("FindRefreshToken failed: %v", errFind)
	}

	// Revoke Refresh Token
	if errRev := store.RevokeRefreshToken(ctx, "ref-token-xyz"); errRev != nil {
		t.Fatalf("RevokeRefreshToken failed: %v", errRev)
	}

	// 5. Add Wallet & Check Ownership
	if err := store.AddWalletToUser(ctx, "bob", "w-bob-100"); err != nil {
		t.Fatalf("AddWalletToUser failed: %v", err)
	}

	owns, errOwns := store.OwnsWallet(ctx, "bob", "w-bob-100")
	if errOwns != nil || !owns {
		t.Fatalf("expected bob to own w-bob-100, got %t, err=%v", owns, errOwns)
	}

	notOwns, _ := store.OwnsWallet(ctx, "bob", "w-other-999")
	if notOwns {
		t.Fatal("expected bob not to own w-other-999")
	}
}

func TestOIDCClientHelpers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// OIDCClient with explicit JWKS fallback
	cli, err := NewOIDCClient(ctx, "http://invalid-keycloak:8080/realms/wallet-realm", "wallet-api", "secret", "http://explicit-jwks/certs")
	if err != nil || cli == nil {
		t.Fatalf("NewOIDCClient failed: %v", err)
	}

	if cli.JwksURI() != "http://explicit-jwks/certs" {
		t.Fatalf("unexpected JwksURI: %s", cli.JwksURI())
	}
	if cli.Issuer() == "" {
		t.Fatal("expected non-empty Issuer")
	}

	// AuthenticatePassword & RefreshToken error paths without token endpoint
	_, errAuth := cli.AuthenticatePassword(ctx, "user", "pass", nil)
	if errAuth == nil {
		t.Fatal("expected error on AuthenticatePassword without discovery token endpoint")
	}

	_, errRef := cli.RefreshToken(ctx, "token")
	if errRef == nil {
		t.Fatal("expected error on RefreshToken without discovery token endpoint")
	}
}
