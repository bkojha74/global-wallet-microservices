package main

import (
	"context"
	"errors"
	"testing"

	"wallet-system/pkg/auth"
	authv1 "wallet-system/proto/auth"
)

func TestAuthServerMethodsComplete(t *testing.T) {
	localProvider := &LocalProvider{}
	engine := NewHS256Engine("test-secret-32-bytes-long-key-1234567")
	srv := &authServer{
		provider: localProvider,
		engine:   engine,
	}

	ctx := context.Background()

	// 1. IssueToken - missing credentials
	_, errMissing := srv.IssueToken(ctx, &authv1.IssueTokenRequest{})
	if errMissing == nil {
		t.Fatal("expected error for missing credentials")
	}

	// 2. ValidateToken - empty token
	valEmpty, _ := srv.ValidateToken(ctx, &authv1.ValidateTokenRequest{Token: ""})
	if valEmpty.Valid || valEmpty.Error != authv1.TokenErrorCode_TOKEN_MALFORMED {
		t.Fatalf("expected MALFORMED for empty token, got %+v", valEmpty)
	}

	// 3. RefreshToken - empty token
	_, errRefEmpty := srv.RefreshToken(ctx, &authv1.RefreshTokenRequest{RefreshToken: ""})
	if errRefEmpty == nil {
		t.Fatal("expected error for empty refresh token")
	}

	// 4. RevokeToken - empty token
	_, errRevEmpty := srv.RevokeToken(ctx, &authv1.RevokeTokenRequest{Token: ""})
	if errRevEmpty == nil {
		t.Fatal("expected error for empty token in RevokeToken")
	}

	// 5. Authorize - Cluster & Ledger resources
	// Cluster admin success
	authCluster, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{
		Subject:  "ops",
		Resource: "cluster:failover",
		Action:   "post",
		Scopes:   []string{auth.ScopeClusterAdmin},
	})
	if !authCluster.Allowed {
		t.Fatalf("expected cluster authorization allowed, got %+v", authCluster)
	}

	// Cluster admin missing scope
	authClusterFail, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{
		Subject:  "ops",
		Resource: "cluster:failover",
		Action:   "post",
		Scopes:   []string{"wallet:read"},
	})
	if authClusterFail.Allowed {
		t.Fatal("expected cluster authorization to fail without cluster:admin scope")
	}

	// Ledger audit success
	authLedger, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{
		Subject:  "auditor",
		Resource: "ledger:entries",
		Action:   "read",
		Scopes:   []string{auth.ScopeLedgerAudit},
	})
	if !authLedger.Allowed {
		t.Fatalf("expected ledger audit allowed, got %+v", authLedger)
	}

	// Ledger audit fail
	authLedgerFail, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{
		Subject:  "auditor",
		Resource: "ledger:entries",
		Action:   "read",
		Scopes:   []string{"wallet:read"},
	})
	if authLedgerFail.Allowed {
		t.Fatal("expected ledger audit to fail without ledger:audit scope")
	}

	// 6. mapAuthErrorToProto
	if code := mapAuthErrorToProto(auth.ErrTokenExpired); code != authv1.TokenErrorCode_TOKEN_EXPIRED {
		t.Fatalf("expected TOKEN_EXPIRED, got %v", code)
	}
	if code := mapAuthErrorToProto(auth.ErrInvalidSignature); code != authv1.TokenErrorCode_TOKEN_INVALID_SIG {
		t.Fatalf("expected TOKEN_INVALID_SIG, got %v", code)
	}
	if code := mapAuthErrorToProto(errors.New("other")); code != authv1.TokenErrorCode_TOKEN_MALFORMED {
		t.Fatalf("expected TOKEN_MALFORMED, got %v", code)
	}

	// 7. HealthCheck
	hResp, errH := srv.HealthCheck(ctx, &authv1.AuthHealthRequest{})
	if errH != nil || hResp.Status != "SERVING" {
		t.Fatalf("expected SERVING status from HealthCheck")
	}

	// 8. Authorize edge cases
	noSub, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: ""})
	if noSub.Allowed {
		t.Fatalf("expected failure for empty subject")
	}

	adminAuth, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: "admin", Roles: []string{"admin"}})
	if !adminAuth.Allowed || adminAuth.Reason != "admin role" {
		t.Fatalf("expected admin bypass")
	}

	noPolicy, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: "user", Resource: "unknown:thing"})
	if noPolicy.Allowed {
		t.Fatalf("expected failure for unknown resource")
	}

	badWalletAction, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{
		Subject:  "user",
		Resource: "wallet:w1",
		Action:   "delete",
		Scopes:   []string{"wallet:delete"},
	})
	if badWalletAction.Allowed {
		t.Fatalf("expected failure for unsupported wallet action")
	}

	missingWalletScope, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{
		Subject:  "user",
		Resource: "wallet:w1",
		Action:   "transfer",
		Scopes:   []string{"wallet:read"},
	})
	if missingWalletScope.Allowed {
		t.Fatalf("expected failure for missing wallet:transfer scope")
	}

	// 9. resolveScopes
	resEmpty := resolveScopes([]string{"user"}, []string{"wallet:read"}, nil)
	if len(resEmpty) == 0 {
		t.Fatalf("expected default scopes for empty requested")
	}
	resGranted := resolveScopes([]string{"user"}, []string{"wallet:read", "wallet:transfer"}, []string{"wallet:read"})
	if len(resGranted) != 1 || resGranted[0] != "wallet:read" {
		t.Fatalf("expected wallet:read scope granted")
	}
	resFallback := resolveScopes([]string{"user"}, []string{"wallet:read"}, []string{"cluster:admin"})
	if len(resFallback) == 0 {
		t.Fatalf("expected fallback scopes")
	}
}

func TestAuthServer_FullTokenLifecycle(t *testing.T) {
	engine := NewHS256Engine("test-secret-32-bytes-long-key-1234567")
	userStore, _ := NewUserStore(nil)
	revStore, _ := NewRevocationStore(nil)

	localProvider := NewLocalProvider(userStore, engine, revStore)
	srv := &authServer{
		provider: localProvider,
		engine:   engine,
		store:    userStore,
		revStore: revStore,
	}

	ctx := context.Background()

	// 1. IssueToken with local provider (requires DB or fallback)
	issueResp, err := srv.IssueToken(ctx, &authv1.IssueTokenRequest{
		Username: "admin",
		Password: "change-me-in-production",
	})
	if err != nil {
		t.Skipf("IssueToken unavailable without DB: %v", err)
		return
	}
	if issueResp.AccessToken == "" || issueResp.RefreshToken == "" {
		t.Fatalf("expected non-empty tokens in IssueToken response")
	}

	// 2. ValidateToken
	valResp, err := srv.ValidateToken(ctx, &authv1.ValidateTokenRequest{
		Token: issueResp.AccessToken,
	})
	if err != nil || !valResp.Valid {
		t.Fatalf("ValidateToken expected valid token, got %+v, err: %v", valResp, err)
	}

	// 3. RefreshToken
	refResp, err := srv.RefreshToken(ctx, &authv1.RefreshTokenRequest{
		RefreshToken: issueResp.RefreshToken,
	})
	if err != nil {
		t.Fatalf("RefreshToken failed: %v", err)
	}
	if refResp.AccessToken == "" {
		t.Fatalf("expected new access token from RefreshToken")
	}

	// 4. RevokeToken
	revResp, err := srv.RevokeToken(ctx, &authv1.RevokeTokenRequest{
		Token:  issueResp.AccessToken,
		Reason: "user_logout",
	})
	if err != nil || !revResp.Success {
		t.Fatalf("RevokeToken failed: %v", err)
	}
}
