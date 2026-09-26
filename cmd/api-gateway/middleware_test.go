package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"

	"wallet-system/pkg/auth"
	authv1 "wallet-system/proto/auth"
)

// fakeAuthClient is an in-process stub implementing authv1.AuthServiceClient.
// It validates tokens by parsing them with the same pkg/auth logic.
type fakeAuthClient struct {
	secret string
}

func (f *fakeAuthClient) IssueToken(_ context.Context, _ *authv1.IssueTokenRequest, _ ...grpc.CallOption) (*authv1.IssueTokenResponse, error) {
	return nil, nil
}
func (f *fakeAuthClient) ValidateToken(_ context.Context, req *authv1.ValidateTokenRequest, _ ...grpc.CallOption) (*authv1.ValidateTokenResponse, error) {
	claims, err := auth.ValidateToken(req.Token, f.secret)
	if err != nil {
		code := authv1.TokenErrorCode_TOKEN_MALFORMED
		if err == auth.ErrTokenExpired {
			code = authv1.TokenErrorCode_TOKEN_EXPIRED
		}
		return &authv1.ValidateTokenResponse{Valid: false, Error: code}, nil
	}
	return &authv1.ValidateTokenResponse{
		Valid:     true,
		Subject:   claims.Subject,
		Roles:     claims.Roles,
		Scopes:    claims.Scopes,
		ExpiresAt: claims.ExpiresAt,
		Error:     authv1.TokenErrorCode_TOKEN_OK,
	}, nil
}
func (f *fakeAuthClient) RefreshToken(_ context.Context, _ *authv1.RefreshTokenRequest, _ ...grpc.CallOption) (*authv1.IssueTokenResponse, error) {
	return nil, nil
}
func (f *fakeAuthClient) RevokeToken(_ context.Context, _ *authv1.RevokeTokenRequest, _ ...grpc.CallOption) (*authv1.RevokeTokenResponse, error) {
	return &authv1.RevokeTokenResponse{Success: true}, nil
}
func (f *fakeAuthClient) Authorize(_ context.Context, _ *authv1.AuthorizeRequest, _ ...grpc.CallOption) (*authv1.AuthorizeResponse, error) {
	return &authv1.AuthorizeResponse{Allowed: true}, nil
}
func (f *fakeAuthClient) HealthCheck(_ context.Context, _ *authv1.AuthHealthRequest, _ ...grpc.CallOption) (*authv1.AuthHealthResponse, error) {
	return &authv1.AuthHealthResponse{Status: "SERVING"}, nil
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	handler := SecurityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("expected nosniff, got %q", rec.Header().Get("X-Content-Type-Options"))
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("expected DENY, got %q", rec.Header().Get("X-Frame-Options"))
	}
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Error("expected HSTS header to be set")
	}
}

func TestMaxBytesMiddleware(t *testing.T) {
	// Limit to 10 bytes
	handler := MaxBytesMiddleware(10, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Small payload (< 10 bytes)
	reqSmall := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", bytes.NewReader([]byte("small")))
	recSmall := httptest.NewRecorder()
	handler.ServeHTTP(recSmall, reqSmall)
	if recSmall.Code != http.StatusOK {
		t.Fatalf("expected 200 for small payload, got %d", recSmall.Code)
	}

	// Large payload (> 10 bytes)
	reqLarge := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", bytes.NewReader([]byte("this payload is definitely longer than 10 bytes")))
	recLarge := httptest.NewRecorder()
	handler.ServeHTTP(recLarge, reqLarge)
	if recLarge.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized payload, got %d", recLarge.Code)
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	// 1 token/sec, burst 2
	limiter := NewRateLimiter(1.0, 2)
	handler := RateLimitMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Request 1: OK
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/wallets", nil)
	req1.RemoteAddr = "192.168.1.50:12345"
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 on request 1, got %d", rec1.Code)
	}

	// Request 2: OK (burst consumed)
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/wallets", nil)
	req2.RemoteAddr = "192.168.1.50:12345"
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 on request 2, got %d", rec2.Code)
	}

	// Request 3: 429 Rate limited
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/wallets", nil)
	req3.RemoteAddr = "192.168.1.50:12345"
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on request 3, got %d", rec3.Code)
	}
}

func TestAuthMiddleware(t *testing.T) {
	secret := "jwt-test-secret"
	publicPaths := map[string]bool{
		"/healthz": true,
	}

	fakeClient := &fakeAuthClient{secret: secret}
	cache := newClaimsCache("test-cache-secret")

	handler := AuthMiddleware(fakeClient, cache, publicPaths, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFromContext(r.Context())
		if ok && claims != nil {
			w.Header().Set("X-User", claims.Subject)
		}
		w.WriteHeader(http.StatusOK)
	}))

	// 1. Public path without token: OK
	reqPub := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recPub := httptest.NewRecorder()
	handler.ServeHTTP(recPub, reqPub)
	if recPub.Code != http.StatusOK {
		t.Fatalf("expected 200 on public path, got %d", recPub.Code)
	}

	// 2. Protected path without token: 401
	reqUnauth := httptest.NewRequest(http.MethodGet, "/api/v1/wallets", nil)
	recUnauth := httptest.NewRecorder()
	handler.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on protected path without token, got %d", recUnauth.Code)
	}

	// 3. Protected path with invalid token: 401
	reqInvalid := httptest.NewRequest(http.MethodGet, "/api/v1/wallets", nil)
	reqInvalid.Header.Set("Authorization", "Bearer bad-token")
	recInvalid := httptest.NewRecorder()
	handler.ServeHTTP(recInvalid, reqInvalid)
	if recInvalid.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on invalid token, got %d", recInvalid.Code)
	}

	// 4. Protected path with valid token: 200 and claims in context
	validToken, err := auth.GenerateToken(auth.Claims{
		Subject: "alice",
		Roles:   []string{auth.RoleUser},
	}, secret)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	reqValid := httptest.NewRequest(http.MethodGet, "/api/v1/wallets", nil)
	reqValid.Header.Set("Authorization", "Bearer "+validToken)
	recValid := httptest.NewRecorder()
	handler.ServeHTTP(recValid, reqValid)
	if recValid.Code != http.StatusOK {
		t.Fatalf("expected 200 on valid token, got %d", recValid.Code)
	}
	if recValid.Header().Get("X-User") != "alice" {
		t.Fatalf("expected X-User alice, got %s", recValid.Header().Get("X-User"))
	}

	// 5. Second request with same token: served from cache (same result)
	recCached := httptest.NewRecorder()
	handler.ServeHTTP(recCached, reqValid)
	if recCached.Code != http.StatusOK {
		t.Fatalf("expected 200 on cached token, got %d", recCached.Code)
	}
}

func TestRequireRoleAndScope(t *testing.T) {
	adminHandler := RequireRole(auth.RoleAdmin, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	transferScopeHandler := RequireScope(auth.ScopeWalletTransfer, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// User without admin role accessing adminHandler -> 403
	userClaims := &auth.Claims{
		Subject: "bob",
		Roles:   []string{auth.RoleUser},
		Scopes:  []string{auth.ScopeWalletRead},
	}
	ctx := ContextWithClaims(httptest.NewRequest(http.MethodPost, "/", nil).Context(), userClaims)

	reqUser := httptest.NewRequest(http.MethodPost, "/cluster/failover", nil).WithContext(ctx)
	recUser := httptest.NewRecorder()
	adminHandler(recUser, reqUser)
	if recUser.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for user accessing admin endpoint, got %d", recUser.Code)
	}

	// Admin accessing adminHandler -> 200
	adminClaims := &auth.Claims{
		Subject: "ops-admin",
		Roles:   []string{auth.RoleAdmin},
	}
	ctxAdmin := ContextWithClaims(httptest.NewRequest(http.MethodPost, "/", nil).Context(), adminClaims)
	reqAdmin := httptest.NewRequest(http.MethodPost, "/cluster/failover", nil).WithContext(ctxAdmin)
	recAdmin := httptest.NewRecorder()
	adminHandler(recAdmin, reqAdmin)
	if recAdmin.Code != http.StatusOK {
		t.Fatalf("expected 200 for admin accessing admin endpoint, got %d", recAdmin.Code)
	}

	// User without transfer scope accessing transferScopeHandler -> 403
	recTransferDeny := httptest.NewRecorder()
	transferScopeHandler(recTransferDeny, reqUser)
	if recTransferDeny.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for user missing transfer scope, got %d", recTransferDeny.Code)
	}
}

func TestValidateWalletOwnershipIDOR(t *testing.T) {
	aliceClaims := &auth.Claims{Subject: "alice", Roles: []string{auth.RoleUser}}
	adminClaims := &auth.Claims{Subject: "admin", Roles: []string{auth.RoleAdmin}}

	// Alice acting on Alice's wallet -> Allowed
	if err := ValidateWalletOwnership(aliceClaims, "alice"); err != nil {
		t.Fatalf("expected ownership valid, got: %v", err)
	}

	// Alice acting on Bob's wallet -> IDOR Blocked!
	if err := ValidateWalletOwnership(aliceClaims, "bob"); err != auth.ErrIDORViolation {
		t.Fatalf("expected IDOR violation for Alice acting on Bob, got: %v", err)
	}

	// Admin acting on Bob's wallet -> Allowed
	if err := ValidateWalletOwnership(adminClaims, "bob"); err != nil {
		t.Fatalf("expected admin allowed on any wallet, got: %v", err)
	}

	// Nil claims -> Insufficient perms
	if err := ValidateWalletOwnership(nil, "bob"); err != auth.ErrInsufficientPerms {
		t.Fatalf("expected ErrInsufficientPerms for nil claims, got: %v", err)
	}

	// tokenErrorMessage helper
	if msg := tokenErrorMessage(authv1.TokenErrorCode_TOKEN_EXPIRED); msg != "token has expired" {
		t.Fatalf("unexpected msg for expired token: %s", msg)
	}
	if msg := tokenErrorMessage(authv1.TokenErrorCode_TOKEN_REVOKED); msg != "token has been revoked" {
		t.Fatalf("unexpected msg for revoked token: %s", msg)
	}
	if msg := tokenErrorMessage(authv1.TokenErrorCode_TOKEN_INVALID_SIG); msg != "token signature is invalid" {
		t.Fatalf("unexpected msg for invalid sig: %s", msg)
	}

	// claimsCache invalidate
	c := newClaimsCache("secret")
	c.set("t1", &auth.Claims{Subject: "sub1"})
	if _, ok := c.get("t1"); !ok {
		t.Fatalf("expected t1 in cache")
	}
	c.invalidate("t1")
	if _, ok := c.get("t1"); ok {
		t.Fatalf("expected t1 invalidated")
	}
}
