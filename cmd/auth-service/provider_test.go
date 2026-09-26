package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wallet-system/pkg/auth"
	"wallet-system/pkg/db"
	authv1 "wallet-system/proto/auth"
)

func TestMapKeycloakClaims(t *testing.T) {
	kc := &KeycloakTokenClaims{
		Subject:           "user-uuid-12345",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Issuer:            "http://keycloak:8080/realms/wallet-realm",
		Audience:          "wallet-api",
		ExpiresAt:         time.Now().Add(1 * time.Hour).Unix(),
		IssuedAt:          time.Now().Unix(),
		TokenID:           "jti-9876",
		Scope:             "openid profile email wallet:read wallet:transfer",
		RealmAccess: KeycloakRealmAccess{
			Roles: []string{"user", "offline_access", "uma_authorization"},
		},
		ResourceAccess: map[string]KeycloakResourceAccess{
			"wallet-api": {
				Roles: []string{"wallet-manager", "wallet:transfer"},
			},
			"account": {
				Roles: []string{"view-profile"},
			},
		},
	}

	unified := MapKeycloakClaims(kc, "wallet-api")

	if unified.Subject != "alice" {
		t.Errorf("expected Subject 'alice', got %q", unified.Subject)
	}
	if unified.TokenID != "jti-9876" {
		t.Errorf("expected TokenID 'jti-9876', got %q", unified.TokenID)
	}
	if unified.Email != "alice@example.com" {
		t.Errorf("expected Email 'alice@example.com', got %q", unified.Email)
	}

	// Roles check: "user", "wallet-manager", "wallet:transfer" should be present; "offline_access" filtered
	roleMap := make(map[string]bool)
	for _, r := range unified.Roles {
		roleMap[r] = true
	}
	if !roleMap["user"] {
		t.Error("expected role 'user' to be present")
	}
	if !roleMap["wallet-manager"] {
		t.Error("expected client role 'wallet-manager' to be present")
	}
	if roleMap["offline_access"] {
		t.Error("expected noise role 'offline_access' to be filtered out")
	}

	// Scopes check: "wallet:read" and "wallet:transfer" should be present; "openid" filtered
	scopeMap := make(map[string]bool)
	for _, s := range unified.Scopes {
		scopeMap[s] = true
	}
	if !scopeMap["wallet:read"] || !scopeMap["wallet:transfer"] {
		t.Error("expected application scopes 'wallet:read' and 'wallet:transfer' to be present")
	}
	if scopeMap["openid"] || scopeMap["profile"] {
		t.Error("expected noise scopes 'openid' and 'profile' to be filtered out")
	}
}

func TestJWKSCache_VerifyToken(t *testing.T) {
	// 1. Generate RSA key pair
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	pubKey := &privKey.PublicKey

	// Encode modulus and exponent to base64url
	nB64 := base64.RawURLEncoding.EncodeToString(pubKey.N.Bytes())
	eBytes := []byte{1, 0, 1} // 65537
	eB64 := base64.RawURLEncoding.EncodeToString(eBytes)

	kid := "test-key-id-1"

	// 2. Start mock JWKS HTTP server
	jwksHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := JSONWebKeySet{
			Keys: []JSONWebKey{
				{
					Kty: "RSA",
					Kid: kid,
					Use: "sig",
					Alg: "RS256",
					N:   nB64,
					E:   eB64,
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	ts := httptest.NewServer(jwksHandler)
	defer ts.Close()

	// 3. Create JWKSCache targeting mock server
	cache := NewJWKSCache(ts.URL, 5*time.Minute)

	// 4. Forge a Keycloak RS256 token signed with private key
	kcClaims := KeycloakTokenClaims{
		Subject:           "user-kc-123",
		PreferredUsername: "bob",
		Issuer:            "http://mock-keycloak/realms/test",
		ExpiresAt:         time.Now().Add(10 * time.Minute).Unix(),
		IssuedAt:          time.Now().Unix(),
		TokenID:           "test-token-1",
		Scope:             "wallet:read",
		RealmAccess: KeycloakRealmAccess{
			Roles: []string{auth.RoleAdmin},
		},
	}

	h := tokenHeader{Alg: "RS256", Typ: "JWT", Kid: kid}
	hBytes, _ := json.Marshal(h)
	cBytes, _ := json.Marshal(kcClaims)
	hB64 := base64.RawURLEncoding.EncodeToString(hBytes)
	cB64 := base64.RawURLEncoding.EncodeToString(cBytes)
	signingInput := hB64 + "." + cB64

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign failed: %v", err)
	}
	tokenStr := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	// 5. Verify token with JWKSCache
	verifiedClaims, err := cache.VerifyToken(context.Background(), tokenStr)
	if err != nil {
		t.Fatalf("VerifyToken failed: %v", err)
	}

	if verifiedClaims.PreferredUsername != "bob" {
		t.Errorf("expected preferred_username 'bob', got %q", verifiedClaims.PreferredUsername)
	}
	if verifiedClaims.Subject != "user-kc-123" {
		t.Errorf("expected subject 'user-kc-123', got %q", verifiedClaims.Subject)
	}
}

func TestHybridProvider_TokenRouting(t *testing.T) {
	// 1. Setup local TokenEngine
	secret := "test-secret-key-1234567890"
	localEngine := NewHS256Engine(secret)

	localProvider := &LocalProvider{
		engine: localEngine,
	}

	// 2. Setup mock Keycloak RS256 provider
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	pubKey := &privKey.PublicKey
	nB64 := base64.RawURLEncoding.EncodeToString(pubKey.N.Bytes())
	eB64 := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
	kid := "kc-key-1"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JSONWebKeySet{
			Keys: []JSONWebKey{{Kty: "RSA", Kid: kid, Alg: "RS256", N: nB64, E: eB64}},
		})
	}))
	defer ts.Close()

	kcCache := NewJWKSCache(ts.URL, time.Hour)
	keycloakProvider := NewKeycloakProvider(nil, kcCache, "wallet-api", "http://keycloak:8080/realms/wallet-realm")

	// 3. Create HybridProvider
	hybrid := NewHybridProvider(localProvider, keycloakProvider)

	// Issue local HS256 token
	localToken, err := localEngine.IssueAccessToken(auth.Claims{
		Subject:   "local-alice",
		Roles:     []string{auth.RoleUser},
		Scopes:    []string{"wallet:read"},
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("issue local token failed: %v", err)
	}

	// Issue Keycloak RS256 token
	kcPayload := KeycloakTokenClaims{
		Subject:           "kc-user-uuid",
		PreferredUsername: "kc-bob",
		Issuer:            "http://keycloak:8080/realms/wallet-realm",
		ExpiresAt:         time.Now().Add(10 * time.Minute).Unix(),
		Scope:             "wallet:transfer",
		RealmAccess:       KeycloakRealmAccess{Roles: []string{auth.RoleAdmin}},
	}
	hBytes, _ := json.Marshal(tokenHeader{Alg: "RS256", Typ: "JWT", Kid: kid})
	cBytes, _ := json.Marshal(kcPayload)
	signingInput := base64.RawURLEncoding.EncodeToString(hBytes) + "." + base64.RawURLEncoding.EncodeToString(cBytes)
	digest := sha256.Sum256([]byte(signingInput))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, digest[:])
	kcToken := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	// 4. Validate local token through HybridProvider
	claims1, err := hybrid.ValidateToken(context.Background(), localToken)
	if err != nil {
		t.Fatalf("hybrid validation for local token failed: %v", err)
	}
	if claims1.Subject != "local-alice" {
		t.Errorf("expected subject 'local-alice', got %q", claims1.Subject)
	}

	// 5. Validate Keycloak token through HybridProvider
	claims2, err := hybrid.ValidateToken(context.Background(), kcToken)
	if err != nil {
		t.Fatalf("hybrid validation for Keycloak token failed: %v", err)
	}
	if claims2.Subject != "kc-bob" {
		t.Errorf("expected subject 'kc-bob', got %q", claims2.Subject)
	}
	if !hasScope(claims2.Scopes, "wallet:transfer") {
		t.Errorf("expected scope 'wallet:transfer' on kc token, got %v", claims2.Scopes)
	}
}

func TestNormalizeAudience(t *testing.T) {
	// String
	if aud := normalizeAudience("my-app"); aud != "my-app" {
		t.Fatalf("expected my-app, got %s", aud)
	}

	// Slice of interfaces
	list := []interface{}{"aud1", "aud2"}
	if aud := normalizeAudience(list); aud != "aud1,aud2" {
		t.Fatalf("expected aud1,aud2, got %s", aud)
	}

	// Unsupported type
	if aud := normalizeAudience(12345); aud != "" {
		t.Fatalf("expected empty string, got %s", aud)
	}
}

func TestStandardNoiseFilters(t *testing.T) {
	if !isStandardKeycloakNoiseRole("offline_access") {
		t.Fatal("expected offline_access to be noise role")
	}
	if !isStandardKeycloakNoiseRole("uma_authorization") {
		t.Fatal("expected uma_authorization to be noise role")
	}
	if isStandardKeycloakNoiseRole("admin") {
		t.Fatal("admin is not noise role")
	}

	if !isStandardOIDCNoiseScope("openid") || !isStandardOIDCNoiseScope("profile") || !isStandardOIDCNoiseScope("email") {
		t.Fatal("expected standard scopes to be noise")
	}
	if isStandardOIDCNoiseScope("wallet:read") {
		t.Fatal("wallet:read should not be noise")
	}
}

func TestAddClientRolesEdgeCases(t *testing.T) {
	roleMap := make(map[string]struct{})
	// Empty clientID
	addClientRoles(roleMap, nil, "")
	if len(roleMap) != 0 {
		t.Fatal("expected 0 roles")
	}

	// Missing clientID in resource access
	resAccess := map[string]KeycloakResourceAccess{
		"other-client": {Roles: []string{"admin"}},
	}
	addClientRoles(roleMap, resAccess, "my-client")
	if len(roleMap) != 0 {
		t.Fatal("expected 0 roles")
	}

	// Populated clientID with whitespace
	resAccess["my-client"] = KeycloakResourceAccess{Roles: []string{"  manager  ", ""}}
	addClientRoles(roleMap, resAccess, "my-client")
	if _, ok := roleMap["manager"]; !ok {
		t.Fatal("expected manager role in roleMap")
	}
}

func TestAuthServerValidationAndAuthorize(t *testing.T) {
	srv := &authServer{}

	// ValidateToken: missing token
	vResp, err := srv.ValidateToken(context.Background(), &authv1.ValidateTokenRequest{Token: ""})
	if err != nil || vResp.Valid {
		t.Fatalf("expected invalid token response, got valid=%v, err=%v", vResp.Valid, err)
	}

	// IssueToken: missing username/password
	_, err = srv.IssueToken(context.Background(), &authv1.IssueTokenRequest{})
	if err == nil {
		t.Fatal("expected error for empty credentials")
	}

	// RefreshToken: missing refresh token
	_, err = srv.RefreshToken(context.Background(), &authv1.RefreshTokenRequest{})
	if err == nil {
		t.Fatal("expected error for empty refresh token")
	}

	// Authorize: missing subject
	authResp, err := srv.Authorize(context.Background(), &authv1.AuthorizeRequest{Subject: ""})
	if err != nil || authResp.Allowed {
		t.Fatalf("expected disallowed for empty subject, got %+v", authResp)
	}

	// Authorize: admin role bypasses
	adminResp, err := srv.Authorize(context.Background(), &authv1.AuthorizeRequest{
		Subject: "admin-user",
		Roles:   []string{auth.RoleAdmin},
	})
	if err != nil || !adminResp.Allowed {
		t.Fatalf("expected allowed for admin, got %+v", adminResp)
	}

	// Authorize: cluster admin with and without scope
	clResp1, _ := srv.Authorize(context.Background(), &authv1.AuthorizeRequest{
		Subject:  "cluster-user",
		Resource: "cluster:failover",
		Scopes:   []string{auth.ScopeClusterAdmin},
	})
	if !clResp1.Allowed {
		t.Fatalf("expected cluster admin allowed, got %+v", clResp1)
	}

	clResp2, _ := srv.Authorize(context.Background(), &authv1.AuthorizeRequest{
		Subject:  "cluster-user",
		Resource: "cluster:failover",
		Scopes:   []string{"wallet:read"},
	})
	if clResp2.Allowed {
		t.Fatal("expected cluster admin disallowed without scope")
	}

	// Authorize: ledger audit with and without scope
	ledResp1, _ := srv.Authorize(context.Background(), &authv1.AuthorizeRequest{
		Subject:  "auditor",
		Resource: "ledger:entries",
		Scopes:   []string{auth.ScopeLedgerAudit},
	})
	if !ledResp1.Allowed {
		t.Fatalf("expected ledger audit allowed, got %+v", ledResp1)
	}

	ledResp2, _ := srv.Authorize(context.Background(), &authv1.AuthorizeRequest{
		Subject:  "auditor",
		Resource: "ledger:entries",
		Scopes:   []string{},
	})
	if ledResp2.Allowed {
		t.Fatal("expected ledger audit disallowed without scope")
	}

	// Authorize: unknown resource
	unkResp, _ := srv.Authorize(context.Background(), &authv1.AuthorizeRequest{
		Subject:  "user-1",
		Resource: "unknown:resource",
	})
	if unkResp.Allowed || unkResp.Reason != "no matching policy" {
		t.Fatalf("expected no matching policy, got %+v", unkResp)
	}

	// Authorize: wallet unsupported action
	wBadAction, _ := srv.Authorize(context.Background(), &authv1.AuthorizeRequest{
		Subject:  "alice",
		Resource: "wallet:w1",
		Action:   "delete",
	})
	if wBadAction.Allowed || wBadAction.Reason != "unsupported wallet action" {
		t.Fatalf("expected unsupported wallet action, got %+v", wBadAction)
	}

	// Authorize: wallet missing scope
	wMissingScope, _ := srv.Authorize(context.Background(), &authv1.AuthorizeRequest{
		Subject:  "alice",
		Resource: "wallet:w1",
		Action:   "read",
		Scopes:   []string{"wallet:transfer"},
	})
	if wMissingScope.Allowed {
		t.Fatal("expected wallet missing scope to be disallowed")
	}

	// Authorize: wallet with nil store (allowed)
	wAllowed, _ := srv.Authorize(context.Background(), &authv1.AuthorizeRequest{
		Subject:  "alice",
		Resource: "wallet:w1",
		Action:   "read",
		Scopes:   []string{"wallet:read"},
	})
	if !wAllowed.Allowed {
		t.Fatalf("expected wallet read to be allowed, got %+v", wAllowed)
	}

	// HealthCheck
	hResp, err := srv.HealthCheck(context.Background(), nil)
	if err != nil || hResp.Status != "SERVING" {
		t.Fatalf("expected SERVING status, got %+v, err=%v", hResp, err)
	}

	// resolveScopes helper
	scopesEmptyReq := resolveScopes([]string{auth.RoleUser}, []string{"wallet:read"}, nil)
	if len(scopesEmptyReq) == 0 {
		t.Fatal("expected non-empty default scopes")
	}
	scopesFiltered := resolveScopes([]string{auth.RoleUser}, []string{"wallet:read"}, []string{"wallet:read", "wallet:write"})
	if len(scopesFiltered) != 1 || scopesFiltered[0] != "wallet:read" {
		t.Fatalf("expected only wallet:read, got %v", scopesFiltered)
	}
}

func TestParseAuthConfig(t *testing.T) {
	// 1. Defaults
	t.Setenv("AUTH_SERVICE_PORT", "")
	t.Setenv("MONGO_URI", "")
	t.Setenv("ENVIRONMENT", "")
	t.Setenv("AUTH_PROVIDER", "")
	t.Setenv("KEYCLOAK_URL", "")

	cfg := parseAuthConfig()
	if cfg.port != "50054" || cfg.environment != "local" || cfg.authProviderType != "local" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	// 2. Hybrid detection via KEYCLOAK_URL
	t.Setenv("KEYCLOAK_URL", "http://keycloak:8080")
	cfgHybrid := parseAuthConfig()
	if cfgHybrid.authProviderType != "hybrid" {
		t.Fatalf("expected hybrid, got %s", cfgHybrid.authProviderType)
	}

	// 3. Explicit provider and port
	t.Setenv("AUTH_PROVIDER", "KEYCLOAK")
	t.Setenv("AUTH_SERVICE_PORT", "9999")
	cfgExplicit := parseAuthConfig()
	if cfgExplicit.authProviderType != "keycloak" || cfgExplicit.port != "9999" {
		t.Fatalf("expected keycloak/9999, got %+v", cfgExplicit)
	}
}

func TestAuthorizeWalletOwnershipLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	authDB := client.Database("auth_db")
	userStore, storeErr := NewUserStore(authDB)
	if storeErr != nil {
		t.Fatalf("NewUserStore failed: %v", storeErr)
	}

	srv := &authServer{
		store: userStore,
	}

	// 1. User does not own wallet (record doesn't exist)
	respNotOwner, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{
		Subject:  "bob_unowned",
		Resource: "wallet:w999",
		Action:   "read",
		Scopes:   []string{"wallet:read"},
	})
	if respNotOwner.Allowed {
		t.Fatal("expected unowned wallet to be rejected")
	}

	// 2. User owns wallet
	_ = userStore.CreateUser(ctx, "charlie_owner", "pwd123", "charlie@test.com", []string{auth.RoleUser}, []string{"wallet:read"})
	_ = userStore.AddWalletToUser(ctx, "charlie_owner", "w-charlie-1")

	respOwner, _ := srv.Authorize(ctx, &authv1.AuthorizeRequest{
		Subject:  "charlie_owner",
		Resource: "wallet:w-charlie-1",
		Action:   "read",
		Scopes:   []string{"wallet:read"},
	})
	if !respOwner.Allowed {
		t.Fatalf("expected owner to be allowed, got %+v", respOwner)
	}

	// 3. Ownership check error (canceled context)
	cancCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	respErr, _ := srv.Authorize(cancCtx, &authv1.AuthorizeRequest{
		Subject:  "charlie_owner",
		Resource: "wallet:w-charlie-1",
		Action:   "read",
		Scopes:   []string{"wallet:read"},
	})
	if respErr.Allowed || respErr.Reason != "ownership check failed" {
		t.Fatalf("expected ownership check failed on error, got %+v", respErr)
	}
}

func TestSetupIdentityProvider(t *testing.T) {
	local := &LocalProvider{}
	t.Setenv("KEYCLOAK_URL", "")
	t.Setenv("KEYCLOAK_JWKS_URL", "")

	// 1. Local fallback when KEYCLOAK_URL is empty
	p1 := setupIdentityProvider("keycloak", local)
	if p1 != local {
		t.Fatal("expected fallback to local provider")
	}

	// 2. Hybrid provider when KEYCLOAK_URL is provided
	t.Setenv("KEYCLOAK_URL", "http://keycloak:8080")
	t.Setenv("KEYCLOAK_JWKS_URL", "http://keycloak:8080/jwks")
	pHybrid := setupIdentityProvider("hybrid", local)
	if pHybrid == nil {
		t.Fatal("expected hybrid provider, got nil")
	}

	// 3. Default fallback
	pDefault := setupIdentityProvider("unknown", local)
	if pDefault == nil {
		t.Fatal("expected default provider, got nil")
	}
}

func TestBuildTokenEngine(t *testing.T) {
	// 1. HS256 default
	t.Setenv("AUTH_PRIVATE_KEY_PATH", "")
	t.Setenv("AUTH_PUBLIC_KEY_PATH", "")
	t.Setenv("JWT_SECRET", "test-secret-12345678901234567890")

	e1, err := buildTokenEngine()
	if err != nil || e1 == nil {
		t.Fatalf("expected HS256 engine, got err=%v", err)
	}

	// 2. Invalid RSA path
	t.Setenv("AUTH_PRIVATE_KEY_PATH", "/non/existent/path.pem")
	t.Setenv("AUTH_PUBLIC_KEY_PATH", "/non/existent/path.pub")
	_, err2 := buildTokenEngine()
	if err2 == nil {
		t.Fatal("expected error for non-existent key path")
	}
}

func TestSeedDefaultAdmin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	authDB := client.Database("auth_db_test_seed")
	_ = authDB.Drop(ctx)
	userStore, err := NewUserStore(authDB)
	if err != nil {
		t.Fatalf("NewUserStore failed: %v", err)
	}

	t.Setenv("ADMIN_USERNAME", "testadmin")
	t.Setenv("ADMIN_PASSWORD", "testpass123")

	seedDefaultAdmin(ctx, userStore)

	// Verify admin exists
	u, err := userStore.FindByUsername(ctx, "testadmin")
	if err != nil || u == nil {
		t.Fatalf("expected admin user to be created, got err=%v", err)
	}

	// Idempotent: seed again when user already exists
	seedDefaultAdmin(ctx, userStore)
}

func TestLocalProvider_NameValidateAndRefreshToken(t *testing.T) {
	engine := NewHS256Engine("test-secret-32-bytes-long-key-1234567")
	userStore, _ := NewUserStore(nil)
	revStore, _ := NewRevocationStore(nil)
	p := NewLocalProvider(userStore, engine, revStore)

	if p.Name() != "local" {
		t.Fatalf("expected name 'local', got %s", p.Name())
	}

	// Validate token issued by engine
	claims := auth.Claims{
		Subject:  "alice",
		Roles:    []string{"user"},
		Scopes:   []string{"wallet:read"},
		IssuedAt: time.Now().Unix(),
	}
	tokenStr, err := engine.IssueAccessToken(claims)
	if err != nil {
		t.Fatalf("failed to issue token: %v", err)
	}

	uClaims, err := p.ValidateToken(context.Background(), tokenStr)
	if err != nil || uClaims.Subject != "alice" {
		t.Fatalf("ValidateToken failed, got %+v, err: %v", uClaims, err)
	}

	// Invalid token
	_, errInvalid := p.ValidateToken(context.Background(), "invalid.jwt.token")
	if errInvalid == nil {
		t.Fatalf("expected error for invalid token")
	}

	// RefreshToken with nil store -> returns error
	_, _, errRefresh := p.RefreshToken(context.Background(), "some-refresh-token")
	if errRefresh == nil {
		t.Fatalf("expected error for RefreshToken without store")
	}

	// Authenticate with nil store -> returns error
	_, _, errAuth := p.Authenticate(context.Background(), "alice", "pass", nil)
	if errAuth == nil {
		t.Fatalf("expected error for Authenticate without store")
	}
}
