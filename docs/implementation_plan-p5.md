# Phase 5 — Production Hardening

This plan implements all items in the Phase 5 checklist from `LOGGING_ARCHITECTURE.md`:

| Item | Status |
|---|---|
| TLS for RabbitMQ connections | ❌ → ✅ |
| RabbitMQ credentials via secrets | ❌ → ✅ |
| Quorum queues in production | ❌ → ✅ |
| Multi-node / HA RabbitMQ | ❌ → ✅ |
| Dedicated replicated log storage | ❌ → ✅ |
| Retention, redaction, and access control | ❌ → ✅ |
| Prometheus metrics, alerts, and dashboards (GAP-07) | ❌ → ✅ |
| Transactional outbox for audit-critical events (GAP-10) | ❌ → ✅ |

---

## Proposed Changes

### 1 — TLS & Secrets: `pkg/observability`

#### [MODIFY] [publisher.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/publisher.go)

- Add `TLSConfig *tls.Config` option to `RabbitPublisher` constructor.
- Read `LOGGING_RABBITMQ_TLS_CERT`, `LOGGING_RABBITMQ_TLS_KEY`, `LOGGING_RABBITMQ_TLS_CA` environment variables.
- When all three are set, load a `tls.Config` and dial with `amqp.DialTLS`.
- Add `NewRabbitPublisherWithTLS(url, exchange string, tlsCfg *tls.Config)` constructor that the factory calls.
- Update `LoggerFromEnvironment` to build TLS config from env vars and call the new constructor.

---

### 2 — Quorum Queues: `cmd/logging-service/consumer.go`

#### [MODIFY] [consumer.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/logging-service/consumer.go)

- Add `LOGGING_QUEUE_TYPE` env var (`classic` default for local, `quorum` for production).
- When `quorum`, append `x-queue-type: quorum` argument to `QueueDeclare` for both the ingest queue and the DLQ.
- Quorum queues don't support `x-message-ttl` or priority; guard accordingly.

---

### 3 — Retention & Deletion Policy: `cmd/logging-service/repository.go`

#### [MODIFY] [repository.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/logging-service/repository.go)

- Add `Retention(ctx, maxAgeDays int) (deleted int64, err error)` method to `LogRepository` interface and `MongoLogRepository` implementation.
- Uses the existing `occurred_at` index with `$lt` filter to delete old documents.
- Call this on a configurable interval via `LOGGING_RETENTION_DAYS` (default 90) and `LOGGING_RETENTION_INTERVAL_HOURS` (default 24).

---

### 4 — Access Control: `cmd/logging-service/main.go`

#### [MODIFY] [main.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/logging-service/main.go)

- Add simple API key middleware gated by `LOGGING_API_KEY` env var. If the var is set, all `/api/v1/*` requests must supply `Authorization: Bearer <key>`. Health/readiness/metrics endpoints remain unauthenticated.
- Add a separate Prometheus metrics HTTP server on port 9090 (already exposed in docker-compose.logging.yml but not yet served).
- Wire the retention scheduler as a background goroutine.

---

### 5 — Prometheus Metrics + Grafana (GAP-07): Infrastructure

#### [NEW] [docker-compose.monitoring.yml](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docker-compose.monitoring.yml)

New compose file adding:
- **Prometheus** scraping all services' `/metrics` endpoints.
- **Grafana** with auto-provisioned datasource and dashboard JSON for the 6 logging SDK metrics.

#### [NEW] [monitoring/prometheus.yml](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/monitoring/prometheus.yml)

Prometheus scrape config targeting:
- `logging-service:9090/metrics`
- `api-gateway:8081/metrics`
- `wallet-primary:50051/metrics` (future)

#### [NEW] [monitoring/grafana/dashboards/logging.json](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/monitoring/grafana/dashboards/logging.json)

Dashboard JSON with panels for all 6 metrics:
`logging_queue_depth`, `logging_spool_bytes`, `logging_publish_failures_total`, `logging_events_dropped_total`, `logging_spool_replay_events_total`, `logging_spool_oldest_event_age_seconds`.

#### [NEW] [monitoring/grafana/provisioning/datasources/prometheus.yml](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/monitoring/grafana/provisioning/datasources/prometheus.yml)

Auto-provision Prometheus as Grafana datasource.

#### [NEW] [monitoring/grafana/provisioning/dashboards/default.yml](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/monitoring/grafana/provisioning/dashboards/default.yml)

Auto-provision the logging dashboard.

---

### 6 — Transactional Outbox (GAP-10): `pkg/observability` + services

The transactional outbox guarantees that audit log events are never lost even if the logging SDK crashes or RabbitMQ is down at the time of the wallet transaction. Events are written **inside the same MongoDB transaction** as the business state change, then a relay reads the outbox and publishes to RabbitMQ.

#### [NEW] [pkg/observability/outbox.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/observability/outbox.go)

Defines:
- `OutboxEntry` struct (same fields as `Event`, plus `status: pending|published`, `created_at`, `attempts`).
- `MongoOutbox` — writes entries inside the caller's `mongo.Session` context.
- `OutboxRelay` — background goroutine that polls `status=pending`, publishes via `EventPublisher`, marks `status=published`.
- `NewOutboxRelay(mongoURI, dbName, collectionName string, publisher EventPublisher) *OutboxRelay`.

#### [MODIFY] [cmd/wallet-service/main.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/main.go)

- Initialize an `OutboxRelay` at startup when `LOGGING_OUTBOX_ENABLED=true`.
- In `Transfer`: inside the existing `mongo.WithSession` block, after the debit/credit operations but before `CommitTransaction`, write AUDIT events (`source_debited`, `destination_credited`, `completed`) to `wallet_outbox` via `MongoOutbox`.
- Start the relay goroutine.

#### [MODIFY] [cmd/ledger-service/main.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/ledger-service/main.go)

- Same pattern: write `ledger.transaction.persisted` to `ledger_outbox` inside the MongoDB session, relay publishes asynchronously.

---

### 7 — HA RabbitMQ compose: `docker-compose.rabbitmq.yml`

#### [MODIFY] [docker-compose.rabbitmq.yml](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docker-compose.rabbitmq.yml)

- Add a commented-out 3-node HA cluster section (nodes `rabbitmq-1`, `rabbitmq-2`, `rabbitmq-3` with `RABBITMQ_ERLANG_COOKIE` and peer discovery).
- Add a `rabbitmq.conf` and `enabled_plugins` for quorum queue and stream plugin readiness.
- Keep the single-node default active so local dev still works.

#### [NEW] [config/rabbitmq/rabbitmq.conf](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/config/rabbitmq/rabbitmq.conf)

Config enabling `cluster_formation.peer_discovery_backend` and quorum queue defaults.

---

### 8 — Documentation: `docs/LOGGING_ARCHITECTURE.md`

#### [MODIFY] [LOGGING_ARCHITECTURE.md](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docs/LOGGING_ARCHITECTURE.md)

- Update Phase 5 table rows to ✅ Done.
- Update overall progress summary to Phase 5 = 100%.
- Mark GAP-07 and GAP-10 as **Resolved**.
- Update **Last reviewed** date.

---

## Verification Plan

### Automated Tests

```bash
cd c:\workarea\personal\After-equifax\global-wallet-microservices
go test ./pkg/observability/... -v -run TestOutbox
go test ./cmd/logging-service/... -v
go build ./...
```

### Manual Verification

1. `docker compose -f docker-compose.monitoring.yml up -d` → Prometheus UI at `:9090`, Grafana at `:3000`.
2. Run a transfer, confirm metrics appear on the Grafana dashboard.
3. Set `LOGGING_API_KEY=secret` and confirm `/api/v1/logs` returns 401 without the header.
4. Set `LOGGING_OUTBOX_ENABLED=true`, kill RabbitMQ mid-transfer, restart — outbox relay replays audit events.
5. Set `LOGGING_QUEUE_TYPE=quorum`, restart logging service — verify queue declared as quorum in RabbitMQ management UI.

