package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"wallet-system/pkg/auth"
)

type contextKey string

const ClaimsContextKey contextKey = "jwt_claims"

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

// AuthMiddleware validates JWT Bearer tokens and rejects unauthenticated requests.
// Public paths bypass token validation.
func AuthMiddleware(secret string, publicPaths map[string]bool, next http.Handler) http.Handler {
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

		claims, err := auth.ValidateToken(tokenStr, secret)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, fmt.Sprintf("Invalid token: %v", err))
			return
		}

		ctx := ContextWithClaims(r.Context(), claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
