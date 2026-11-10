# Global Wallet Microservices — Release v1.9.0
## Sprint 2: Core Banking Distributed Resilience, Standby Fencing, Invalidation Bus Reconnect, FX Compensating Saga & Decoupled Webhooks

**Release Version**: `v1.9.0`  
**Previous Version**: `v1.8.0`  
**Release Date**: November 2026  
**Status**: General Availability (GA)  

---

## 1. Executive Summary

**Release v1.9.0** successfully delivers **Sprint 2** of the enterprise core banking hardening initiative as specified in [docs/PENDING_REQUIREMENTS_AND_ARCHITECTURE_SPEC.md](PENDING_REQUIREMENTS_AND_ARCHITECTURE_SPEC.md).

This release introduces comprehensive distributed resilience, high availability safeguards, and decoupled asynchronous processing:
1. **Standby Write Fencing (`REQ-DIST-03`)**: Exhaustive mutation fencing across 100% of mutation gRPC RPCs and HTTP admin endpoints on standby replicas, preventing split-brain writes while preserving read replicas.
2. **AMQP Invalidation Bus Full-Jitter Auto-Reconnect (`REQ-DIST-02`)**: Bounded exponential backoff with full jitter to automatically reconnect during RabbitMQ broker outages, coupled with degraded-mode cache TTL (5s) and atomic cache flushing upon reconnect.
3. **Distributed Compensating Saga for Cross-Currency FX Conversions (`REQ-FIN-02`)**: Two-phase quote reservation, execution, and compensating release across `proto/fx`, `fx-service`, and `wallet-service` to eliminate currency arbitrage and balance drift on multi-stage transfer failures.
4. **Decoupled RabbitMQ Partner Webhook Pipeline (`REQ-ASYNC-01`)**: Queue-backed webhook dispatch using exchange `wallet.events.v1`, durable queue `wallet.webhooks.dispatch.v1`, DLQ `wallet.webhooks.dead.v1`, worker concurrency, exponential retry backoff, and cryptographic HMAC-SHA256 signatures.

---

## 2. Key Milestones Delivered in v1.9.0

### 2.1. REQ-DIST-03: Exhaustive Standby Write Fencing
- **Components**: `cmd/wallet-service`, `cmd/api-gateway`, `proto/wallet`
- **Problem**: In multi-region active-passive deployments, standby replicas previously relied on MongoDB secondary write errors. If a standby node misconfigured its replica connection, silent split-brain writes could corrupt ledger invariants.
- **Implementation**:
  - Implemented proactive write fencing in `wallet-service` on every mutation RPC:
    - `DepositFunds`
    - `WithdrawFunds`
    - `ExchangeCurrency`
    - `UpdateWalletKYC`
    - `UpdateWalletStatus`
  - When running in standby mode (`!s.isWritable(ctx)`), mutation RPCs immediately abort and return gRPC status:
    ```
    codes.FailedPrecondition: Region [<region>] is in standby replica mode; write operations are fenced
    ```
  - Protected admin HTTP endpoints (e.g. `POST /admin/wallets/{id}/status`) to return `HTTP 503 Service Unavailable`.
  - Updated API Gateway error translator to map gRPC `FailedPrecondition` to `HTTP 503 Service Unavailable` with `Retry-After: 5` header.
  - Verified via concurrency tests (`concurrency_test.go:TestStandbyWriteFencing`) that 100% of mutation operations fail on standby while read operations (`GetBalance`, `ListWallets`, `HealthCheck`) remain fully operational.

### 2.2. REQ-DIST-02: AMQP Invalidation Bus Full-Jitter Auto-Reconnect & Degraded TTL
- **Components**: `pkg/cache`, `cmd/wallet-service`
- **Problem**: If RabbitMQ temporarily restarted or suffered a network partition, the cache invalidation bus connection was permanently dropped. Without auto-reconnect, local cache invalidation signals were lost, risking stale balance reads.
- **Implementation**:
  - Implemented `ComputeJitterBackoff` in `pkg/cache/invalidation_bus.go` using full jitter exponential backoff with configurable minimum backoff (500ms) and maximum ceiling (30s).
  - Added background subscriber loop `runSubscriberLoop` with automatic reconnection, channel rebuilding, and connection state notifications via `OnConnectionStatusChange`.
  - Added degraded mode to `BalanceCache`:
    - Under normal conditions, local cache items use configured TTL (15s–60s).
    - Upon bus disconnect, `BalanceCache` automatically enters **degraded mode**, clamping TTL down to **5 seconds** to bound stale read windows.
    - Upon bus reconnection, `BalanceCache` flushes all local cache entries (`c.Clear()`) to discard potentially stale data, then restores standard TTL.

### 2.3. REQ-FIN-02: Distributed Compensating Saga for FX Conversions
- **Components**: `proto/fx/fx.proto`, `cmd/fx-service`, `cmd/wallet-service`
- **Problem**: Multi-currency cross-wallet transfers required an FX quote conversion. If the subsequent MongoDB multi-document ACID debit/credit transaction failed (e.g. compliance rejection or network timeout), the FX rate quote could remain locked or improperly recorded, causing accounting discrepancies.
- **Implementation**:
  - Protobuf Contract Additions:
    - `ReserveQuote(QuoteReservationRequest)`: Places a 30s exclusive TTL lock on an active quote (status `RESERVED`).
    - `FinalizeQuote(FinalizeQuoteRequest)`: Commits the reservation to `EXECUTED` and creates immutable conversion records.
    - `ReleaseQuote(ReleaseQuoteRequest)`: Compensating Saga action that releases the reservation back to `ACTIVE` or aborts it cleanly.
  - Core Banking Saga Orchestration:
    - In `cmd/wallet-service/main.go`, `prepareFXConversion` explicitly invokes `ReserveQuote` prior to the database transaction.
    - On successful commit, `handleTransferSuccess` invokes `FinalizeQuote`.
    - On any error or rollback during the transaction, `handleTransferFailure` executes the compensating Saga step via `ReleaseQuote`.
  - Background expiration worker in `fx-service` automatically sweeps expired reservations after 30 seconds.

### 2.4. REQ-ASYNC-01: Decoupled RabbitMQ Webhook Dispatcher Pipeline
- **Components**: `pkg/webhook`, `cmd/api-gateway`, `cmd/wallet-service`
- **Problem**: Outbound partner notifications previously dispatched over synchronous HTTP in worker goroutines. Under high transaction volume or partner endpoint latency, goroutine pools could starve or drop notifications.
- **Implementation**:
  - Designed RabbitMQ event-driven architecture:
    - Direct exchange: `wallet.events.v1`
    - Durable quorum dispatch queue: `wallet.webhooks.dispatch.v1`
    - Dead letter exchange (DLX) & queue: `wallet.webhooks.dead.v1`
  - Worker Pool:
    - Configurable concurrent workers (default: 4 workers per replica).
    - Implemented exponential backoff retry scheduling for transient network/HTTP failures.
    - Exhausted retries (e.g. $> 5$ attempts) dead-letter to DLQ for manual inspection or replay.
  - Security & Integrity:
    - Every webhook delivery payload is signed with HMAC-SHA256 and transmitted in `X-Signature-SHA256` header.
    - Delivery history stored in MongoDB collection `webhook_deliveries` with delivery attempt metrics.
    - In-memory dispatch fallback automatically engaged if RabbitMQ broker is not configured.

---

## 3. Verification & Quality Gates

| Verification Gate | Result | Notes |
|---|:---:|---|
| **`go build ./cmd/...`** | ✅ PASS | All 6 microservices & CLI compiled cleanly |
| **`gofmt -l .`** | ✅ PASS | 0 unformatted files across codebase |
| **`go vet ./...`** | ✅ PASS | 0 warnings or dead-code issues |
| **`gosec` (SAST Linter)** | ✅ PASS | **0 issues** across 60 files and 21,298 lines (`-severity medium`) |
| **`govulncheck ./...`** | ✅ PASS | All dependencies up-to-date, `klauspost/compress@v1.20.2` verified |
| **`go test -race`** | ✅ PASS | Concurrency, cache, fx, webhook, and wallet test suites passed |
| **`Docker Build & Publish`** | ✅ PASS | Successfully built and pushed via desktop self-hosted runner |
| **`Post-Deploy Smoke Verification`** | ✅ PASS | Final CI/CD stage executed with status: `Succeeded` |

---

## 4. Upgrade & Migration Guide

1. **Protobuf Generation**:
   - `proto/fx/fx.proto` updated with `ReserveQuote`, `FinalizeQuote`, and `ReleaseQuote`. If maintaining external FX service integrations, re-run `protoc` generation.
2. **RabbitMQ Topologies**:
   - For environments utilizing asynchronous webhooks, verify that `RABBITMQ_URL` is configured. If unset, services automatically fall back to in-memory dispatch.
3. **Standby Replica Mode**:
   - Ensure secondary wallet instances have environment variable `STANDBY_MODE=true` set. Standby instances will reject write operations with `HTTP 503` / `codes.FailedPrecondition`.
