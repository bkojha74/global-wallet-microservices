package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"wallet-system/pkg/auth"
	authv1 "wallet-system/proto/auth"
)

type contextKey string

const ClaimsContextKey contextKey = "jwt_claims"

// ── Token Validation Cache ────────────────────────────────────────────────────
// claimsCacheEntry stores a validated set of claims with an expiry timestamp.
type claimsCacheEntry struct {
	claims    *auth.Claims
	expiresAt time.Time
}

// claimsCache is a short-lived in-process cache for token validation responses.
// It reduces auth-service gRPC calls on hot paths without adding a Redis dependency.
// Cache TTL is 30 seconds; entries are also bounded by the token's own expiry.
type claimsCache struct {
	mu      sync.RWMutex
	entries map[string]claimsCacheEntry
	secret  string // HMAC key used only for cache-key fingerprinting
}

func newClaimsCache(fingerprintSecret string) *claimsCache {
	c := &claimsCache{
		entries: make(map[string]claimsCacheEntry),
		secret:  fingerprintSecret,
	}
	// Background cleanup of stale entries every 2 minutes
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			c.mu.Lock()
			now := time.Now()
			for k, v := range c.entries {
				if now.After(v.expiresAt) {
					delete(c.entries, k)
				}
			}
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *claimsCache) key(token string) string {
	mac := hmac.New(sha256.New, []byte(c.secret))
	mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (c *claimsCache) get(token string) (*auth.Claims, bool) {
	k := c.key(token)
	c.mu.RLock()
	entry, ok := c.entries[k]
	c.mu.RUnlock()
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.claims, true
}

func (c *claimsCache) set(token string, claims *auth.Claims) {
	ttl := 30 * time.Second
	if claims.ExpiresAt > 0 {
		tokenExpiry := time.Unix(claims.ExpiresAt, 0)
		if tokenExpiry.Before(time.Now().Add(ttl)) {
			ttl = time.Until(tokenExpiry)
		}
	}
	if ttl <= 0 {
		return
	}
	k := c.key(token)
	c.mu.Lock()
	c.entries[k] = claimsCacheEntry{claims: claims, expiresAt: time.Now().Add(ttl)}
	c.mu.Unlock()
}

func (c *claimsCache) invalidate(token string) {
	k := c.key(token)
	c.mu.Lock()
	delete(c.entries, k)
	c.mu.Unlock()
}

// ContextWithClaims stores the verified JWT claims into the request context.
func ContextWithClaims(ctx context.Context, claims *auth.Claims) context.Context {
	return context.WithValue(ctx, ClaimsContextKey, claims)
}

// ClaimsFromContext retrieves JWT claims from the request context.
func ClaimsFromContext(ctx context.Context) (*auth.Claims, bool) {
	claims, ok := ctx.Value(ClaimsContextKey).(*auth.Claims)
	return claims, ok
}

// SecurityHeadersMiddleware adds industry-standard security headers to every HTTP response.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// MaxBytesMiddleware wraps the request body to enforce a maximum payload size.
func MaxBytesMiddleware(maxBytes int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// CORSMiddleware manages cross-origin resource sharing headers and OPTIONS preflights.
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Association-ID, X-Idempotency-Key")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientBucket tracks token bucket rate limiting state for a client.
type clientBucket struct {
	tokens   float64
	lastSeen time.Time
}

// RateLimiter implements a clean token-bucket rate limiter per IP address.
type RateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64 // max tokens
	clients map[string]*clientBucket
}

// NewRateLimiter creates a RateLimiter with the specified replenishment rate and burst capacity.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	limiter := &RateLimiter{
		rate:    rate,
		burst:   float64(burst),
		clients: make(map[string]*clientBucket),
	}

	// Periodic cleanup of stale clients (inactive > 5 minutes)
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			limiter.mu.Lock()
			cutoff := time.Now().Add(-5 * time.Minute)
			for ip, b := range limiter.clients {
				if b.lastSeen.Before(cutoff) {
					delete(limiter.clients, ip)
				}
			}
			limiter.mu.Unlock()
		}
	}()

	return limiter
}

// Allow returns true if the client IP is within rate limits.
func (rl *RateLimiter) Allow(clientIP string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.clients[clientIP]
	if !exists {
		rl.clients[clientIP] = &clientBucket{
			tokens:   rl.burst - 1,
			lastSeen: now,
		}
		return true
	}

	// Replenish tokens based on elapsed time
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.lastSeen = now
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true
	}

	return false
}

// RateLimitMiddleware enforces token-bucket limits per remote client IP.
func RateLimitMiddleware(limiter *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := extractClientIP(r)
		if !limiter.Allow(ip) {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "Too many requests. Rate limit exceeded.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// AuthMiddleware validates JWT Bearer tokens by delegating to the auth-service via gRPC.
// Results are cached locally for 30 seconds to avoid an RPC on every protected request.
// Public paths bypass token validation entirely.
func AuthMiddleware(authClient authv1.AuthServiceClient, cache *claimsCache, publicPaths map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if publicPaths[path] {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		tokenStr, err := auth.ExtractBearerToken(authHeader)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, fmt.Sprintf("Authentication required: %v", err))
			return
		}

		// Fast path: check local cache before making an RPC
		if cached, ok := cache.get(tokenStr); ok {
			ctx := ContextWithClaims(r.Context(), cached)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Slow path: delegate to auth-service gRPC
		validateCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		resp, err := authClient.ValidateToken(validateCtx, &authv1.ValidateTokenRequest{Token: tokenStr})
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "Authentication service unavailable")
			return
		}
		if !resp.Valid {
			msg := tokenErrorMessage(resp.Error)
			writeAuthError(w, http.StatusUnauthorized, fmt.Sprintf("Invalid token: %s", msg))
			return
		}

		claims := &auth.Claims{
			Subject:   resp.Subject,
			Roles:     resp.Roles,
			Scopes:    resp.Scopes,
			ExpiresAt: resp.ExpiresAt,
		}
		cache.set(tokenStr, claims)

		ctx := ContextWithClaims(r.Context(), claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tokenErrorMessage converts a proto error code to a human-readable string.
func tokenErrorMessage(code authv1.TokenErrorCode) string {
	switch code {
	case authv1.TokenErrorCode_TOKEN_EXPIRED:
		return "token has expired"
	case authv1.TokenErrorCode_TOKEN_REVOKED:
		return "token has been revoked"
	case authv1.TokenErrorCode_TOKEN_INVALID_SIG:
		return "token signature is invalid"
	default:
		return "malformed token"
	}
}

// RequireRole checks that the authenticated claims have the specified role.
func RequireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFromContext(r.Context())
		if !ok || claims == nil || !claims.HasRole(role) {
			writeAuthError(w, http.StatusForbidden, fmt.Sprintf("Access forbidden: requires %s role", role))
			return
		}
		next(w, r)
	}
}

// RequireScope checks that the authenticated claims have the specified scope or admin role.
func RequireScope(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFromContext(r.Context())
		if !ok || claims == nil || !claims.HasScope(scope) {
			writeAuthError(w, http.StatusForbidden, fmt.Sprintf("Access forbidden: requires %s permission", scope))
			return
		}
		next(w, r)
	}
}

// ValidateWalletOwnership verifies that the token's subject owns the specified wallet or is an admin.
func ValidateWalletOwnership(claims *auth.Claims, walletID string) error {
	if claims == nil {
		return auth.ErrInsufficientPerms
	}
	if claims.HasRole(auth.RoleAdmin) {
		return nil
	}
	if claims.Subject != walletID {
		return auth.ErrIDORViolation
	}
	return nil
}

func writeAuthError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}
