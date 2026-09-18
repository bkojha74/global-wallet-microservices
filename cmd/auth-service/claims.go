package main

import (
	"strings"
	"wallet-system/pkg/auth"
)

// UnifiedClaims represents normalized identity and permission attributes
// produced by any IdentityProvider (Local, Keycloak, Okta, etc.).
// This keeps downstream services completely decoupled from provider-specific formats.
type UnifiedClaims struct {
	Subject   string   `json:"sub"`
	Roles     []string `json:"roles"`
	Scopes    []string `json:"scopes"`
	ExpiresAt int64    `json:"exp"`
	IssuedAt  int64    `json:"iat"`
	TokenID   string   `json:"jti"`
	Issuer    string   `json:"iss"`
	Audience  string   `json:"aud"`
	Email     string   `json:"email,omitempty"`
	Provider  string   `json:"provider,omitempty"` // "local", "keycloak", etc.
}

// ToAuthClaims converts UnifiedClaims to the platform's pkg/auth.Claims
// for compatibility with internal helpers.
func (u *UnifiedClaims) ToAuthClaims() auth.Claims {
	return auth.Claims{
		Subject:   u.Subject,
		Roles:     u.Roles,
		Scopes:    u.Scopes,
		ExpiresAt: u.ExpiresAt,
		IssuedAt:  u.IssuedAt,
		Issuer:    u.Issuer,
		Audience:  u.Audience,
	}
}

// KeycloakRealmAccess maps Keycloak's realm_access claim.
type KeycloakRealmAccess struct {
	Roles []string `json:"roles"`
}

// KeycloakResourceAccess maps Keycloak's resource_access claim.
type KeycloakResourceAccess struct {
	Roles []string `json:"roles"`
}

// KeycloakTokenClaims mirrors standard claims issued by a Keycloak OIDC realm.
type KeycloakTokenClaims struct {
	Subject           string                            `json:"sub"`
	PreferredUsername string                            `json:"preferred_username"`
	Email             string                            `json:"email"`
	EmailVerified     bool                              `json:"email_verified"`
	Name              string                            `json:"name"`
	GivenName         string                            `json:"given_name"`
	FamilyName        string                            `json:"family_name"`
	Issuer            string                            `json:"iss"`
	Audience          interface{}                       `json:"aud"` // can be string or []string in OIDC
	ExpiresAt         int64                             `json:"exp"`
	IssuedAt          int64                             `json:"iat"`
	NotBefore         int64                             `json:"nbf"`
	TokenID           string                            `json:"jti"`
	Scope             string                            `json:"scope"`
	RealmAccess       KeycloakRealmAccess               `json:"realm_access"`
	ResourceAccess    map[string]KeycloakResourceAccess `json:"resource_access"`
}

// MapKeycloakClaims transforms raw Keycloak claims into normalized UnifiedClaims.
// It extracts:
// 1. Subject: PreferredUsername if present (e.g. "alice"), falling back to sub UUID.
// 2. Roles: Combines realm_access roles and specific client resource_access roles.
// 3. Scopes: Parses space-separated OIDC scope string.
// 4. TokenID: Extracts jti (or generates a fallback).
func MapKeycloakClaims(kc *KeycloakTokenClaims, clientID string) *UnifiedClaims {
	subject := kc.PreferredUsername
	if subject == "" {
		subject = kc.Subject
	}

	// Aggregate roles with deduplication
	roleMap := make(map[string]struct{})
	for _, r := range kc.RealmAccess.Roles {
		rClean := strings.TrimSpace(r)
		if rClean != "" && !isStandardKeycloakNoiseRole(rClean) {
			roleMap[rClean] = struct{}{}
		}
	}

	// Also pull client-specific roles if target clientID is configured
	if clientID != "" && kc.ResourceAccess != nil {
		if clientRes, ok := kc.ResourceAccess[clientID]; ok {
			for _, r := range clientRes.Roles {
				rClean := strings.TrimSpace(r)
				if rClean != "" {
					roleMap[rClean] = struct{}{}
				}
			}
		}
	}

	roles := make([]string, 0, len(roleMap))
	for r := range roleMap {
		roles = append(roles, r)
	}

	// Parse space-delimited scopes
	scopeList := strings.Fields(kc.Scope)
	scopeMap := make(map[string]struct{})
	for _, s := range scopeList {
		sClean := strings.TrimSpace(s)
		if sClean != "" && !isStandardOIDCNoiseScope(sClean) {
			scopeMap[sClean] = struct{}{}
		}
	}
	scopes := make([]string, 0, len(scopeMap))
	for s := range scopeMap {
		scopes = append(scopes, s)
	}

	// Normalize audience
	var audStr string
	switch v := kc.Audience.(type) {
	case string:
		audStr = v
	case []interface{}:
		var auds []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				auds = append(auds, s)
			}
		}
		audStr = strings.Join(auds, ",")
	}

	return &UnifiedClaims{
		Subject:   subject,
		Roles:     roles,
		Scopes:    scopes,
		ExpiresAt: kc.ExpiresAt,
		IssuedAt:  kc.IssuedAt,
		TokenID:   kc.TokenID,
		Issuer:    kc.Issuer,
		Audience:  audStr,
		Email:     kc.Email,
		Provider:  "keycloak",
	}
}

// isStandardKeycloakNoiseRole filters out internal Keycloak maintenance roles.
func isStandardKeycloakNoiseRole(role string) bool {
	switch role {
	case "offline_access", "uma_authorization", "default-roles-wallet-realm":
		return true
	default:
		return false
	}
}

// isStandardOIDCNoiseScope filters standard base OIDC scopes to retain application scopes.
func isStandardOIDCNoiseScope(scope string) bool {
	switch scope {
	case "openid", "profile", "email":
		return true
	default:
		return false
	}
}
