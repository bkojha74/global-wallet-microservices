# Centralized Asynchronous Logging — Implementation Guide & Tracker

> **Companion Architecture Document:** [LOGGING_ARCHITECTURE.md](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docs/LOGGING_ARCHITECTURE.md) (Design Status: Frozen v1.0)  
> **Repository:** `global-wallet-microservices`  
> **Last Updated:** September 13, 2026  

---

## 1. Executive Implementation Dashboard

| Phase | Description | Target Component(s) | Status | Completion Date |
|---|---|---|:---:|:---:|
| **Phase 1** | Contract, Shared SDK & Service Instrumentation | `pkg/observability`, Gateway, Wallet, Ledger | **COMPLETE (100%)** | 2026-09-13 |
| **Phase 2** | Broker Setup, Reconnect Worker, Spool Hardening | `docker-compose.rabbitmq.yml`, `FileSpool`, `RabbitPublisher` | **READY / NEXT** | — |
| **Phase 3** | Standalone Logging Service & Dedicated Store | `cmd/logging-service`, `docker-compose.logging.yml`, `logging_db` | **PLANNED** | — |
| **Phase 4** | Query API & End-to-End Operational Tracing | `cmd/logging-service` Query Endpoints, CLI Verification | **PLANNED** | — |
| **Phase 5** | Production Hardening, Outbox, & Retention | Transactional Outbox, TLS, Retention TTL, Dashboards | **PLANNED** | — |

---

## 2. Phase 1 — Contract, Shared SDK & Service Instrumentation

### 2.1 Objectives
1. Establish a strongly-typed, versioned JSON event envelope.
2. Provide shared Go logging infrastructure (`pkg/observability`) with pluggable sinks.
3. Propagate correlation metadata (`association_id`, `transaction_id`, `idempotency_key`) across HTTP and gRPC boundaries.
4. Implement the standard 12-step event catalog with `AUDIT` level on financial mutations and latency/success tracking on terminal boundaries.
5. Provide attribute redaction and unit test coverage.

### 2.2 Phase 1 Deliverables & Artifacts

| Component / File | Description | Status |
|---|---|:---:|
| [`pkg/observability/event.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/event.go) | `Event` struct, `InstanceID`, Level constants, `ValidateEvent`, `StructuredLogger`, `MemoryLogger` | ✅ Complete |
| [`pkg/observability/context.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/context.go) | `Correlation` context, HTTP header extraction, gRPC metadata propagation helpers | ✅ Complete |
| [`pkg/observability/redact.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/redact.go) | `RedactAttributes` masking passwords, secrets, tokens, API keys, card numbers | ✅ Complete |
| [`pkg/observability/publisher.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/publisher.go) | `AsyncLogger` scaffolding, `RabbitPublisher`, `LoggerFromEnvironment` with instance disambiguation | ✅ Complete |
| [`pkg/observability/spool.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/spool.go) | `FileSpool` append and replay with Windows file-locking fix | ✅ Complete |
| [`pkg/observability/observability_test.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/observability_test.go) | Unit tests for envelope, validation, correlation round-trip, redaction, and spooling | ✅ Complete |
| [`cmd/api-gateway/main.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/api-gateway/main.go) | Emits Step 1 (`api.request.received`), Step 2 (`api.wallet_proto.request_created`), and Step 12 (`api.response.sent`) with latency & success | ✅ Complete |
| [`cmd/wallet-service/main.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/main.go) | Emits Steps 3, 4, 5, 6, 7, 9, 10, 11 with `LevelAudit` on mutation boundaries, rollback events, and terminal metrics | ✅ Complete |
| [`cmd/ledger-service/main.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/ledger-service/main.go) | Emits Step 8 (`ledger.transaction.persisted` - `AUDIT`) and error failure events with latency & success | ✅ Complete |
| [`docker-compose.yml`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docker-compose.yml) | Injected `INSTANCE_ID` across all 4 microservice containers | ✅ Complete |
| [`pkg/db/mongo.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/db/mongo.go) | Added `DefaultMongoURI()` auto-detecting Docker DNS vs host `127.0.0.1:27017` | ✅ Complete |

### 2.3 Phase 1 Verification & Sign-off

```powershell
# 1. Observability SDK Unit Tests
go test -v ./pkg/observability
# Result: 8/8 PASS (Validation, Envelope, Redaction, InstanceID, Spool Replay)

# 2. Entire Workspace Test Suite
go test -v ./...
# Result: All packages PASS (api-gateway, ledger-service, wallet-service, db, observability, protos)

# 3. Compilation Verification
go build ./...
# Result: Clean compilation (exit code 0)
```

**Phase 1 Sign-Off:** ✅ **APPROVED & VERIFIED (2026-09-13)**

---

## 3. Phase 2 — Broker Setup, Publisher Resilience & Spool Hardening

### 3.1 Objectives
1. Provide standalone RabbitMQ broker infrastructure (`docker-compose.rabbitmq.yml`).
2. Implement exponential backoff reconnect worker with jitter in `RabbitPublisher` (`GAP-08`).
3. Harden `FileSpool` with OS-level file locking (`<spool-path>.lock`) to guard against multi-process concurrency (`GAP-05`).
4. Implement spool disk budgeting, max retention age pruning, and configurable `fsync` policies (`GAP-06`).
5. Expose standard Prometheus metrics endpoint on publishers (`GAP-07`).
6. Validate broker outage simulation (local spooling while broker is down, auto-replay upon broker recovery).

### 3.2 Phase 2 Task Checklist

- [ ] **Task 2.1: Standalone RabbitMQ Docker Compose (`docker-compose.rabbitmq.yml`)**
  - Container name: `wallet_rabbitmq`
  - Image: `rabbitmq:3.13-management`
  - Ports: `5672:5672` (AMQP), `15672:15672` (Management UI)
  - Persistent volume: `wallet_rabbitmq_data`
  - Network: `wallet_shared_net`
  - Default exchange declaration: `wallet.logs.v1` (topic)

- [ ] **Task 2.2: Reconnect Worker with Exponential Backoff & Jitter (`GAP-08`)**
  - File: `pkg/observability/publisher.go`
  - Parameters: Initial interval: `500ms`, Multiplier: `2.0`, Max interval: `30s`, Jitter: `±20%`
  - Background worker continuously monitors connection; reconnects automatically on channel/connection close.
  - Throttled logging: log warning on every 10 consecutive connection failures to prevent log flooding.

- [ ] **Task 2.3: OS-Level Spool File Locking (`GAP-05`)**
  - File: `pkg/observability/spool.go`
  - Implement cross-platform locking (`flock` on Linux/macOS, `LockFileEx` on Windows) using lock file `<spool-path>.lock`.
  - Ensures safe concurrent execution if multiple instances run on the same node.

- [ ] **Task 2.4: Spool Pruning & Retention Enforcement (`GAP-06`)**
  - File: `pkg/observability/spool.go`
  - Environment variables & defaults:
    - `LOGGING_SPOOL_MAX_BYTES`: `52428800` (50 MiB)
    - `LOGGING_SPOOL_MAX_AGE_HOURS`: `72` (72 hours)
    - `LOGGING_SPOOL_FSYNC`: `true`
  - On spool write, check file size: if budget exceeded, drop oldest non-audit events or report drop metric; never drop `AUDIT` events.

- [ ] **Task 2.5: Prometheus Publisher Metrics Exporter (`GAP-07`)**
  - Expose metrics table specified in Section 7:
    - `logging_events_emitted_total{service, level, event_type}`
    - `logging_publish_failures_total{service, reason}`
    - `logging_spool_writes_total{service}`
    - `logging_spool_bytes_current{service}`
    - `logging_spool_replayed_total{service}`
    - `logging_spool_oldest_age_seconds{service}`

- [ ] **Task 2.6: Broker Failure & Replay Integration Test**
  - Stop RabbitMQ -> Verify microservices continue processing transfers.
  - Verify events append to local `<service>-<instance_id>.jsonl` spool.
  - Start RabbitMQ -> Verify background replay flushes spool cleanly without message loss.

### 3.3 Phase 2 Definition of Done (DoD)
- [ ] `docker compose -f docker-compose.rabbitmq.yml up -d` starts healthy RabbitMQ broker.
- [ ] All Phase 2 unit and integration tests pass.
- [ ] No data loss of `AUDIT` events during simulated broker downtime.

---

## 4. Phase 3 — Standalone Logging Service & Dedicated Store

### 4.1 Objectives
1. Create standalone `cmd/logging-service`.
2. Own RabbitMQ topology (queue, dead-letter exchange, dead-letter queue) (`GAP-04`).
3. Ingest messages with manual acknowledgements and deduplicate by `event_id`.
4. Persist events into dedicated MongoDB `logging_db` database via `LogRepository` interface.
5. Route malformed payloads to DLQ (`wallet.logging.dead.v1`).
6. Expose health checks (`/healthz`, `/readyz`) and metrics (`/metrics`).

### 4.2 Phase 3 Task Checklist

- [ ] **Task 3.1: Service Skeleton & Compose File**
  - Create `cmd/logging-service/main.go`
  - Create `docker-compose.logging.yml`
  - Port allocation: `8090` (HTTP API / Health), `9090` (Metrics)

- [ ] **Task 3.2: Topology Declaration Ownership (`GAP-04`)**
  - Declare topic exchange: `wallet.logs.v1`
  - Declare queue: `wallet.logging.v1` with binding `#`
  - Declare DLX: `wallet.logs.dlx.v1`
  - Declare DLQ: `wallet.logging.dead.v1`

- [ ] **Task 3.3: Ingestion Consumer & Idempotency**
  - RabbitMQ consumer with prefetch count (e.g. 50).
  - Manual acknowledgement (`Ack`) only AFTER storage confirmation.
  - Reject / Dead-letter (`Nack(requeue=false)`) on schema validation errors.
  - Bounded retry with exponential backoff on transient DB errors.

- [ ] **Task 3.4: Dedicated `logging_db` Store & Schema**
  - Target database: `logging_db`, Collection: `events`
  - Define `LogRepository` interface (`Save`, `Find`, `Health`)
  - Create required indexes:
    - `{ "event_id": 1 }` (unique constraint for deduplication)
    - `{ "transaction_id": 1 }` (sparse)
    - `{ "association_id": 1 }`
    - `{ "occurred_at": 1 }`
    - `{ "service": 1, "level": 1, "occurred_at": -1 }`

- [ ] **Task 3.5: Health & Readiness Probes**
  - `/healthz`: Liveness probe (process up)
  - `/readyz`: Readiness probe (broker connected + storage healthy)
  - `/metrics`: Prometheus consumer lag, ingestion throughput, DLQ counter

### 4.3 Phase 3 Definition of Done (DoD)
- [ ] Events published by microservices are successfully ingested into `logging_db.events`.
- [ ] Duplicate event delivery does not create duplicate database records.
- [ ] Corrupted events are routed to DLQ without crashing the consumer.

---

## 5. Phase 4 — Query API & Operational Tracing

### 5.1 Objectives
1. Implement query endpoints in `logging-service` to inspect transaction audit trails.
2. Support correlation searches by `transaction_id`, `association_id`, service, level, and timestamp.
3. Provide end-to-end trace reconstruction ordered by `occurred_at`.
4. Create verification scripts/CLI to inspect live transfer timelines.

### 5.2 Phase 4 Task Checklist

- [ ] **Task 4.1: Query Endpoints in Logging Service**
  - `GET /api/v1/logs`: Filter by `transaction_id`, `association_id`, `service`, `level`, `from`, `to`, `limit`, `offset`.
  - `GET /api/v1/traces/{association_id}`: Reconstruct full chronological lifecycle across API Gateway, Wallet, and Ledger.
- [ ] **Task 4.2: Automated Timeline Verification Test**
  - Execute transfer via API Gateway -> Query `GET /api/v1/traces/{association_id}`.
  - Verify all 12 sequence steps appear in chronological order.
- [ ] **Task 4.3: Health & Diagnostics CLI Tool**
  - Command-line utility to query recent errors and DLQ status.

### 5.3 Phase 4 Definition of Done (DoD)
- [ ] A transfer can be queried by its `association_id` or `transaction_id`, returning the unified 12-step trace.
- [ ] Query API response time < 50ms for indexed lookups.

---

## 6. Phase 5 — Production Hardening & Transactional Outbox

### 6.1 Objectives
1. Implement Transactional Outbox Pattern in Wallet and Ledger services (`GAP-10`).
2. Enable TLS mutual authentication and secret injection for RabbitMQ and MongoDB.
3. Configure quorum queues for high-availability multi-node broker deployments.
4. Establish retention lifecycle policies (TTL index on `occurred_at`).
5. Grafana dashboard templates for logging throughput, consumer lag, and failure rates.

### 6.2 Phase 5 Task Checklist

- [ ] **Task 5.1: Transactional Outbox Pattern (`GAP-10`)**
  - Collections: `banking_db.wallet_outbox` and `banking_db.ledger_outbox`.
  - Save audit log events in the same MongoDB transaction as the financial state change.
  - Dedicated relay process tails/polls the outbox and publishes reliably to RabbitMQ.
- [ ] **Task 5.2: RabbitMQ Quorum Queues & TLS**
  - Migrate durable queues to quorum queues.
  - Enforce TLS encryption for client connections.
- [ ] **Task 5.3: Production Storage Evolution**
  - Swap `logging_db` MongoDB repository implementation with OpenSearch or ClickHouse adapter via the existing `LogRepository` interface.
- [ ] **Task 5.4: Retention Policies & TTL**
  - Configure MongoDB TTL index or storage index lifecycle policies for log expiry (e.g. 30 days general, 365 days audit).

### 6.3 Phase 5 Definition of Done (DoD)
- [ ] Zero event loss even under arbitrary service crashes during transactions (guaranteed by outbox).
- [ ] Full compliance with security, secret management, and retention requirements.

---

## 7. Workflow: How to Update this Document

When completing tasks in each phase, developers and agents must follow this update procedure:

1. **Check off Completed Tasks:** Change `[ ]` to `[x]` for completed checklist items.
2. **Update Status Column in Section 1:** Change status from `READY` / `PLANNED` to `IN PROGRESS` or `COMPLETE (100%)`.
3. **Record Deliverables in Phase Section:** Add new files, modified files, and PR references.
4. **Append Verification Logs:** Paste automated test outputs, commands used, and validation results.
5. **Sync Architecture Document:** Ensure [LOGGING_ARCHITECTURE.md](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docs/LOGGING_ARCHITECTURE.md) Section 16 is updated in tandem.
