# Auth Service Implementation Tasks

## Phase A — Proto Contract & Code Generation
- [x] Create `proto/auth/auth.proto`
- [x] Generate `proto/auth/auth.pb.go` and `auth_grpc.pb.go`

## Phase B — Auth Service Core
- [x] Create `pkg/auth/keys.go` — RSA key loading helpers
- [x] Create `cmd/auth-service/token_engine.go` — HS256/RS256 sign+verify
- [x] Create `cmd/auth-service/user_store.go` — MongoDB user repository
- [x] Create `cmd/auth-service/revocation.go` — token blocklist
- [x] Create `cmd/auth-service/server.go` — AuthServiceServer RPC implementations
- [x] Create `cmd/auth-service/main.go` — gRPC server bootstrap

## Phase C — API Gateway Changes
- [x] Update `cmd/api-gateway/middleware.go` — AuthMiddleware accepts gRPC client
- [x] Update `cmd/api-gateway/main.go` — auth gRPC conn, handleLogin/Refresh/Logout

## Phase D — Infrastructure
- [x] Update `docker-compose.yml` — add auth-service block + gateway dependency
- [x] Update `Dockerfile` — add auth-service build target
- [x] Update `.env.example` — add AUTH_SERVICE_ADDR, key paths

## Phase E — Keycloak & External OIDC IdP Integration
- [x] Create `cmd/auth-service/claims.go` — UnifiedClaims and Keycloak claims normalizer
- [x] Create `cmd/auth-service/jwks.go` — RFC 7517 JWKS parser, thread-safe cache, and RS256 token verification
- [x] Create `cmd/auth-service/oidc_client.go` — OIDC discovery and Keycloak token endpoint client
- [x] Create `cmd/auth-service/provider.go` — Pluggable `IdentityProvider` interface (`LocalProvider`, `KeycloakProvider`, `HybridProvider`)
- [x] Update `cmd/auth-service/server.go` — Wire gRPC endpoints to `IdentityProvider`
- [x] Update `cmd/auth-service/main.go` — Bootstrap provider based on `AUTH_PROVIDER` and `KEYCLOAK_*` env vars
- [x] Create `cmd/auth-service/provider_test.go` — Unit tests for claims mapping, JWKS verification, and hybrid routing

## Phase F — Keycloak Docker Deployment & Live Verification
- [x] Pull latest Keycloak docker image (`quay.io/keycloak/keycloak:latest` - Keycloak 26.7.4)
- [x] Create `deploy/keycloak/wallet-realm-realm.json` with realm, client `wallet-api`, protocol mappers, roles, scopes, test users (`alice`, `admin`)
- [x] Add `keycloak` container to `docker-compose.yml` (ports `8085:8080`, auto-import volume mount)
- [x] Launch Keycloak container and verify clean startup and realm import
- [x] Create `cmd/auth-service/keycloak_integration_test.go` (5-level test plan)
- [x] Run live integration test suite against Keycloak container (Level 1 through Level 5 pass)
- [x] Validate all Acceptance Criteria AC-01 through AC-08
- [x] Run full regression tests (`go test -count=1 ./...`) and build (`go build ./...`)
- [x] Update walkthrough
