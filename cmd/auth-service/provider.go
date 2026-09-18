package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"wallet-system/pkg/auth"
)

// TokenPair represents an issued access + refresh token set.
type TokenPair struct {
	AccessToken   string
	RefreshToken  string
	TokenType     string
	ExpiresIn     int64
	GrantedScopes []string
}

// IdentityProvider abstracts identity verification and token operations.
// Implementations include LocalProvider (MongoDB + internal TokenEngine),
// KeycloakProvider (OIDC + JWKS), and HybridProvider (federated multi-provider).
type IdentityProvider interface {
	Name() string
	Authenticate(ctx context.Context, username, password string, requestedScopes []string) (*UnifiedClaims, *TokenPair, error)
	ValidateToken(ctx context.Context, tokenStr string) (*UnifiedClaims, error)
	RefreshToken(ctx context.Context, refreshToken string) (*UnifiedClaims, *TokenPair, error)
}

// ── LocalProvider ────────────────────────────────────────────────────────────

// LocalProvider authenticates against internal MongoDB and signs tokens using TokenEngine.
type LocalProvider struct {
	store    *UserStore
	engine   *TokenEngine
	revStore *RevocationStore
}

// NewLocalProvider creates a new LocalProvider instance.
func NewLocalProvider(store *UserStore, engine *TokenEngine, revStore *RevocationStore) *LocalProvider {
	return &LocalProvider{
		store:    store,
		engine:   engine,
		revStore: revStore,
	}
}

func (p *LocalProvider) Name() string {
	return "local"
}

func (p *LocalProvider) Authenticate(ctx context.Context, username, password string, requestedScopes []string) (*UnifiedClaims, *TokenPair, error) {
	user, err := p.store.Authenticate(ctx, username, password)
	if err != nil {
		return nil, nil, err
	}

	grantedScopes := resolveScopes(user.Roles, user.Scopes, requestedScopes)
	claims := auth.Claims{
		Subject:  user.Username,
		Roles:    user.Roles,
		Scopes:   grantedScopes,
		Issuer:   auth.DefaultIssuer,
		Audience: auth.DefaultAudience,
	}

	accessToken, err := p.engine.IssueAccessToken(claims)
	if err != nil {
		return nil, nil, fmt.Errorf("local: access token generation failed: %w", err)
	}

	refreshToken, err := p.engine.IssueRefreshToken()
	if err != nil {
		return nil, nil, fmt.Errorf("local: refresh token generation failed: %w", err)
	}

	if err := p.store.StoreRefreshToken(ctx, refreshToken, user.Username, user.Roles, grantedScopes, refreshTokenTTL); err != nil {
		return nil, nil, fmt.Errorf("local: failed to store refresh token: %w", err)
	}

	uClaims := &UnifiedClaims{
		Subject:   user.Username,
		Roles:     user.Roles,
		Scopes:    grantedScopes,
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute).Unix(),
		IssuedAt:  time.Now().UTC().Unix(),
		Issuer:    auth.DefaultIssuer,
		Audience:  auth.DefaultAudience,
		Provider:  "local",
	}

	return uClaims, &TokenPair{
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		TokenType:     "Bearer",
		ExpiresIn:     int64((15 * time.Minute).Seconds()),
		GrantedScopes: grantedScopes,
	}, nil
}

func (p *LocalProvider) ValidateToken(ctx context.Context, tokenStr string) (*UnifiedClaims, error) {
	claims, err := p.engine.ValidateAccessToken(tokenStr)
	if err != nil {
		return nil, err
	}

	tokenID := deriveTokenID(claims)
	return &UnifiedClaims{
		Subject:   claims.Subject,
		Roles:     claims.Roles,
		Scopes:    claims.Scopes,
		ExpiresAt: claims.ExpiresAt,
		IssuedAt:  claims.IssuedAt,
		TokenID:   tokenID,
		Issuer:    claims.Issuer,
		Audience:  claims.Audience,
		Provider:  "local",
	}, nil
}

func (p *LocalProvider) RefreshToken(ctx context.Context, refreshToken string) (*UnifiedClaims, *TokenPair, error) {
	rec, err := p.store.FindRefreshToken(ctx, refreshToken)
	if err != nil {
		return nil, nil, err
	}

	claims := auth.Claims{
		Subject:  rec.Subject,
		Roles:    rec.Roles,
		Scopes:   rec.Scopes,
		Issuer:   auth.DefaultIssuer,
		Audience: auth.DefaultAudience,
	}

	accessToken, err := p.engine.IssueAccessToken(claims)
	if err != nil {
		return nil, nil, fmt.Errorf("local: token generation failed: %w", err)
	}

	newRefreshToken, err := p.engine.IssueRefreshToken()
	if err != nil {
		return nil, nil, fmt.Errorf("local: refresh token generation failed: %w", err)
	}

	_ = p.store.RevokeRefreshToken(ctx, refreshToken)
	if err := p.store.StoreRefreshToken(ctx, newRefreshToken, rec.Subject, rec.Roles, rec.Scopes, refreshTokenTTL); err != nil {
		return nil, nil, fmt.Errorf("local: session rotation failed: %w", err)
	}

	uClaims := &UnifiedClaims{
		Subject:   rec.Subject,
		Roles:     rec.Roles,
		Scopes:    rec.Scopes,
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute).Unix(),
		IssuedAt:  time.Now().UTC().Unix(),
		Issuer:    auth.DefaultIssuer,
		Audience:  auth.DefaultAudience,
		Provider:  "local",
	}

	return uClaims, &TokenPair{
		AccessToken:   accessToken,
		RefreshToken:  newRefreshToken,
		TokenType:     "Bearer",
		ExpiresIn:     int64((15 * time.Minute).Seconds()),
		GrantedScopes: rec.Scopes,
	}, nil
}

// ── KeycloakProvider ─────────────────────────────────────────────────────────

// KeycloakProvider validates tokens using Keycloak's JWKS and normalizes Keycloak claims.
type KeycloakProvider struct {
	oidcClient *OIDCClient
	jwksCache  *JWKSCache
	clientID   string
	issuer     string
}

// NewKeycloakProvider creates a new KeycloakProvider.
func NewKeycloakProvider(oidcClient *OIDCClient, jwksCache *JWKSCache, clientID, issuer string) *KeycloakProvider {
	return &KeycloakProvider{
		oidcClient: oidcClient,
		jwksCache:  jwksCache,
		clientID:   clientID,
		issuer:     strings.TrimRight(issuer, "/"),
	}
}

func (p *KeycloakProvider) Name() string {
	return "keycloak"
}

func (p *KeycloakProvider) Authenticate(ctx context.Context, username, password string, requestedScopes []string) (*UnifiedClaims, *TokenPair, error) {
	if p.oidcClient == nil {
		return nil, nil, errors.New("keycloak: OIDC client not configured for login")
	}

	resp, err := p.oidcClient.AuthenticatePassword(ctx, username, password, requestedScopes)
	if err != nil {
		return nil, nil, err
	}

	uClaims, err := p.ValidateToken(ctx, resp.AccessToken)
	if err != nil {
		return nil, nil, fmt.Errorf("keycloak: validating issued access token: %w", err)
	}

	return uClaims, &TokenPair{
		AccessToken:   resp.AccessToken,
		RefreshToken:  resp.RefreshToken,
		TokenType:     resp.TokenType,
		ExpiresIn:     resp.ExpiresIn,
		GrantedScopes: uClaims.Scopes,
	}, nil
}

func (p *KeycloakProvider) ValidateToken(ctx context.Context, tokenStr string) (*UnifiedClaims, error) {
	kcClaims, err := p.jwksCache.VerifyToken(ctx, tokenStr)
	if err != nil {
		return nil, err
	}

	// Validate issuer if configured
	if p.issuer != "" && strings.TrimRight(kcClaims.Issuer, "/") != p.issuer {
		return nil, fmt.Errorf("keycloak: token issuer %q does not match configured issuer %q", kcClaims.Issuer, p.issuer)
	}

	uClaims := MapKeycloakClaims(kcClaims, p.clientID)
	return uClaims, nil
}

func (p *KeycloakProvider) RefreshToken(ctx context.Context, refreshToken string) (*UnifiedClaims, *TokenPair, error) {
	if p.oidcClient == nil {
		return nil, nil, errors.New("keycloak: OIDC client not configured for refresh")
	}

	resp, err := p.oidcClient.RefreshToken(ctx, refreshToken)
	if err != nil {
		return nil, nil, err
	}

	uClaims, err := p.ValidateToken(ctx, resp.AccessToken)
	if err != nil {
		return nil, nil, fmt.Errorf("keycloak: validating refreshed access token: %w", err)
	}

	return uClaims, &TokenPair{
		AccessToken:   resp.AccessToken,
		RefreshToken:  resp.RefreshToken,
		TokenType:     resp.TokenType,
		ExpiresIn:     resp.ExpiresIn,
		GrantedScopes: uClaims.Scopes,
	}, nil
}

// ── HybridProvider ───────────────────────────────────────────────────────────

// HybridProvider enables simultaneous support for internal local tokens and external Keycloak tokens.
type HybridProvider struct {
	local    *LocalProvider
	external *KeycloakProvider
}

// NewHybridProvider initializes a HybridProvider with both local and external providers.
func NewHybridProvider(local *LocalProvider, external *KeycloakProvider) *HybridProvider {
	return &HybridProvider{
		local:    local,
		external: external,
	}
}

func (h *HybridProvider) Name() string {
	return "hybrid"
}

func (h *HybridProvider) Authenticate(ctx context.Context, username, password string, requestedScopes []string) (*UnifiedClaims, *TokenPair, error) {
	// First attempt local user store authentication
	uClaims, pair, err := h.local.Authenticate(ctx, username, password, requestedScopes)
	if err == nil {
		return uClaims, pair, nil
	}

	// If local authentication failed and external provider is configured, attempt Keycloak Direct Grant
	if h.external != nil {
		extClaims, extPair, extErr := h.external.Authenticate(ctx, username, password, requestedScopes)
		if extErr == nil {
			return extClaims, extPair, nil
		}
	}

	return nil, nil, err
}

func (h *HybridProvider) ValidateToken(ctx context.Context, tokenStr string) (*UnifiedClaims, error) {
	issuer, _ := peekTokenMetadata(tokenStr)

	// If token issuer matches Keycloak realm or starts with external issuer URL, route to Keycloak first
	if h.external != nil && h.external.issuer != "" && strings.HasPrefix(issuer, h.external.issuer) {
		claims, err := h.external.ValidateToken(ctx, tokenStr)
		if err == nil {
			return claims, nil
		}
		log.Printf("[HYBRID-AUTH] Keycloak validation failed: %v, falling back to local...", err)
	}

	// Try local provider
	claims, err := h.local.ValidateToken(ctx, tokenStr)
	if err == nil {
		return claims, nil
	}

	// If local failed and we have not attempted external yet, try external as fallback
	if h.external != nil && (h.external.issuer == "" || !strings.HasPrefix(issuer, h.external.issuer)) {
		extClaims, extErr := h.external.ValidateToken(ctx, tokenStr)
		if extErr == nil {
			return extClaims, nil
		}
	}

	return nil, err
}

func (h *HybridProvider) RefreshToken(ctx context.Context, refreshToken string) (*UnifiedClaims, *TokenPair, error) {
	// Try local store first
	claims, pair, err := h.local.RefreshToken(ctx, refreshToken)
	if err == nil {
		return claims, pair, nil
	}

	// Fallback to external if configured
	if h.external != nil {
		extClaims, extPair, extErr := h.external.RefreshToken(ctx, refreshToken)
		if extErr == nil {
			return extClaims, extPair, nil
		}
	}

	return nil, nil, err
}

// peekTokenMetadata extracts unverified "iss" and "kid" from a JWT string without verifying signatures.
func peekTokenMetadata(tokenStr string) (issuer, kid string) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return "", ""
	}

	// Header
	if hBytes, err := decodeBase64URL(parts[0]); err == nil {
		var hdr tokenHeader
		if json.Unmarshal(hBytes, &hdr) == nil {
			kid = hdr.Kid
		}
	}

	// Claims
	if cBytes, err := decodeBase64URL(parts[1]); err == nil {
		var minimal struct {
			Issuer string `json:"iss"`
		}
		if json.Unmarshal(cBytes, &minimal) == nil {
			issuer = minimal.Issuer
		}
	}

	return issuer, kid
}
