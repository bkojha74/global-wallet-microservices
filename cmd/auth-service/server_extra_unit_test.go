package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"wallet-system/pkg/auth"
	authv1 "wallet-system/proto/auth"
)

type mockProvider struct {
	authErr        error
	valErr         error
	refErr         error
	claimsToReturn *UnifiedClaims
	pairToReturn   *TokenPair
}

func (m *mockProvider) Name() string { return "mock" }
func (m *mockProvider) Authenticate(_ context.Context, u, p string, scopes []string) (*UnifiedClaims, *TokenPair, error) {
	if m.authErr != nil {
		return nil, nil, m.authErr
	}
	return m.claimsToReturn, m.pairToReturn, nil
}
func (m *mockProvider) ValidateToken(_ context.Context, token string) (*UnifiedClaims, error) {
	if m.valErr != nil {
		return nil, m.valErr
	}
	return m.claimsToReturn, nil
}
func (m *mockProvider) RefreshToken(_ context.Context, token string) (*UnifiedClaims, *TokenPair, error) {
	if m.refErr != nil {
		return nil, nil, m.refErr
	}
	return m.claimsToReturn, m.pairToReturn, nil
}

func TestAuthServer_FullMethods(t *testing.T) {
	engine := NewHS256Engine("test-secret-key-32-bytes-minimum")
	store, _ := NewUserStore(nil)
	revStore, _ := NewRevocationStore(nil)

	prov := &mockProvider{
		claimsToReturn: &UnifiedClaims{
			Subject:   "alice",
			Roles:     []string{auth.RoleUser},
			Scopes:    []string{"wallet:read", "cluster:admin", "ledger:audit"},
			ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
			IssuedAt:  time.Now().Unix(),
			TokenID:   "tok-123",
		},
		pairToReturn: &TokenPair{
			AccessToken:  "access-tok-123",
			RefreshToken: "refresh-tok-123",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
		},
	}

	srv := &authServer{
		provider: prov,
		engine:   engine,
		store:    store,
		revStore: revStore,
	}

	ctx := context.Background()

	// 1. HealthCheck
	health, err := srv.HealthCheck(ctx, &authv1.AuthHealthRequest{})
	if err != nil || health.Status != "SERVING" {
		t.Errorf("Expected SERVING status, got %v, err=%v", health, err)
	}

	// 2. IssueToken invalid req
	if _, err := srv.IssueToken(ctx, &authv1.IssueTokenRequest{}); err == nil {
		t.Errorf("Expected error for empty credentials")
	}

	// IssueToken success
	resp, err := srv.IssueToken(ctx, &authv1.IssueTokenRequest{Username: "alice", Password: "secret"})
	if err != nil || resp.AccessToken != "access-tok-123" {
		t.Errorf("Expected successful token issuance, got %v, err=%v", resp, err)
	}

	// 3. RefreshToken empty
	if _, err := srv.RefreshToken(ctx, &authv1.RefreshTokenRequest{}); err == nil {
		t.Errorf("Expected error for empty refresh token")
	}
	// RefreshToken success
	refResp, err := srv.RefreshToken(ctx, &authv1.RefreshTokenRequest{RefreshToken: "refresh-tok-123"})
	if err != nil || refResp.AccessToken != "access-tok-123" {
		t.Errorf("Expected successful token refresh, got %v, err=%v", refResp, err)
	}

	// 4. ValidateToken empty
	valResp, _ := srv.ValidateToken(ctx, &authv1.ValidateTokenRequest{Token: ""})
	if valResp.Valid {
		t.Errorf("Expected invalid for empty token")
	}
	// ValidateToken success
	valSuccess, _ := srv.ValidateToken(ctx, &authv1.ValidateTokenRequest{Token: "valid-tok"})
	if !valSuccess.Valid || valSuccess.Subject != "alice" {
		t.Errorf("Expected valid token response, got %v", valSuccess)
	}

	// ValidateToken signature & expired errors
	prov.valErr = auth.ErrTokenExpired
	valExp, _ := srv.ValidateToken(ctx, &authv1.ValidateTokenRequest{Token: "expired-tok"})
	if valExp.Error != authv1.TokenErrorCode_TOKEN_EXPIRED {
		t.Errorf("Expected TOKEN_EXPIRED code, got %v", valExp.Error)
	}

	prov.valErr = auth.ErrInvalidSignature
	valSig, _ := srv.ValidateToken(ctx, &authv1.ValidateTokenRequest{Token: "bad-sig"})
	if valSig.Error != authv1.TokenErrorCode_TOKEN_INVALID_SIG {
		t.Errorf("Expected TOKEN_INVALID_SIG code, got %v", valSig.Error)
	}
	prov.valErr = nil

	// 5. RevokeToken empty
	if _, err := srv.RevokeToken(ctx, &authv1.RevokeTokenRequest{}); err == nil {
		t.Errorf("Expected error for empty token in RevokeToken")
	}

	// RevokeToken valid access token
	revResp, err := srv.RevokeToken(ctx, &authv1.RevokeTokenRequest{Token: "access-tok-123", Reason: "logout"})
	if err != nil || !revResp.Success {
		t.Errorf("Expected successful revocation, got %v, err=%v", revResp, err)
	}

	// RevokeToken invalid access token (falls back to refresh token revocation)
	prov.valErr = errors.New("not access token")
	revRefResp, err := srv.RevokeToken(ctx, &authv1.RevokeTokenRequest{Token: "refresh-tok-123"})
	if err != nil || !revRefResp.Success {
		t.Errorf("Expected successful refresh token revocation, got %v, err=%v", revRefResp, err)
	}
	prov.valErr = nil

	// 6. Authorize checks
	// Empty subject
	authEmpty, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{})
	if authEmpty.Allowed {
		t.Errorf("Expected false for empty subject")
	}

	// Admin role bypass
	authAdmin, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: "bob", Roles: []string{auth.RoleAdmin}})
	if !authAdmin.Allowed {
		t.Errorf("Expected admin to be allowed")
	}

	// Cluster scope
	authCluster, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: "alice", Resource: "cluster:prod", Scopes: []string{auth.ScopeClusterAdmin}})
	if !authCluster.Allowed {
		t.Errorf("Expected cluster admin allowed")
	}
	authClusterFail, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: "alice", Resource: "cluster:prod", Scopes: nil})
	if authClusterFail.Allowed {
		t.Errorf("Expected cluster admin rejected without scope")
	}

	// Ledger scope
	authLedger, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: "alice", Resource: "ledger:audit", Scopes: []string{auth.ScopeLedgerAudit}})
	if !authLedger.Allowed {
		t.Errorf("Expected ledger audit allowed")
	}
	authLedgerFail, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: "alice", Resource: "ledger:audit", Scopes: nil})
	if authLedgerFail.Allowed {
		t.Errorf("Expected ledger audit rejected without scope")
	}

	// Unsupported resource
	authUnsupp, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: "alice", Resource: "unknown:resource"})
	if authUnsupp.Allowed {
		t.Errorf("Expected unknown resource rejected")
	}

	// Wallet action
	authWallet, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: "alice", Resource: "wallet:w-100", Action: "read", Scopes: []string{"wallet:read"}})
	if !authWallet.Allowed {
		t.Errorf("Expected wallet read allowed")
	}
	authWalletBadAction, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{Subject: "alice", Resource: "wallet:w-100", Action: "delete", Scopes: []string{"wallet:read"}})
	if authWalletBadAction.Allowed {
		t.Errorf("Expected unsupported wallet action rejected")
	}
}
