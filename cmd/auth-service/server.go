package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"wallet-system/pkg/auth"
	authv1 "wallet-system/proto/auth"
)

// refreshTokenTTL is the lifetime of an internal refresh token session.
const refreshTokenTTL = 7 * 24 * time.Hour

// authServer implements authv1.AuthServiceServer with pluggable identity providers.
type authServer struct {
	authv1.UnimplementedAuthServiceServer
	provider IdentityProvider
	engine   *TokenEngine
	store    *UserStore
	revStore *RevocationStore
}

// IssueToken authenticates credentials against the active IdentityProvider and returns tokens.
func (s *authServer) IssueToken(ctx context.Context, req *authv1.IssueTokenRequest) (*authv1.IssueTokenResponse, error) {
	if req.Username == "" || req.Password == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "username and password are required")
	}

	claims, tokens, err := s.provider.Authenticate(ctx, req.Username, req.Password, req.RequestedScopes)
	if err != nil {
		log.Printf("[AUTH-SERVER] IssueToken failed for user=%q: %v", req.Username, err)
		return nil, grpcstatus.Error(codes.Unauthenticated, "invalid credentials")
	}

	log.Printf("[AUTH-SERVER] token issued: subject=%s roles=%v (provider=%s)", claims.Subject, claims.Roles, s.provider.Name())
	return &authv1.IssueTokenResponse{
		AccessToken:   tokens.AccessToken,
		RefreshToken:  tokens.RefreshToken,
		TokenType:     tokens.TokenType,
		ExpiresIn:     tokens.ExpiresIn,
		GrantedScopes: tokens.GrantedScopes,
		Subject:       claims.Subject,
	}, nil
}

// ValidateToken verifies JWT signature, expiry, and revocation across providers.
func (s *authServer) ValidateToken(ctx context.Context, req *authv1.ValidateTokenRequest) (*authv1.ValidateTokenResponse, error) {
	if req.Token == "" {
		return &authv1.ValidateTokenResponse{Valid: false, Error: authv1.TokenErrorCode_TOKEN_MALFORMED}, nil
	}

	claims, err := s.provider.ValidateToken(ctx, req.Token)
	if err != nil {
		code := mapAuthErrorToProto(err)
		return &authv1.ValidateTokenResponse{Valid: false, Error: code}, nil
	}

	tokenID := claims.TokenID
	if tokenID == "" {
		tokenID = deriveTokenID(&auth.Claims{Subject: claims.Subject, IssuedAt: claims.IssuedAt})
	}

	revoked, err := s.revStore.IsRevoked(ctx, tokenID)
	if err != nil {
		log.Printf("[AUTH-SERVER] revocation check error: %v", err)
	}
	if revoked {
		return &authv1.ValidateTokenResponse{Valid: false, Error: authv1.TokenErrorCode_TOKEN_REVOKED}, nil
	}

	return &authv1.ValidateTokenResponse{
		Valid:     true,
		Subject:   claims.Subject,
		Roles:     claims.Roles,
		Scopes:    claims.Scopes,
		ExpiresAt: claims.ExpiresAt,
		TokenId:   tokenID,
		Error:     authv1.TokenErrorCode_TOKEN_OK,
	}, nil
}

// RefreshToken exchanges a refresh token for a new access token via the active IdentityProvider.
func (s *authServer) RefreshToken(ctx context.Context, req *authv1.RefreshTokenRequest) (*authv1.IssueTokenResponse, error) {
	if req.RefreshToken == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "refresh_token is required")
	}

	claims, tokens, err := s.provider.RefreshToken(ctx, req.RefreshToken)
	if err != nil {
		return nil, grpcstatus.Error(codes.Unauthenticated, "invalid or expired refresh token")
	}

	log.Printf("[AUTH-SERVER] token refreshed: subject=%s (provider=%s)", claims.Subject, s.provider.Name())
	return &authv1.IssueTokenResponse{
		AccessToken:   tokens.AccessToken,
		RefreshToken:  tokens.RefreshToken,
		TokenType:     tokens.TokenType,
		ExpiresIn:     tokens.ExpiresIn,
		GrantedScopes: tokens.GrantedScopes,
		Subject:       claims.Subject,
	}, nil
}

// RevokeToken adds a token to the blocklist and invalidates refresh sessions.
func (s *authServer) RevokeToken(ctx context.Context, req *authv1.RevokeTokenRequest) (*authv1.RevokeTokenResponse, error) {
	if req.Token == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "token is required")
	}

	reason := req.Reason
	if reason == "" {
		reason = "logout"
	}

	claims, err := s.provider.ValidateToken(ctx, req.Token)
	if err == nil {
		tokenID := claims.TokenID
		if tokenID == "" {
			tokenID = deriveTokenID(&auth.Claims{Subject: claims.Subject, IssuedAt: claims.IssuedAt})
		}
		expiresAt := time.Unix(claims.ExpiresAt, 0)
		if rErr := s.revStore.Revoke(ctx, tokenID, claims.Subject, reason, expiresAt); rErr != nil {
			log.Printf("[AUTH-SERVER] RevokeToken blocklist error: %v", rErr)
		}
		log.Printf("[AUTH-SERVER] access token revoked: subject=%s reason=%s", claims.Subject, reason)
	} else {
		// Attempt to revoke as refresh token in local user store
		if rErr := s.store.RevokeRefreshToken(ctx, req.Token); rErr != nil {
			log.Printf("[AUTH-SERVER] RevokeToken refresh token error: %v", rErr)
		}
		log.Printf("[AUTH-SERVER] refresh token revoked: reason=%s", reason)
	}

	return &authv1.RevokeTokenResponse{Success: true, Message: "token revoked"}, nil
}

// Authorize makes a real-time RBAC + ABAC decision across all identity sources.
func (s *authServer) Authorize(ctx context.Context, req *authv1.AuthorizeRequest) (*authv1.AuthorizeResponse, error) {
	if req.Subject == "" {
		return &authv1.AuthorizeResponse{Allowed: false, Reason: "missing subject"}, nil
	}

	// Admin role bypasses all checks
	for _, role := range req.Roles {
		if strings.EqualFold(role, auth.RoleAdmin) {
			return &authv1.AuthorizeResponse{Allowed: true, Reason: "admin role"}, nil
		}
	}

	if strings.HasPrefix(req.Resource, "wallet:") {
		return s.authorizeWallet(ctx, req)
	}
	if strings.HasPrefix(req.Resource, "cluster:") {
		return s.authorizeCluster(req)
	}
	if strings.HasPrefix(req.Resource, "ledger:") {
		return s.authorizeLedger(req)
	}

	return &authv1.AuthorizeResponse{Allowed: false, Reason: "no matching policy"}, nil
}

func (s *authServer) authorizeWallet(ctx context.Context, req *authv1.AuthorizeRequest) (*authv1.AuthorizeResponse, error) {
	walletID := strings.TrimPrefix(req.Resource, "wallet:")
	if req.Action != "read" && req.Action != "transfer" {
		return &authv1.AuthorizeResponse{Allowed: false, Reason: "unsupported wallet action"}, nil
	}
	required := fmt.Sprintf("wallet:%s", req.Action)
	if !hasScope(req.Scopes, required) {
		return &authv1.AuthorizeResponse{
			Allowed: false,
			Reason:  fmt.Sprintf("missing scope: %s", required),
		}, nil
	}

	// Verify ownership against user store if record exists
	if s.store != nil {
		owns, err := s.store.OwnsWallet(ctx, req.Subject, walletID)
		if err != nil {
			log.Printf("[AUTH-SERVER] Authorize wallet ownership check error: %v", err)
			return &authv1.AuthorizeResponse{Allowed: false, Reason: "ownership check failed"}, nil
		}
		if !owns {
			return &authv1.AuthorizeResponse{
				Allowed: false,
				Reason:  fmt.Sprintf("subject %s does not own wallet %s", req.Subject, walletID),
			}, nil
		}
	}
	return &authv1.AuthorizeResponse{Allowed: true, Reason: "wallet owner"}, nil
}

func (s *authServer) authorizeCluster(req *authv1.AuthorizeRequest) (*authv1.AuthorizeResponse, error) {
	if !hasScope(req.Scopes, auth.ScopeClusterAdmin) {
		return &authv1.AuthorizeResponse{Allowed: false, Reason: "requires cluster:admin scope"}, nil
	}
	return &authv1.AuthorizeResponse{Allowed: true, Reason: "cluster admin scope"}, nil
}

func (s *authServer) authorizeLedger(req *authv1.AuthorizeRequest) (*authv1.AuthorizeResponse, error) {
	if !hasScope(req.Scopes, auth.ScopeLedgerAudit) {
		return &authv1.AuthorizeResponse{Allowed: false, Reason: "requires ledger:audit scope"}, nil
	}
	return &authv1.AuthorizeResponse{Allowed: true, Reason: "ledger audit scope"}, nil
}

// HealthCheck returns the service health status.
func (s *authServer) HealthCheck(_ context.Context, _ *authv1.AuthHealthRequest) (*authv1.AuthHealthResponse, error) {
	return &authv1.AuthHealthResponse{Status: "SERVING", Version: "1.1.0"}, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// resolveScopes intersects requested scopes with the user's allowed scopes.
func resolveScopes(roles, allowed, requested []string) []string {
	if len(requested) == 0 {
		return DefaultScopesForRoles(roles)
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, s := range allowed {
		allowedSet[strings.ToLower(s)] = true
	}
	var granted []string
	for _, s := range requested {
		if allowedSet[strings.ToLower(s)] {
			granted = append(granted, s)
		}
	}
	if len(granted) == 0 {
		return DefaultScopesForRoles(roles)
	}
	return granted
}

// hasScope returns true if target scope is present.
func hasScope(scopes []string, target string) bool {
	for _, s := range scopes {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}

// deriveTokenID produces a stable token identifier from claims.
func deriveTokenID(claims *auth.Claims) string {
	return fmt.Sprintf("%s:%d", claims.Subject, claims.IssuedAt)
}

// mapAuthErrorToProto converts standard errors to proto error codes.
func mapAuthErrorToProto(err error) authv1.TokenErrorCode {
	switch {
	case errors.Is(err, auth.ErrTokenExpired):
		return authv1.TokenErrorCode_TOKEN_EXPIRED
	case errors.Is(err, auth.ErrInvalidSignature):
		return authv1.TokenErrorCode_TOKEN_INVALID_SIG
	default:
		return authv1.TokenErrorCode_TOKEN_MALFORMED
	}
}
