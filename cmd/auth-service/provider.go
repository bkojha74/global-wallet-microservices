package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
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

// KeycloakProvider validates tokens using an external OIDC provider's JWKS and normalizes claims.
// It is general enough to support Keycloak, Google Identity, Okta, or any OpenID Connect compliant provider.
type KeycloakProvider struct {
	mu           sync.RWMutex
	oidcClient   *OIDCClient
	jwksCache    *JWKSCache
	clientID     string
	clientSecret string
	issuer       string
	explicitJWKS string
	isHealthy    bool
	stopPoller   chan struct{}
}

// NewKeycloakProvider creates a new KeycloakProvider / OIDC provider.
func NewKeycloakProvider(oidcClient *OIDCClient, jwksCache *JWKSCache, clientID, issuer string) *KeycloakProvider {
	healthy := (oidcClient != nil)
	return &KeycloakProvider{
		oidcClient: oidcClient,
		jwksCache:  jwksCache,
		clientID:   clientID,
		issuer:     strings.TrimRight(issuer, "/"),
		isHealthy:  healthy,
		stopPoller: make(chan struct{}),
	}
}

func (p *KeycloakProvider) Name() string {
	return "keycloak"
}

// IsHealthy returns true if the external OIDC provider is currently connected and responsive.
func (p *KeycloakProvider) IsHealthy() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.isHealthy
}

// SetConfig sets parameters needed for background self-healing reconnection.
func (p *KeycloakProvider) SetConfig(clientSecret, explicitJWKS string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clientSecret = clientSecret
	p.explicitJWKS = explicitJWKS
}

// Ping checks if the remote OIDC provider is reachable and responsive.
func (p *KeycloakProvider) Ping(ctx context.Context) error {
	p.mu.RLock()
	issuer := p.issuer
	p.mu.RUnlock()

	if issuer == "" {
		return errors.New("oidc: issuer URL is empty")
	}

	discoveryURL := fmt.Sprintf("%s/.well-known/openid-configuration", strings.TrimRight(issuer, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: ping returned status %d", resp.StatusCode)
	}
	return nil
}

// Reconnect attempts to rediscover OIDC endpoints and refresh JWKS keys.
func (p *KeycloakProvider) Reconnect(ctx context.Context) error {
	p.mu.RLock()
	issuer := p.issuer
	clientID := p.clientID
	clientSecret := p.clientSecret
	explicitJWKS := p.explicitJWKS
	p.mu.RUnlock()

	client, err := NewOIDCClient(ctx, issuer, clientID, clientSecret, explicitJWKS)
	if err != nil {
		p.mu.Lock()
		p.isHealthy = false
		p.mu.Unlock()
		return fmt.Errorf("oidc reconnect failed: %w", err)
	}

	jwksURL := explicitJWKS
	if jwksURL == "" && client != nil {
		jwksURL = client.JwksURI()
	}
	if jwksURL == "" {
		jwksURL = fmt.Sprintf("%s/protocol/openid-connect/certs", strings.TrimRight(issuer, "/"))
	}

	jwksCache := NewJWKSCache(jwksURL, 1*time.Hour)
	_ = jwksCache.Refresh(ctx)

	p.mu.Lock()
	p.oidcClient = client
	p.jwksCache = jwksCache
	p.isHealthy = true
	p.mu.Unlock()

	log.Printf("[OIDC-RECOVERY] Successfully reconnected to OIDC provider: %s", issuer)
	return nil
}

// StartHealthPoller starts an asynchronous background health check / reconnect poller.
// If Keycloak/OIDC is down, it pings and reconnects as soon as it recovers.
func (p *KeycloakProvider) StartHealthPoller(interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-p.stopPoller:
				return
			case <-ticker.C:
				p.mu.RLock()
				healthy := p.isHealthy
				p.mu.RUnlock()

				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				err := p.Ping(ctx)
				cancel()

				if err != nil {
					if healthy {
						log.Printf("[OIDC-HEALTH] Provider %s ping failed (%v); marking degraded", p.issuer, err)
						p.mu.Lock()
						p.isHealthy = false
						p.mu.Unlock()
					}
				} else {
					if !healthy {
						log.Printf("[OIDC-HEALTH] Provider %s is now reachable! Reconnecting...", p.issuer)
						reconCtx, reconCancel := context.WithTimeout(context.Background(), 5*time.Second)
						reconErr := p.Reconnect(reconCtx)
						if reconErr != nil {
							log.Printf("[OIDC-HEALTH] Reconnect attempt failed: %v", reconErr)
						} else {
							log.Printf("[OIDC-HEALTH] Reconnect succeeded! Healthy is now true")
						}
						reconCancel()
					}
				}
			}
		}
	}()
}

// StopHealthPoller stops the background poller routine.
func (p *KeycloakProvider) StopHealthPoller() {
	select {
	case <-p.stopPoller:
		// already closed
	default:
		close(p.stopPoller)
	}
}

func (p *KeycloakProvider) Authenticate(ctx context.Context, username, password string, requestedScopes []string) (*UnifiedClaims, *TokenPair, error) {
	p.mu.RLock()
	client := p.oidcClient
	healthy := p.isHealthy
	p.mu.RUnlock()

	if client == nil || !healthy {
		return nil, nil, errors.New("oidc: provider client unavailable or unhealthy")
	}

	resp, err := client.AuthenticatePassword(ctx, username, password, requestedScopes)
	if err != nil {
		return nil, nil, err
	}

	uClaims, err := p.ValidateToken(ctx, resp.AccessToken)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc: validating issued access token: %w", err)
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
	p.mu.RLock()
	cache := p.jwksCache
	issuer := p.issuer
	clientID := p.clientID
	p.mu.RUnlock()

	if cache == nil {
		return nil, errors.New("oidc: JWKS cache unavailable")
	}

	kcClaims, err := cache.VerifyToken(ctx, tokenStr)
	if err != nil {
		return nil, err
	}

	// Validate issuer if configured
	if issuer != "" && strings.TrimRight(kcClaims.Issuer, "/") != issuer {
		return nil, fmt.Errorf("oidc: token issuer %q does not match configured issuer %q", kcClaims.Issuer, issuer)
	}

	uClaims := MapKeycloakClaims(kcClaims, clientID)
	return uClaims, nil
}

func (p *KeycloakProvider) RefreshToken(ctx context.Context, refreshToken string) (*UnifiedClaims, *TokenPair, error) {
	p.mu.RLock()
	client := p.oidcClient
	healthy := p.isHealthy
	p.mu.RUnlock()

	if client == nil || !healthy {
		return nil, nil, errors.New("oidc: provider client unavailable or unhealthy")
	}

	resp, err := client.RefreshToken(ctx, refreshToken)
	if err != nil {
		return nil, nil, err
	}

	uClaims, err := p.ValidateToken(ctx, resp.AccessToken)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc: validating refreshed access token: %w", err)
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

// HybridProvider enables simultaneous support for internal local tokens and external OIDC tokens.
type HybridProvider struct {
	mu       sync.RWMutex
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

// SetExternalProvider dynamically replaces or updates the external provider.
func (h *HybridProvider) SetExternalProvider(external *KeycloakProvider) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.external = external
}

func (h *HybridProvider) Authenticate(ctx context.Context, username, password string, requestedScopes []string) (*UnifiedClaims, *TokenPair, error) {
	h.mu.RLock()
	ext := h.external
	h.mu.RUnlock()

	// First attempt external provider (e.g. Keycloak / Google OIDC) when healthy
	if ext != nil && ext.IsHealthy() {
		extClaims, extPair, extErr := ext.Authenticate(ctx, username, password, requestedScopes)
		if extErr == nil {
			return extClaims, extPair, nil
		}
	}

	// Fallback to local user store authentication
	return h.local.Authenticate(ctx, username, password, requestedScopes)
}

func (h *HybridProvider) ValidateToken(ctx context.Context, tokenStr string) (*UnifiedClaims, error) {
	h.mu.RLock()
	ext := h.external
	h.mu.RUnlock()

	issuer, _ := peekTokenMetadata(tokenStr)

	// If token issuer matches external realm or starts with external issuer URL, route to external first
	if ext != nil && ext.issuer != "" && strings.HasPrefix(issuer, ext.issuer) {
		claims, err := ext.ValidateToken(ctx, tokenStr)
		if err == nil {
			return claims, nil
		}
		log.Printf("[HYBRID-AUTH] External provider validation failed: %v, falling back to local...", err)
	}

	// Try local provider
	claims, err := h.local.ValidateToken(ctx, tokenStr)
	if err == nil {
		return claims, nil
	}

	// If local failed and we have not attempted external yet, try external as fallback
	if ext != nil && (ext.issuer == "" || !strings.HasPrefix(issuer, ext.issuer)) {
		extClaims, extErr := ext.ValidateToken(ctx, tokenStr)
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

	h.mu.RLock()
	ext := h.external
	h.mu.RUnlock()

	// Fallback to external if configured and healthy
	if ext != nil && ext.IsHealthy() {
		extClaims, extPair, extErr := ext.RefreshToken(ctx, refreshToken)
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
