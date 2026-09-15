# Phase 4 — Query API & Operational Tracing

Add HTTP query endpoints to the existing `logging-service` so any stakeholder can retrieve
the full 12-step transaction timeline by `association_id` or `transaction_id`, filter logs
by service/level/time range, and inspect the DLQ status — all from a single REST surface.

---

## Proposed Changes

### `cmd/logging-service` — Core service

---

#### [MODIFY] [repository.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/logging-service/repository.go)

Extend the `LogRepository` interface with a `Find` method and implement it on `MongoLogRepository`.

**New interface:**
```go
type QueryFilter struct {
    TransactionID  string
    AssociationID  string
    Service        string
    Level          string
    From           time.Time
    To             time.Time
    Limit          int64
    Offset         int64
}

type LogRepository interface {
    Save(ctx context.Context, event observability.Event) error
    Find(ctx context.Context, filter QueryFilter) ([]observability.Event, error)
    Health(ctx context.Context) error
}
```

`Find` builds a MongoDB `bson.D` filter from whichever `QueryFilter` fields are non-zero,
sorts by `occurred_at ASC`, and uses `Skip`/`Limit` for pagination. All fields are optional —
an empty filter returns the most recent events (up to `Limit`, which defaults to 100).

---

#### [NEW] [query.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/logging-service/query.go)

New file — HTTP handler wiring for Phase 4 endpoints. Keeps `main.go` clean.

**Endpoints:**

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/logs` | Filtered log search |
| `GET` | `/api/v1/traces/{association_id}` | Full trace reconstruction |

**`GET /api/v1/logs` query parameters:**

| Param | Type | Description |
|---|---|---|
| `transaction_id` | string | Filter by transaction |
| `association_id` | string | Filter by correlation |
| `service` | string | Filter by service name |
| `level` | string | Filter by level (AUDIT, INFO, …) |
| `from` | RFC3339 | Start of time window |
| `to` | RFC3339 | End of time window |
| `limit` | int | Max results (default 100, max 1000) |
| `offset` | int | Pagination offset |

**`GET /api/v1/traces/{association_id}` response:**

```json
{
  "association_id": "9f5f2d20...",
  "total_events": 12,
  "duration_ms": 42,
  "events": [ ...ordered by occurred_at ASC... ]
}
```

The trace endpoint additionally computes the total duration from the `occurred_at` of
event #1 to the `occurred_at` of the last event, and groups events by step number using
the known 12-step catalog.

---

#### [MODIFY] [main.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/logging-service/main.go)

Register the two new query handlers on the existing `http.ServeMux` after mounting
`/healthz` and `/readyz`.

---

#### [NEW] [query_test.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/logging-service/query_test.go)

Unit tests using `httptest` and a stub `LogRepository` (in-memory fake):

- `GET /api/v1/logs` with no params returns up to 100 events.
- `GET /api/v1/logs?transaction_id=X` calls `Find` with the correct filter.
- `GET /api/v1/logs?level=AUDIT&service=wallet-service` applies both filters.
- `GET /api/v1/logs?from=...&to=...` applies the time window.
- `GET /api/v1/traces/{association_id}` returns events sorted by `occurred_at`.
- `GET /api/v1/traces/{association_id}` with 12 events computes `duration_ms` correctly.
- Missing `association_id` returns `400 Bad Request`.
- Repo error on `Find` returns `500 Internal Server Error`.

---

### Documentation

---

#### [MODIFY] [LOGGING_IMPLEMENTATION.md](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docs/LOGGING_IMPLEMENTATION.md)

- Mark Phase 4 `IN PROGRESS` → `COMPLETE (100%)` in the dashboard.
- Add Phase 4 deliverables table (new files, modified files).
- Check off all Phase 4 checklist items.
- Add verification log with `curl` examples.

#### [MODIFY] [LOGGING_ARCHITECTURE.md](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docs/LOGGING_ARCHITECTURE.md)

- Update Section 16 implementation status — Phase 4 rows → ✅ Done.

---

## Verification Plan

### Automated Tests
```powershell
go test ./cmd/logging-service/... -v -timeout 30s
```
All existing Phase 3 tests must remain green. New Phase 4 tests must pass.

### Manual Verification

With all services running (MongoDB, RabbitMQ, ledger, wallet x2, api-gateway, logging-service):

```powershell
# Execute a transfer
$body = @{ idempotency_key = "p4-tx-001"; source_wallet_id = "bipin"
           destination_wallet_id = "ruby"; amount = 10; currency = "USD"
         } | ConvertTo-Json -Compress
$res = Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8080/api/v1/transfers `
       -ContentType application/json -Body $body
$txId = $res.transaction_id

# Wait 1 second for logging-service to drain queue, then query
Start-Sleep 1

# 1. Filter by transaction_id
curl "http://127.0.0.1:8090/api/v1/logs?transaction_id=$txId"

# 2. Get full trace (replace <assoc_id> with the X-Association-ID from the gateway log)
curl "http://127.0.0.1:8090/api/v1/traces/<assoc_id>"
```

Expected: 12 events returned, all carrying the same `association_id`, sorted chronologically.
