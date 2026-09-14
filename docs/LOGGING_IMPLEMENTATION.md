# Centralized Asynchronous Logging — Implementation Guide & Tracker

> **Companion Architecture Document:** [LOGGING_ARCHITECTURE.md](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docs/LOGGING_ARCHITECTURE.md) (Design Status: Frozen v1.0)  
> **Repository:** `global-wallet-microservices`  
> **Last Updated:** September 14, 2026  

---

## 1. Executive Implementation Dashboard

| Phase | Description | Target Component(s) | Status | Completion Date |
|---|---|---|:---:|:---:|
| **Phase 1** | Contract, Shared SDK & Service Instrumentation | `pkg/observability`, Gateway, Wallet, Ledger | **COMPLETE (100%)** | 2026-09-13 |
| **Phase 2** | Broker Setup, Reconnect Worker, Spool Hardening | `docker-compose.rabbitmq.yml`, `FileSpool`, `RabbitPublisher` | **COMPLETE (100%)** | 2026-09-14 |
| **Phase 3** | Standalone Logging Service & Dedicated Store | `cmd/logging-service`, `docker-compose.logging.yml`, `logging_db` | **COMPLETE (100%)** | 2026-09-14 |
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

- [x] **Task 2.1: Standalone RabbitMQ Docker Compose (`docker-compose.rabbitmq.yml`)**
  - Container name: `wallet_rabbitmq`
  - Image: `rabbitmq:3.13-management`
  - Ports: `5672:5672` (AMQP), `15672:15672` (Management UI)
  - Persistent volume: `wallet_rabbitmq_data`
  - Network: `wallet_shared_net`
  - Default exchange declaration: `wallet.logs.v1` (topic)

- [x] **Task 2.2: Reconnect Worker with Exponential Backoff & Jitter (`GAP-08`)**
  - File: `pkg/observability/publisher.go`
  - Parameters: Initial interval: `500ms`, Multiplier: `2.0`, Max interval: `30s`, Jitter: `±20%`
  - Background worker continuously monitors connection; reconnects automatically on channel/connection close.
  - Throttled logging: log warning on every 10 consecutive connection failures to prevent log flooding.

- [x] **Task 2.3: OS-Level Spool File Locking (`GAP-05`)**
  - File: `pkg/observability/spool.go`
  - Implement cross-platform locking (`flock` on Linux/macOS, `LockFileEx` on Windows) using lock file `<spool-path>.lock`.
  - Ensures safe concurrent execution if multiple instances run on the same node.

- [x] **Task 2.4: Spool Pruning & Retention Enforcement (`GAP-06`)**
  - File: `pkg/observability/spool.go`
  - Environment variables & defaults:
    - `LOGGING_SPOOL_MAX_BYTES`: `52428800` (50 MiB)
    - `LOGGING_SPOOL_MAX_AGE_HOURS`: `72` (72 hours)
    - `LOGGING_SPOOL_FSYNC`: `true`
  - On spool write, check file size: if budget exceeded, drop oldest non-audit events or report drop metric; never drop `AUDIT` events.

- [x] **Task 2.5: Prometheus Publisher Metrics Exporter (`GAP-07`)**
  - All 6 required metrics implemented per architecture Section 7:
    - `logging_queue_depth{service}` — gauge
    - `logging_spool_bytes{service}` — gauge
    - `logging_publish_failures_total{service, reason}` — counter
    - `logging_events_dropped_total{service}` — counter
    - `logging_spool_replay_events_total{service}` — counter
    - `logging_spool_oldest_event_age_seconds{service}` — gauge
  - Wired into `AsyncLogger.Emit()`, `publish()`, and periodic ticker.
  - Accessible via `asyncLogger.MetricsHandler()` for mounting at `GET /metrics`.

- [x] **Task 2.6: Broker Failure & Replay Integration Test**
  - `TestAsyncLoggerSpoolsOnBrokerFailure`: verifies events spool when broker is unavailable.
  - `TestAsyncLoggerReplayOnBrokerRecovery`: verifies spool drains after broker comes back.
  - `TestFileSpoolPruneRespectsAuditLevel`: verifies AUDIT events are never pruned.
  - `TestFileSpoolMaxBytesDropsNonAudit`: verifies non-audit events may be pruned under budget.
  - `TestMetricsRegistryHandlerContainsAllRequiredMetrics`: verifies all 6 metric names in output.

### 3.3 Phase 2 Definition of Done (DoD)
- [x] `docker compose -f docker-compose.rabbitmq.yml up -d` starts healthy RabbitMQ broker.
- [x] All Phase 2 unit and integration tests pass (17/17 PASS).
- [x] No data loss of `AUDIT` events during simulated broker downtime (verified by `TestFileSpoolPruneRespectsAuditLevel`).

### 3.4 Phase 2 Deliverables

| Component / File | Description | Status |
|---|---|:---:|
| [`docker-compose.rabbitmq.yml`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docker-compose.rabbitmq.yml) | Standalone RabbitMQ broker with management UI, persistent volume, `wallet_shared_net` | ✅ Complete |
| [`docker-compose.yml`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docker-compose.yml) | Added `LOGGING_RABBITMQ_URL`, `ENVIRONMENT`, `LOGGING_SPOOL_PATH` to all 4 services | ✅ Complete |
| [`pkg/observability/publisher.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/publisher.go) | `AsyncLogger` fully wired with `MetricsRegistry`; `MetricsHandler()` + `MetricsRegistry()` accessors; `NewAsyncLoggerFull` for test injection; `countingPublisher` for replay counting | ✅ Complete |
| [`pkg/observability/metrics.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/metrics.go) | All 6 Prometheus metrics per architecture spec; correct names; `SetQueueDepth`, `IncEventsDropped` added; Prometheus text-format handler | ✅ Complete |
| [`pkg/observability/spool.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/spool.go) | OS-level lock via `lock_posix.go`/`lock_windows.go`; `SpoolConfig` env-var driven; `pruneUnderLock` never drops AUDIT; `Size()` + `OldestAge()` helpers | ✅ Complete |
| [`pkg/observability/observability_test.go`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/observability_test.go) | 9 new Phase 2 tests: emit success, broker outage spool, replay, metrics wiring, spool prune AUDIT safety, budget enforcement, Prometheus output | ✅ Complete |

### 3.5 Phase 2 Verification Log

```powershell
# Run observability tests
go test -v -timeout 30s ./pkg/observability/...
# Result: 17/17 PASS (3.534s)
# Tests: Phase 1 preserved (8) + Phase 2 new (9)
#   TestAsyncLoggerEmitPublishSuccess         PASS
#   TestAsyncLoggerSpoolsOnBrokerFailure      PASS
#   TestAsyncLoggerReplayOnBrokerRecovery     PASS
#   TestAsyncLoggerMetricsEventsEmitted       PASS
#   TestAsyncLoggerMetricsPublishFailure      PASS
#   TestFileSpoolPruneRespectsAuditLevel      PASS
#   TestFileSpoolMaxBytesDropsNonAudit        PASS
#   TestMetricsRegistryHandlerContainsAllRequiredMetrics  PASS
#   TestMetricsRegistryCounterValues          PASS

# Full workspace build
go build ./...
# Result: Clean compilation (exit code 0)
```

**Phase 2 Sign-Off:** ✅ **APPROVED & VERIFIED (2026-09-14)**

---

### 3.6 Phase 2 Operational Guide

This section is the step-by-step runbook for operating the Phase 2 logging pipeline locally.
It covers broker deployment, launching services without Docker Compose, executing a real
transaction, and verifying the resulting log events in the RabbitMQ Management UI.

---

#### 3.6.1 Why Do We Check Logs in the RabbitMQ UI?

Before walking through the steps, it is important to understand **what we are verifying** and
**why the broker is the right place to look at this stage**.

> **The Core Purpose**
>
> The centralized logging pipeline exists to give every stakeholder a single, ordered view
> of what happened across the API Gateway, Wallet Service, and Ledger Service during one
> financial transaction. A successful transfer generates **12 structured events** (Steps 1-12
> in the event catalog). Each event carries the same `association_id` so the full lifecycle
> can be reconstructed even though the events originate from three independent processes.

| What we verify | Why it matters |
|---|---|
| **Events reach the broker** | Proves `AsyncLogger` serialized the event, `RabbitPublisher` connected, and publisher-confirm succeeded. |
| **All 12 steps are present** | Confirms the full 12-step event catalog is wired correctly across gateway, wallet, and ledger. |
| **`association_id` is the same on every event** | Proves cross-service correlation propagation works through HTTP headers and gRPC metadata. |
| **`transaction_id` appears from Step 8 onward** | Proves the ledger-assigned ID is backfilled into the correlation context on all subsequent events. |
| **`AUDIT` level on financial mutations** | Steps 5, 6, 8, 11 (debit, credit, ledger persist, completed) must carry `AUDIT` level and must never be dropped. |
| **`duration_ms` and `success` on terminal events** | Confirms latency measurement and outcome tagging on Steps 11 and 12. |

> **Why the broker UI and not just the console?**
>
> Console logs are process-local and ephemeral. The broker is the first **durable
> checkpoint** - once an event is confirmed by the broker it survives a service restart.
> Verifying messages in the broker confirms the full publish pipeline works, not just that
> the event was emitted to stdout. Phase 3 will add the logging service as the permanent
> consumer; until then the broker queue is the observable sink.

---

#### 3.6.2 Deploy RabbitMQ with Docker

RabbitMQ runs in its own independent compose stack, completely separate from the application.

**a. Create the shared Docker network (one-time setup)**

All compose stacks (MongoDB, RabbitMQ, application services) share one bridge network:

```powershell
docker network create wallet_shared_net
```

> Skip if you already ran `docker-compose.mongodb.yml` - the network already exists.
> Running the command again is harmless; Docker reports it already exists and exits cleanly.

**b. Start RabbitMQ**

```powershell
docker compose -f docker-compose.rabbitmq.yml up -d
```

Expected output:
```
[+] Running 2/2
 - Volume "wallet_rabbitmq_data"  Created
 - Container wallet_rabbitmq      Started
```

**c. Verify broker health**

```powershell
# Container status - look for (healthy) in STATUS column
docker compose -f docker-compose.rabbitmq.yml ps

# Direct ping - should print "Ping succeeded"
docker exec wallet_rabbitmq rabbitmq-diagnostics -q ping

# Tail broker startup logs (Ctrl+C to exit)
docker compose -f docker-compose.rabbitmq.yml logs -f
```

Healthy output from `ps`:
```
NAME              IMAGE                        STATUS
wallet_rabbitmq   rabbitmq:3.13-management     Up 30 seconds (healthy)
```

**d. Open the Management UI**

Navigate to **http://localhost:15672** in your browser.

| Field | Value |
|---|---|
| Username | `guest` |
| Password | `guest` |

The Overview page shows **Connections: 0**, **Exchanges: 7** (RabbitMQ built-ins),
**Queues: 0**. This is correct - services have not started yet.

**e. Teardown commands**

```powershell
# Stop containers, keep volume data (broker config and messages survive)
docker compose -f docker-compose.rabbitmq.yml down

# Stop and wipe all data (fresh clean slate)
docker compose -f docker-compose.rabbitmq.yml down -v

# Remove the shared network (only when ALL stacks are already stopped)
docker network rm wallet_shared_net
```

---

#### 3.6.3 Launch Application Services Locally (Without Docker)

The services have sensible defaults for every environment variable and can run as plain
`go run` processes. Only MongoDB needs to be running.

**Prerequisite - MongoDB must be running**

```powershell
docker compose -f docker-compose.mongodb.yml up -d
```

Open **three separate PowerShell terminals** at the project root, one per service.

**Terminal 1 - Ledger Service** *(start first - wallet dials it on startup)*

```powershell
cd c:\workarea\personal\After-equifax\global-wallet-microservices

$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
$env:INSTANCE_ID          = "ledger-local-1"
$env:REGION_NAME          = "global-core"

go run ./cmd/ledger-service
```

Wait for: `[LEDGER-SERVICE] gRPC listening on :50052`

**Terminal 2 - Wallet Service (Primary)**

```powershell
cd c:\workarea\personal\After-equifax\global-wallet-microservices

$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
$env:INSTANCE_ID          = "wallet-primary-local"
$env:IS_ACTIVE            = "true"
$env:REGION_NAME          = "us-east-1"

go run ./cmd/wallet-service
```

Wait for: `[WALLET-SERVICE] Listening for gRPC requests on :50051`

**Terminal 3 - API Gateway**

```powershell
cd c:\workarea\personal\After-equifax\global-wallet-microservices

$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
$env:INSTANCE_ID          = "api-gateway-local"

go run ./cmd/api-gateway
```

Wait for: `[API-GATEWAY] HTTP listening on :8080`

**Environment variable reference**

| Variable | Default (if not set) | Purpose |
|---|---|---|
| `LOGGING_RABBITMQ_URL` | *(empty - stdout JSON only)* | Enables async RabbitMQ publishing |
| `LOGGING_SPOOL_PATH` | `data/logging/<service>-<id>.jsonl` | Local fallback spool file path |
| `LOGGING_SPOOL_FSYNC` | `true` | `fsync` after every spool append |
| `LOGGING_SPOOL_MAX_BYTES` | `52428800` (50 MiB) | Max spool disk budget before pruning |
| `INSTANCE_ID` | `os.Hostname()` | Distinguishes multiple running instances |
| `PORT` | `50051` / `50052` / `8080` | gRPC or HTTP listen port |
| `MONGO_URI` | `127.0.0.1:27017` | MongoDB connection (auto-detected) |
| `LEDGER_SERVICE_ADDR` | `127.0.0.1:50052` | Wallet -> Ledger gRPC dial address |
| `IS_ACTIVE` | `false` | Marks wallet as primary (`true`) or standby |
| `REGION_NAME` | `us-east-1` | Region tag stamped on all log events |
| `ENVIRONMENT` | `local` | Environment tag stamped on all log events |

> **No `LOGGING_RABBITMQ_URL` set?** Services still work - `LoggerFromEnvironment` falls back
> to `StructuredLogger`, printing JSON events to stdout. Transactions complete normally;
> only the broker delivery path is skipped.

---

#### 3.6.4 Create Wallets and Execute a Transaction

**Step 1 - Create source wallet (bipin)**

```powershell
$body = @{
    wallet_id       = "bipin"
    currency        = "USD"
    initial_balance = 5000
} | ConvertTo-Json -Compress

Invoke-RestMethod -Method Post `
    -Uri "http://127.0.0.1:8080/api/v1/wallets" `
    -ContentType "application/json" `
    -Body $body | ConvertTo-Json
```

**Step 2 - Create destination wallet (ruby)**

```powershell
$body = @{
    wallet_id       = "ruby"
    currency        = "USD"
    initial_balance = 1000
} | ConvertTo-Json -Compress

Invoke-RestMethod -Method Post `
    -Uri "http://127.0.0.1:8080/api/v1/wallets" `
    -ContentType "application/json" `
    -Body $body | ConvertTo-Json
```

**Step 3 - Execute a transfer**

```powershell
$body = @{
    idempotency_key       = "local-tx-102"
    source_wallet_id      = "bipin"
    destination_wallet_id = "ruby"
    amount                = 50
    currency              = "USD"
} | ConvertTo-Json -Compress

Invoke-RestMethod -Method Post `
    -Uri "http://127.0.0.1:8080/api/v1/transfers" `
    -ContentType "application/json" `
    -Body $body | ConvertTo-Json
```

Expected response:
```json
{
  "transaction_id":    "66e4a1b2c3d4e5f6a7b8c9d0",
  "status":            "SUCCESS",
  "handled_by_region": "us-east-1"
}
```

A successful transfer emits exactly **12 log events** across all three services.

**Step 4 - Verify balances**

```powershell
# bipin debited 50 -> should show 4950
Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/v1/wallets/bipin/balance" | ConvertTo-Json

# ruby credited 50 -> should show 1050
Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/v1/wallets/ruby/balance" | ConvertTo-Json
```

---

#### 3.6.5 Check Log Events in the RabbitMQ Management UI

> **Phase 2 context:** The logging service (Phase 3) is the permanent queue owner and will
> declare `wallet.logging.ingest.v1` on startup. For Phase 2 verification we create this
> queue manually in the UI so we have a catch-all sink to inspect.

**Step 1 - Confirm the exchange was created**

- Open **http://localhost:15672** -> **Exchanges** tab.
- You should see `wallet.logs.v1` of type `topic` in the list.
- The Overview page should now show **Connections: 3** - one per running service.

**Step 2 - Create a temporary ingest queue**

- Click **"Queues and Streams"** -> **"Add a new queue"**.

| Field | Value |
|---|---|
| Type | Classic |
| Name | `wallet.logging.ingest.v1` |
| Durability | Durable |

- Click **"Add queue"**.

**Step 3 - Bind the queue to the exchange**

- Go to **Exchanges** -> click `wallet.logs.v1`.
- Scroll to **"Bindings"** -> **"Add binding from this exchange"**.

| Field | Value |
|---|---|
| To queue | `wallet.logging.ingest.v1` |
| Routing key | `#` |

- Click **"Bind"**.

> The routing key `#` is a topic wildcard that matches every pattern
> (`local.wallet-service.audit`, `local.api-gateway.info`, etc.) so all events from all
> services and levels land in this single queue.

**Step 4 - Run the transfer** (Section 3.6.4 Step 3).

**Step 5 - Inspect the messages**

- Go to **Queues and Streams** -> click `wallet.logging.ingest.v1`.
- Scroll to the **"Get messages"** panel.

| Field | Value |
|---|---|
| Count | `20` |
| Ack Mode | `Nack message requeue true` (non-destructive; messages stay in queue) |
| Encoding | `Auto string / base64` |

- Click **"Get Message(s)"**.

**The 12-step event sequence**

| # | Routing key | `event_type` | `level` | Service |
|---|---|---|---|---|
| 1 | `local.api-gateway.info` | `api.request.received` | `INFO` | API Gateway |
| 2 | `local.api-gateway.debug` | `api.wallet_proto.request_created` | `DEBUG` | API Gateway |
| 3 | `local.wallet-service.info` | `wallet.transfer.request_received` | `INFO` | Wallet |
| 4 | `local.wallet-service.debug` | `wallet.transfer.idempotency_checked` | `DEBUG` | Wallet |
| 5 | `local.wallet-service.audit` | `wallet.transfer.source_debited` | **AUDIT** | Wallet |
| 6 | `local.wallet-service.audit` | `wallet.transfer.destination_credited` | **AUDIT** | Wallet |
| 7 | `local.wallet-service.debug` | `wallet.transfer.ledger_request_sent` | `DEBUG` | Wallet |
| 8 | `local.ledger-service.audit` | `ledger.transaction.persisted` | **AUDIT** | Ledger |
| 9 | `local.wallet-service.debug` | `wallet.transfer.ledger_response_received` | `DEBUG` | Wallet |
| 10 | `local.wallet-service.debug` | `wallet.transfer.idempotency_record_stored` | `DEBUG` | Wallet |
| 11 | `local.wallet-service.audit` | `wallet.transfer.completed` | **AUDIT** | Wallet |
| 12 | `local.api-gateway.info` | `api.response.sent` | `INFO` | API Gateway |

**Sample AUDIT event - Step 11 (`wallet.transfer.completed`)**

```json
{
  "schema_version":  1,
  "event_id":        "01J8XYZABC123DEF456GHI789",
  "occurred_at":     "2026-09-14T02:10:33.123Z",
  "service":         "wallet-service",
  "instance_id":     "wallet-primary-local",
  "environment":     "local",
  "region":          "us-east-1",
  "level":           "AUDIT",
  "event_type":      "wallet.transfer.completed",
  "message":         "Transfer completed successfully",
  "association_id":  "9f5f2d20-a1b2-4c3d-8e4f-123456789abc",
  "transaction_id":  "66e4a1b2c3d4e5f6a7b8c9d0",
  "idempotency_key": "local-tx-102",
  "duration_ms":     42,
  "success":         true,
  "attributes": {
    "status":        "SUCCESS",
    "source_wallet": "bipin",
    "dest_wallet":   "ruby",
    "amount":        50,
    "currency":      "USD"
  }
}
```

**Verification checklist for each transaction**

| Check | How to verify |
|---|---|
| **12 messages present** | Count messages - exactly 12 for a clean transfer |
| **Same `association_id` on all 12** | Copy from any message; it must match across all others |
| **`transaction_id` empty on Steps 1-7** | Ledger has not assigned the ID yet at those steps |
| **`transaction_id` populated on Steps 8-12** | All carry the same MongoDB ObjectID |
| **Steps 5, 6, 8, 11 have `"level": "AUDIT"`** | Financial mutation boundaries - must never be dropped |
| **Step 11 has `duration_ms > 0` and `success: true`** | End-to-end transfer latency recorded |
| **Step 12 has `duration_ms > 0`** | Full HTTP round-trip latency recorded |
| **Routing keys follow `{env}.{service}.{level}`** | Visible in the message Properties panel in the UI |

---

#### 3.6.6 Full Local Startup Order (Quick Reference)

```powershell
# Infrastructure
docker network create wallet_shared_net
docker compose -f docker-compose.mongodb.yml up -d
docker compose -f docker-compose.rabbitmq.yml up -d

# Terminal 1: Ledger Service
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
$env:INSTANCE_ID = "ledger-local-1"
go run ./cmd/ledger-service

# Terminal 2: Wallet Service (Primary)
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
$env:INSTANCE_ID = "wallet-primary-local"
$env:IS_ACTIVE = "true"
go run ./cmd/wallet-service

# Terminal 3: API Gateway
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
$env:INSTANCE_ID = "api-gateway-local"
go run ./cmd/api-gateway

# Terminal 4: Logging Service
$env:MONGO_URI = "mongodb://127.0.0.1:27017"
$env:RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
go run ./cmd/logging-service
```

> Application services remain **fully functional** even if RabbitMQ is not yet running.
> `AsyncLogger` spools events locally and replays them automatically once the broker comes
> up - confirmed by `TestAsyncLoggerReplayOnBrokerRecovery` in the Phase 2 test suite.

---


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

- [x] **Task 3.1: Service Skeleton & Compose File**
  - Create `cmd/logging-service/main.go`
  - Create `docker-compose.logging.yml`
  - Port allocation: `8090` (HTTP API / Health), `9090` (Metrics)

- [x] **Task 3.2: Topology Declaration Ownership (`GAP-04`)**
  - Declare topic exchange: `wallet.logs.v1`
  - Declare queue: `wallet.logging.v1` with binding `#`
  - Declare DLX: `wallet.logs.dlx.v1`
  - Declare DLQ: `wallet.logging.dead.v1`

- [x] **Task 3.3: Ingestion Consumer & Idempotency**
  - RabbitMQ consumer with prefetch count (e.g. 50).
  - Manual acknowledgement (`Ack`) only AFTER storage confirmation.
  - Reject / Dead-letter (`Nack(requeue=false)`) on schema validation errors.
  - Bounded retry with exponential backoff on transient DB errors.

- [x] **Task 3.4: Dedicated `logging_db` Store & Schema**
  - Target database: `logging_db`, Collection: `events`
  - Define `LogRepository` interface (`Save`, `Find`, `Health`)
  - Create required indexes:
    - `{ "event_id": 1 }` (unique constraint for deduplication)
    - `{ "transaction_id": 1 }` (sparse)
    - `{ "association_id": 1 }`
    - `{ "occurred_at": 1 }`
    - `{ "service": 1, "level": 1, "occurred_at": -1 }`

- [x] **Task 3.5: Health & Readiness Probes**
  - `/healthz`: Liveness probe (process up)
  - `/readyz`: Readiness probe (broker connected + storage healthy)
  - `/metrics`: Prometheus consumer lag, ingestion throughput, DLQ counter

### 4.3 Phase 3 Definition of Done (DoD)
- [x] Events published by microservices are successfully ingested into `logging_db.events`.
- [x] Duplicate event delivery does not create duplicate database records.
- [x] Corrupted events are routed to DLQ without crashing the consumer.

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
