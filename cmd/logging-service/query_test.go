package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wallet-system/pkg/observability"
)

// stubRepo is an in-memory LogRepository for unit tests.
type stubRepo struct {
	events  []observability.Event
	findErr error
}

func (s *stubRepo) Save(_ context.Context, e observability.Event) error {
	s.events = append(s.events, e)
	return nil
}

func (s *stubRepo) Find(_ context.Context, filter QueryFilter) ([]observability.Event, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}

	var result []observability.Event
	for _, e := range s.events {
		if filter.TransactionID != "" && e.TransactionID != filter.TransactionID {
			continue
		}
		if filter.AssociationID != "" && e.AssociationID != filter.AssociationID {
			continue
		}
		if filter.Service != "" && e.Service != filter.Service {
			continue
		}
		if filter.Level != "" && e.Level != filter.Level {
			continue
		}
		if !filter.From.IsZero() && e.OccurredAt.Before(filter.From) {
			continue
		}
		if !filter.To.IsZero() && e.OccurredAt.After(filter.To) {
			continue
		}
		result = append(result, e)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := filter.Offset
	if offset > int64(len(result)) {
		return []observability.Event{}, nil
	}
	result = result[offset:]
	if int64(len(result)) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *stubRepo) Health(_ context.Context) error { return nil }

func (s *stubRepo) Retention(_ context.Context, _ int) (int64, error) { return 0, nil }

// makeEvent is a helper that builds a minimal valid event.
func makeEvent(assocID, txID, service, level string, occurredAt time.Time) observability.Event {
	b := true
	return observability.Event{
		SchemaVersion: 1,
		EventID:       assocID + "-" + service + "-" + level,
		OccurredAt:    occurredAt,
		Service:       service,
		Environment:   "test",
		Level:         level,
		EventType:     "test.event",
		AssociationID: assocID,
		TransactionID: txID,
		Success:       &b,
	}
}

// ── GET /api/v1/logs ──────────────────────────────────────────────────────────

func TestHandleLogs_NoParams_ReturnsAll(t *testing.T) {
	now := time.Now().UTC()
	repo := &stubRepo{events: []observability.Event{
		makeEvent("assoc-1", "tx-1", "api-gateway", observability.LevelInfo, now),
		makeEvent("assoc-1", "tx-1", "wallet-service", observability.LevelAudit, now.Add(time.Millisecond)),
	}}

	mux := http.NewServeMux()
	registerQueryRoutes(mux, repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp logsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal("decode:", err)
	}
	if resp.Total != 2 {
		t.Errorf("expected 2 events, got %d", resp.Total)
	}
}

func TestHandleLogs_FilterByTransactionID(t *testing.T) {
	now := time.Now().UTC()
	repo := &stubRepo{events: []observability.Event{
		makeEvent("assoc-1", "tx-A", "api-gateway", observability.LevelInfo, now),
		makeEvent("assoc-2", "tx-B", "wallet-service", observability.LevelAudit, now.Add(time.Millisecond)),
	}}

	mux := http.NewServeMux()
	registerQueryRoutes(mux, repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?transaction_id=tx-A", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp logsResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Total != 1 {
		t.Errorf("expected 1 event for tx-A, got %d", resp.Total)
	}
	if resp.Events[0].TransactionID != "tx-A" {
		t.Errorf("expected tx-A, got %s", resp.Events[0].TransactionID)
	}
}

func TestHandleLogs_FilterByLevelAndService(t *testing.T) {
	now := time.Now().UTC()
	repo := &stubRepo{events: []observability.Event{
		makeEvent("a1", "tx-1", "wallet-service", observability.LevelAudit, now),
		makeEvent("a1", "tx-1", "wallet-service", observability.LevelInfo, now.Add(time.Millisecond)),
		makeEvent("a1", "tx-1", "api-gateway", observability.LevelAudit, now.Add(2*time.Millisecond)),
	}}

	mux := http.NewServeMux()
	registerQueryRoutes(mux, repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?level=AUDIT&service=wallet-service", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp logsResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Total != 1 {
		t.Errorf("expected 1 AUDIT wallet-service event, got %d", resp.Total)
	}
}

func TestHandleLogs_FilterByTimeWindow(t *testing.T) {
	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	repo := &stubRepo{events: []observability.Event{
		makeEvent("a1", "tx-1", "svc", observability.LevelInfo, base),
		makeEvent("a1", "tx-1", "svc", observability.LevelInfo, base.Add(2*time.Hour)),
		makeEvent("a1", "tx-1", "svc", observability.LevelInfo, base.Add(4*time.Hour)),
	}}

	mux := http.NewServeMux()
	registerQueryRoutes(mux, repo)

	from := base.Add(time.Hour).Format(time.RFC3339)
	to := base.Add(3 * time.Hour).Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?from="+from+"&to="+to, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp logsResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Total != 1 {
		t.Errorf("expected 1 event in window, got %d", resp.Total)
	}
}

func TestHandleLogs_InvalidFrom_Returns400(t *testing.T) {
	mux := http.NewServeMux()
	registerQueryRoutes(mux, &stubRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?from=not-a-date", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleLogs_InvalidLimit_Returns400(t *testing.T) {
	mux := http.NewServeMux()
	registerQueryRoutes(mux, &stubRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?limit=abc", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleLogs_RepoError_Returns500(t *testing.T) {
	repo := &stubRepo{findErr: fmt.Errorf("mongo timeout")}

	mux := http.NewServeMux()
	registerQueryRoutes(mux, repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestHandleLogs_MethodNotAllowed(t *testing.T) {
	mux := http.NewServeMux()
	registerQueryRoutes(mux, &stubRepo{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/logs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

// ── GET /api/v1/traces/{association_id} ───────────────────────────────────────

func TestHandleTrace_Returns12EventsInOrder(t *testing.T) {
	assocID := "trace-assoc-001"
	base := time.Now().UTC()
	var events []observability.Event
	for i := 0; i < 12; i++ {
		events = append(events, makeEvent(assocID, "tx-1", "svc", observability.LevelInfo, base.Add(time.Duration(i)*time.Millisecond)))
	}
	repo := &stubRepo{events: events}

	mux := http.NewServeMux()
	registerQueryRoutes(mux, repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/traces/"+assocID, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp traceResponse
	json.NewDecoder(rec.Body).Decode(&resp)

	if resp.AssociationID != assocID {
		t.Errorf("expected assocID %q, got %q", assocID, resp.AssociationID)
	}
	if resp.TotalEvents != 12 {
		t.Errorf("expected 12 events, got %d", resp.TotalEvents)
	}
	// 12 events each 1ms apart = 11ms total
	if resp.DurationMS != 11 {
		t.Errorf("expected duration_ms 11, got %d", resp.DurationMS)
	}
	// Verify chronological order
	for i := 1; i < len(resp.Events); i++ {
		if resp.Events[i].OccurredAt.Before(resp.Events[i-1].OccurredAt) {
			t.Errorf("events not in chronological order at index %d", i)
		}
	}
}

func TestHandleTrace_MissingAssociationID_Returns400(t *testing.T) {
	mux := http.NewServeMux()
	registerQueryRoutes(mux, &stubRepo{})

	// Path with trailing slash but no ID
	req := httptest.NewRequest(http.MethodGet, "/api/v1/traces/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleTrace_RepoError_Returns500(t *testing.T) {
	repo := &stubRepo{findErr: fmt.Errorf("db down")}

	mux := http.NewServeMux()
	registerQueryRoutes(mux, repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/traces/some-assoc-id", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestHandleTrace_EmptyResult_ReturnsDurationZero(t *testing.T) {
	repo := &stubRepo{events: []observability.Event{}} // nothing in DB

	mux := http.NewServeMux()
	registerQueryRoutes(mux, repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/traces/unknown-assoc", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp traceResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.TotalEvents != 0 {
		t.Errorf("expected 0 events, got %d", resp.TotalEvents)
	}
	if resp.DurationMS != 0 {
		t.Errorf("expected duration_ms 0, got %d", resp.DurationMS)
	}
}

func TestHandleTrace_MethodNotAllowed(t *testing.T) {
	mux := http.NewServeMux()
	registerQueryRoutes(mux, &stubRepo{})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/traces/some-id", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestParseFilterValidationBranches(t *testing.T) {
	// 1. Invalid 'to' parameter
	reqBadTo := httptest.NewRequest(http.MethodGet, "/api/v1/logs?to=invalid-date", nil)
	_, err := parseQueryFilter(reqBadTo.URL.Query())
	if err == nil || !strings.Contains(err.Error(), "invalid 'to' parameter") {
		t.Fatalf("expected invalid 'to' parameter error, got %v", err)
	}

	// 2. Valid 'to' parameter
	reqGoodTo := httptest.NewRequest(http.MethodGet, "/api/v1/logs?to=2026-09-23T12:00:00Z", nil)
	fTo, err := parseQueryFilter(reqGoodTo.URL.Query())
	if err != nil || fTo.To.IsZero() {
		t.Fatalf("expected parsed 'to' time, got %v, err=%v", fTo.To, err)
	}

	// 3. Invalid 'offset' parameter (negative or non-int)
	reqBadOffset := httptest.NewRequest(http.MethodGet, "/api/v1/logs?offset=abc", nil)
	_, err = parseQueryFilter(reqBadOffset.URL.Query())
	if err == nil || !strings.Contains(err.Error(), "invalid 'offset' parameter") {
		t.Fatalf("expected invalid 'offset' parameter error, got %v", err)
	}

	reqNegOffset := httptest.NewRequest(http.MethodGet, "/api/v1/logs?offset=-5", nil)
	_, err = parseQueryFilter(reqNegOffset.URL.Query())
	if err == nil || !strings.Contains(err.Error(), "invalid 'offset' parameter") {
		t.Fatalf("expected invalid 'offset' parameter error, got %v", err)
	}

	// 4. Valid limit and offset
	reqValid := httptest.NewRequest(http.MethodGet, "/api/v1/logs?limit=25&offset=50", nil)
	fValid, err := parseQueryFilter(reqValid.URL.Query())
	if err != nil || fValid.Limit != 25 || fValid.Offset != 50 {
		t.Fatalf("expected limit=25, offset=50, got limit=%d, offset=%d, err=%v", fValid.Limit, fValid.Offset, err)
	}
}
