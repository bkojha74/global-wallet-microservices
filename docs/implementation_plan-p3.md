# Implementation Plan: Phase 3 — Standalone Logging Service & Dedicated Store

This plan details the implementation steps for Phase 3 of the Centralized Asynchronous Logging architecture. The goal is to build a standalone `logging-service` that consumes events from RabbitMQ, handles schema validation and deduplication, and persists the events to a dedicated `logging_db` MongoDB instance.

## User Review Required

- Please review the planned `LogRepository` interface and MongoDB index strategy to ensure it meets query performance requirements.
- The `docker-compose.logging.yml` file will be created. Let me know if you have specific volume mount or network preferences beyond what's specified in the architecture document.

## Open Questions

- Should the `logging-service` use the exact same `pkg/observability` structs for its internal logic, or should we define a separate internal representation? (Assuming `pkg/observability.Event` will be used for consistency).
- For transient MongoDB errors during ingestion, is a simple bounded retry (e.g., up to 3 retries with backoff) sufficient before `Nack`ing the message without requeueing (sending to DLQ)?

## Proposed Changes

### `cmd/logging-service` (New Microservice)

- **[NEW] `cmd/logging-service/main.go`**: The entrypoint for the standalone logging service. Sets up RabbitMQ connection, MongoDB connection, starts the HTTP server (for `/healthz`, `/readyz`, `/metrics`), and starts the AMQP consumer.
- **[NEW] `cmd/logging-service/repository.go`**: Defines the `LogRepository` interface and implements `MongoLogRepository` for `logging_db` -> `events` collection. Will handle index creation on startup.
- **[NEW] `cmd/logging-service/consumer.go`**: Implements the RabbitMQ consumer loop. Handles topology declaration (exchange, queue, dlx, dlq), prefetching, message decoding, validation, deduplication (via MongoDB unique index error handling), and Ack/Nack logic.

### Infrastructure & Deployment

- **[NEW] `docker-compose.logging.yml`**: A Docker Compose file defining the `logging-service` container, connected to `wallet_shared_net`, exposing ports `8090` and `9090`.
- **[MODIFY] `Dockerfile`**: Update to include a build stage for the new `logging-service` binary.

### Documentation Updates

- **[MODIFY] `docs/LOGGING_ARCHITECTURE.md`**: Update the gap register and implementation status to mark Phase 3 tasks as complete.
- **[MODIFY] `docs/LOGGING_IMPLEMENTATION.md`**: Check off tasks in section 4.2 (Task 3.1 to 3.5), update the dashboard status, and add a Phase 3 verification log and deliverables section.

## Verification Plan

### Automated Tests
- Unit tests for the `MongoLogRepository` to verify index creation and idempotent save behavior.
- Unit tests for the consumer logic (simulating successful saves, validation failures routing to DLQ, and transient storage errors).

### Manual Verification
- Deploy `docker-compose.logging.yml`.
- Generate transactions via the `wallet-service` (as documented in Phase 2 Operational Guide).
- Verify events appear in the `logging_db` MongoDB database.
- Send an invalid payload to the `wallet.logs.v1` exchange and verify it ends up in the `wallet.logging.dead.v1` dead-letter queue.
- Access the `http://localhost:8090/healthz` and `http://localhost:9090/metrics` endpoints.
