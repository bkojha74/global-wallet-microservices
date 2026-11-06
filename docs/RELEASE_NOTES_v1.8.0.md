# Global Wallet Microservices — Release v1.8.0
## Sprint 1: BOLA Data-Layer Ownership, Air-Gapped Test Isolation & ISO-4217 Decimal Scale Standardization

**Release Version**: `v1.8.0`  
**Previous Version**: `v1.7.0`  
**Release Date**: November 2026  
**Status**: General Availability (GA)  

---

## 1. Executive Summary

**Release v1.8.0** successfully delivers **Sprint 1** of the enterprise core banking hardening initiative as specified in [docs/PENDING_REQUIREMENTS_AND_ARCHITECTURE_SPEC.md](PENDING_REQUIREMENTS_AND_ARCHITECTURE_SPEC.md). 

This release resolves three critical (P0) architectural requirements:
1. **Unified Broken Object Level Authorization (BOLA/IDOR)** between API Gateway and the core banking MongoDB data layer.
2. **Air-Gapped and Offline Test Suite Resilience**, removing all live TCP bindings and external DNS queries to ensure deterministic, zero-flake CI/CD runs.
3. **Core Banking ISO-4217 Decimal Scale & Fractional Nanos Representation**, evolving the protobuf `Money` contract to Google's financial money pattern with full backward compatibility, zero-decimal currency protection (JPY, KRW), three-decimal currency support (BHD, KWD, TND), and complete GoSec CWE-190 (G115) integer overflow compliance.

---

## 2. Key Milestones Delivered in v1.8.0

### 2.1. REQ-SEC-01: BOLA / IDOR Data-Layer Ownership Unification
- **Components**: `api-gateway`, `wallet-service`
- **Problem**: API Gateway previously relied exclusively on string prefix heuristics (`wal_<sub_or_role>_*`) to authorize wallet mutations and balance views. When encountering UUIDs or foreign wallet identifiers, ownership could not be verified at the edge, risking Broken Object Level Authorization.
- **Implementation**:
  - Implemented `WalletOwnershipCache` with fast in-memory lookups, thread-safe synchronization, and auto-expiration (TTL: 60s, capacity: 50,000 entries).
  - Added authenticated gRPC fallback probes: when an unknown wallet identifier is requested, the Gateway queries `wallet-service`'s `GetBalance` RPC using mTLS/service identity to verify the true `OwnerID` in MongoDB.
  - Transparently invalidates local cache on wallet closure or reassignment.
  - Enforced strict 403 Forbidden responses whenever a subject attempts unauthorized balance inquiries, deposits, or transfers across non-owned accounts.

### 2.2. REQ-DEV-01: Air-Gapped & Offline Test Suite Resilience
- **Components**: `pkg/webhook`, `cmd/walletctl`, `cmd/api-gateway`, `cmd/auth-service`, `cmd/fx-service`, `pkg/tlsutil`
- **Problem**: Test suites contained live TCP socket allocations (`httptest.NewServer`) and unmocked DNS resolutions (`net.LookupIP`), causing intermittent failures and timeouts in corporate firewalls, air-gapped CI environments, and container sandboxes.
- **Implementation**:
  - Decoupled `net.LookupIP` in `pkg/webhook/dispatcher.go` with an injectable `lookupIPFunc`, enabling air-gapped mock DNS resolution without network access.
  - Converted HTTP test mocks across `walletctl`, `api-gateway`, `auth-service`, `fx-service`, and `webhook` to in-memory `http.RoundTripper` transports (`roundTripFunc`), eliminating live port bindings.
  - Converted gRPC mTLS tests in `pkg/tlsutil/tls_test.go` to in-memory `bufconn.Listen`.
  - Added pass-through support in `mockAuthServerRegistry` and `mockServerRegistry` to allow live Keycloak integration tests to seamlessly communicate with dockerized services during full integration runs.

### 2.3. REQ-FIN-01: Core Banking ISO-4217 Decimal Scale & Fractional Nanos
- **Components**: `proto/wallet/wallet.proto`, `pkg/db/currency.go`, `cmd/wallet-service`, `cmd/api-gateway`, `pkg/cache`
- **Problem**: The original `Money` contract lacked minor-unit decimal scaling and fractional units, creating truncation and precision mismatch vulnerabilities between standard currencies (USD/EUR, scale 2), zero-decimal currencies (JPY, scale 0), and three-decimal currencies (BHD, scale 3).
- **Implementation**:
  - Evolved `walletv1.Money` with Google-standard financial fields while guaranteeing 100% backward compatibility:
    ```protobuf
    message Money {
      string currency = 1;      // Backward compatible ISO code
      int64 units = 2;          // Minor units or whole units
      int32 nanos = 3;          // Fractional amount (10^-9) in [0, 999,999,999]
      int32 scale = 4;          // Minor-unit decimal exponent (e.g. 0 for JPY/KRW, 2 for USD/EUR, 3 for BHD/KWD)
      string currency_code = 5; // Standard 3-letter ISO-4217 code
    }
    ```
  - Added zero-decimal currency `KRW` (scale 0) and three-decimal currencies `KWD` (scale 3) and `TND` (scale 3) to `ISO4217Catalog` and `SupportedCurrencies`.
  - Implemented `ValidateMoney`:
    - Rejects invalid or unsupported currencies.
    - Rejects negative units and zero total amounts.
    - Validates nanos in `[0, 999,999,999]`.
    - **Rejects fractional subunits (nanos != 0) for zero-decimal currencies (`JPY`, `KRW`)**.
  - Implemented `CurrencyScaleInt32` with verified bounds checks `[0, 18]`, resolving all GoSec CWE-190 (G115) integer overflow conversion warnings across all microservices (0 GoSec issues).
  - Updated API Gateway `parseMoney` helper to seamlessly accept scalar integer amounts or structured JSON money payloads with explicit scale and nanos.

---

## 3. Verification & Quality Gates

| Verification Gate | Result | Notes |
|---|:---:|---|
| **`go build ./...`** | ✅ PASS | Clean compilation across all 6 microservices & CLI |
| **`gofmt -l .`** | ✅ PASS | 100% compliant code formatting |
| **`go vet ./...`** | ✅ PASS | Clean static analysis |
| **`golangci-lint run`** | ✅ PASS | 0 dead code or unused symbol warnings |
| **`gosec` (SAST Linter)** | ✅ PASS | 0 issues across 20,254 lines of Go code (`severity: medium+`) |
| **`Unit Tests & Race Detection`** | ✅ PASS | `go test -race ./...` passed across all packages |
| **`Air-Gapped Resilience`** | ✅ PASS | 0 external network requests or live TCP binds during unit tests |
| **`Docker Build & Publish`** | ✅ PASS | Automated multi-stage Docker images published |

---

## 4. Upgrade & Migration Guide

1. **Protobuf Backward Compatibility**:
   - Existing clients communicating with `currency` and `units` fields remain 100% binary and JSON compatible. No breaking changes were introduced.
2. **Zero-Decimal Currencies**:
   - Clients sending requests in `JPY` or `KRW` with `nanos > 0` will receive `codes.InvalidArgument` (rejected by core banking invariant validation).
3. **API Gateway Ingress**:
   - JSON payloads can now provide either integer units (e.g. `{"amount": 1000, "currency": "USD"}`) or structured money (e.g. `{"amount": {"units": 1000, "currency_code": "USD", "scale": 2}}`).

