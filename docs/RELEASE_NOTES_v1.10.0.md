# Global Wallet Microservices — Release v1.10.0
## Sprint 3: Counterparty Journal Security, Keycloak Backchannel Logout, Redis Compose Integration, Outbox Engine Consolidation & OpenAPI/Bruno Contract Parity

**Release Version**: `v1.10.0`  
**Previous Version**: `v1.9.0`  
**Release Date**: November 2026  
**Status**: General Availability (GA)  

---

## 1. Executive Summary

**Release v1.10.0** successfully delivers **Sprint 3** of the enterprise core banking hardening initiative as specified in [docs/PENDING_REQUIREMENTS_AND_ARCHITECTURE_SPEC.md](PENDING_REQUIREMENTS_AND_ARCHITECTURE_SPEC.md).

This release introduces critical identity security protections, transaction-level ledger journal access control, infrastructure consolidation, and full API contract parity:
1. **Counterparty Ledger Journal Access Control (`REQ-SEC-02`)**: Extended gRPC and REST ledger APIs to allow queries by `transaction_id`. Enforced strict counterparty authorization at the API Gateway: non-admin callers can only inspect journal entries if they own either the source or destination wallet of the transaction (`403 Forbidden` otherwise).
2. **Keycloak Backchannel Logout & Instant Token Revocation Sync (`REQ-SEC-03`)**: Added OIDC backchannel logout endpoint (`POST /api/v1/auth/backchannel-logout`) accepting `logout_token` JWTs, propagating subject-level revocation into `auth-service` MongoDB TTL collections, and clearing gateway in-memory claims cache immediately with `Cache-Control: no-store`.
3. **Native Redis L2 Cache Compose Integration (`REQ-DIST-01`)**: Embedded `redis:7-alpine` container (`wallet_redis`) directly into root `docker-compose.yml` with healthchecks, persistent `wallet_redis_data` volume, and explicit `depends_on` conditions on `wallet-primary` and `wallet-standby`.
4. **Consolidated Single Outbox Polling Engine (`REQ-ASYNC-02`)**: Chained generic `OutboxRelay` batch processing directly into the elected `LedgerRelay` active leader loop via `WithOutboxRelay`, eliminating redundant polling routines and secondary node database IOPS.
5. **OpenAPI 3.1 & Bruno Automated E2E Suite Contract Parity (`REQ-DEV-02`)**: Updated OpenAPI 3.1 schema specification, added dedicated Bruno collection tests, and extended `scripts/verify_bruno_suite.ps1` to validate counterparty ledger queries and backchannel logout flows.
6. **Toolchain Security Patch**: Updated Go toolchain to `1.26.11` to remediate standard library security advisories (`GO-2026-6859`, `GO-2026-6600`, `GO-2026-6599`) across all microservices and CI/CD pipelines.

---

## 2. Key Milestones Delivered in v1.10.0

### 2.1. REQ-SEC-02: Counterparty Ledger Journal Access Control
- **Components**: `proto/ledger/ledger.proto`, `cmd/ledger-service`, `cmd/api-gateway`
- **Problem**: Previously, `GET /api/v1/ledger` required a caller to specify `wallet_id` and enforced that non-admin callers could only access their own wallet. If a user participated in a transaction as a counterparty (e.g. sender transferring to recipient), they could not inspect the immutable double-entry ledger journal of that transaction using `transaction_id` without exposing the entire journal of the other party.
- **Implementation**:
  - **Protobuf Contract Extension**:
    - Added `string transaction_id = 4;` to `GetLedgerRequest` in `proto/ledger/ledger.proto`.
  - **Ledger Service Implementation**:
    - Updated `GetLedgerEntries` in `cmd/ledger-service/main.go` to support filtering by `transaction_id`, `wallet_id`, or both.
  - **API Gateway Access Control**:
    - Updated `handleLedger` in `cmd/api-gateway/main.go` to accept optional query parameter `transaction_id`.
    - If `transaction_id` is supplied by a non-admin caller without `wallet_id`:
      - Queries `ledger-service` for entries associated with the transaction.
      - For each entry, verifies whether the caller's authenticated identity (`sub` or `wallet_id`) matches `entry.SourceWalletId` or `entry.DestinationWalletId`.
      - If the caller is not a party to the transaction, the request is immediately rejected with `HTTP 403 Forbidden` (`error: forbidden: not a counterparty to this transaction`).
  - **Verification**:
    - Unit and integration tests added in `cmd/api-gateway/main_test.go:TestHandleLedgerCounterpartyAccess` verifying sender, recipient, admin, and unauthorized third-party access patterns.

### 2.2. REQ-SEC-03: Keycloak Backchannel Logout & Instant Token Revocation Sync
- **Components**: `proto/auth/auth.proto`, `cmd/auth-service`, `cmd/api-gateway`
- **Problem**: When a user logged out via an OpenID Connect Identity Provider (e.g. Keycloak) or an administrator revoked a session in Keycloak, API Gateway in-memory claims cache and downstream services continued to accept cached JWTs until local TTL expiration.
- **Implementation**:
  - **Protobuf Contract Extension**:
    - Added `string subject = 3;` to `RevokeTokenRequest` in `proto/auth/auth.proto` to allow identity-level blanket revocation.
  - **Auth Service Subject Revocation**:
    - Added `revoked_subjects` MongoDB collection in `cmd/auth-service/revocation.go` with compound unique index on `(subject, revoked_at)` and TTL index.
    - Updated `RevokeToken` RPC to handle subject-level revocation requests.
    - Updated `ValidateToken` RPC to check `IsSubjectRevoked(subject)` prior to validating individual claims.
  - **API Gateway Invalidation & Endpoint**:
    - Added `invalidateSubject(subject string)` to `claimsCache` in `cmd/api-gateway/middleware.go` to instantly evict cached JWT entries for a revoked user.
    - Added standard OIDC Backchannel Logout handler `POST /api/v1/auth/backchannel-logout` in `cmd/api-gateway/main.go`:
      - Parses `logout_token` form-encoded parameter or JSON payload.
      - Extracts subject claim (`sub`) from unverified JWT token headers/payload.
      - Forwards revocation request to `auth-service` via `RevokeToken(subject=sub, reason="backchannel_logout")`.
      - Instantly invalidates all in-memory `claimsCache` entries matching that subject.
      - Returns `HTTP 200 OK` with required RFC `Cache-Control: no-store` header.
  - **Verification**:
    - Tested via `cmd/api-gateway/main_test.go:TestHandleBackchannelLogout` and `cmd/auth-service/server_methods_test.go`.

### 2.3. REQ-DIST-01: Native Redis L2 Cache Compose Architecture
- **Components**: `docker-compose.yml`, `.github/workflows/ci.yml`
- **Problem**: Running local development environments via `docker-compose.yml` previously required launching `docker-compose.redis.yml` separately. Without it, `wallet-service` emitted connection retry warnings against `REDIS_ADDR: redis:6379`.
- **Implementation**:
  - Embedded `redis:7-alpine` container (`wallet_redis`) directly into primary `docker-compose.yml`:
    - Port mapping: `127.0.0.1:6379:6379`
    - Persistence: `wallet_redis_data` named volume
    - Healthcheck: `redis-cli ping` with 5s interval and 5 retries
    - Cache policy: `--maxmemory 256mb --maxmemory-policy allkeys-lru --appendonly yes`
  - Wired `wallet-primary` and `wallet-standby` services with explicit `depends_on: redis`.
  - Updated CI/CD pipeline deployment stages to prevent container naming collisions across multi-file deployments.

### 2.4. REQ-ASYNC-02: Consolidated Single Outbox Polling Engine
- **Components**: `pkg/observability/outbox.go`, `cmd/wallet-service/ledger_outbox.go`, `cmd/wallet-service/main.go`
- **Problem**: `wallet-service` previously ran two independent background polling routines: one for `LedgerRelay` (disaster recovery / leader lease) and another for generic audit event `OutboxRelay`. Standby nodes also ran background workers, wasting database IOPS.
- **Implementation**:
  - Exported `ProcessBatch(ctx context.Context) error` on `OutboxRelay` in `pkg/observability/outbox.go`.
  - Added functional option `WithOutboxRelay(or *observability.OutboxRelay)` to `LedgerRelay` in `cmd/wallet-service/ledger_outbox.go`.
  - Chained outbox batch processing directly inside `LedgerRelay.pollOnce()` strictly when `r.IsLeader()` is `true`.
  - Standby nodes and non-leader replicas automatically suspend outbox polling, completely eliminating secondary node IOPS and duplicate message publications.

### 2.5. REQ-DEV-02: OpenAPI 3.1 & Bruno Automated E2E Suite Contract Parity
- **Components**: `cmd/api-gateway/openapi.json`, `bruno/`, `scripts/verify_bruno_suite.ps1`
- **Problem**: API Gateway documentation and automated regression suites lacked specifications and assertions for counterparty ledger queries and backchannel logout endpoints.
- **Implementation**:
  - Updated `cmd/api-gateway/openapi.json`:
    - Added `transaction_id` query parameter to `GET /api/v1/ledger`.
    - Added `POST /api/v1/auth/backchannel-logout` with OpenAPI request/response schema specifications.
  - Added Bruno request definitions:
    - `bruno/08-Ledger-Merkle/04-Get-Ledger-By-Transaction-ID.bru`
    - `bruno/02-Authentication/04-Backchannel-Logout.bru`
  - Extended automated verification script `scripts/verify_bruno_suite.ps1`:
    - Added test `04-Backchannel-Logout` asserting `200 OK` and `Cache-Control: no-store`.
    - Added test `03-Get-Ledger-By-TxnID-Counterparty` asserting authenticated counterparty journal access.

---

## 3. Verification & Quality Gates

| Verification Gate | Result | Notes |
|---|:---:|---|
| **`go build ./cmd/...`** | ✅ PASS | All 6 microservices & `walletctl` compiled cleanly |
| **`gofmt -l .`** | ✅ PASS | 0 unformatted files across entire workspace |
| **`go vet ./...`** | ✅ PASS | 0 warnings across all internal packages |
| **`gosec` (SAST Linter)** | ✅ PASS | **0 issues** across 60 files and 21,606 lines (`-severity medium`) |
| **`govulncheck ./...`** | ✅ PASS | Go toolchain 1.26.11 remediates all stdlib vulnerabilities |
| **`go test -race`** | ✅ PASS | Gateway, Auth, Ledger, Wallet, and Observability suites passed |
| **`Docker Build & Publish`** | ✅ PASS | Built and pushed all 6 images to Docker Hub registry |
| **`Local Deploy Verification`** | ✅ PASS | Docker Compose cluster verified healthy on port 8080 |

---

## 4. Upgrade & Migration Guide

1. **Protobuf Stubs Recompilation**:
   - `proto/ledger/ledger.proto` and `proto/auth/auth.proto` contain new fields. Pre-compiled `.pb.go` stubs are checked into the repository. External gRPC clients should update their proto definitions.
2. **Keycloak Backchannel Logout URL**:
   - In Keycloak Admin Console under Client Settings for `wallet-api-gateway`:
     - Set **Backchannel Logout URL** to: `https://<gateway-domain>/api/v1/auth/backchannel-logout`
     - Ensure **Backchannel Logout Session Required** is enabled.
3. **Docker Compose Launch**:
   - Running `docker compose up -d` now automatically includes Redis L2 cache. Standalone invocation of `docker-compose.redis.yml` is no longer necessary.
