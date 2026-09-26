package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"wallet-system/pkg/observability"
)

// logsResponse is the JSON envelope returned by GET /api/v1/logs.
type logsResponse struct {
	Total  int                   `json:"total"`
	Events []observability.Event `json:"events"`
}

// traceResponse is the JSON envelope returned by GET /api/v1/traces/{association_id}.
type traceResponse struct {
	AssociationID string                `json:"association_id"`
	TotalEvents   int                   `json:"total_events"`
	DurationMS    int64                 `json:"duration_ms"`
	Events        []observability.Event `json:"events"`
}

// registerQueryRoutes mounts the Phase 4 query endpoints on mux.
func registerQueryRoutes(mux *http.ServeMux, repo LogRepository) {
	mux.HandleFunc("/api/v1/logs", handleLogs(repo))
	// Route /api/v1/traces/{association_id} — Go 1.21 ServeMux routes by prefix
	mux.HandleFunc("/api/v1/traces/", handleTrace(repo))
}

// handleLogs implements GET /api/v1/logs with optional query parameters.
func handleLogs(repo LogRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		filter, err := parseQueryFilter(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		events, err := repo.Find(r.Context(), filter)
		if err != nil {
			http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if events == nil {
			events = []observability.Event{}
		}

		resp := logsResponse{
			Total:  len(events),
			Events: events,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func parseQueryFilter(q url.Values) (QueryFilter, error) {
	filter := QueryFilter{
		TransactionID: q.Get("transaction_id"),
		AssociationID: q.Get("association_id"),
		Service:       q.Get("service"),
		Level:         q.Get("level"),
	}

	if fromStr := q.Get("from"); fromStr != "" {
		t, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			return filter, errors.New("invalid 'from' parameter: must be RFC3339 (e.g. 2026-09-14T00:00:00Z)")
		}
		filter.From = t
	}
	if toStr := q.Get("to"); toStr != "" {
		t, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			return filter, errors.New("invalid 'to' parameter: must be RFC3339 (e.g. 2026-09-14T23:59:59Z)")
		}
		filter.To = t
	}
	if limitStr := q.Get("limit"); limitStr != "" {
		n, err := strconv.ParseInt(limitStr, 10, 64)
		if err != nil || n < 0 {
			return filter, errors.New("invalid 'limit' parameter: must be a non-negative integer")
		}
		filter.Limit = n
	}
	if offsetStr := q.Get("offset"); offsetStr != "" {
		n, err := strconv.ParseInt(offsetStr, 10, 64)
		if err != nil || n < 0 {
			return filter, errors.New("invalid 'offset' parameter: must be a non-negative integer")
		}
		filter.Offset = n
	}
	return filter, nil
}

// handleTrace implements GET /api/v1/traces/{association_id}.
// It returns all events for the given association_id sorted by occurred_at ascending,
// and computes the end-to-end duration from the first to the last event.
func handleTrace(repo LogRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Extract association_id from path: /api/v1/traces/{association_id}
		assocID := strings.TrimPrefix(r.URL.Path, "/api/v1/traces/")
		assocID = strings.TrimRight(assocID, "/")
		if assocID == "" {
			http.Error(w, "association_id is required in path: /api/v1/traces/{association_id}", http.StatusBadRequest)
			return
		}

		filter := QueryFilter{
			AssociationID: assocID,
			Limit:         1000, // traces should never exceed a few dozen events
		}
		events, err := repo.Find(r.Context(), filter)
		if err != nil {
			http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if events == nil {
			events = []observability.Event{}
		}

		// Compute end-to-end duration from first to last event
		var durationMS int64
		if len(events) >= 2 {
			first := events[0].OccurredAt
			last := events[len(events)-1].OccurredAt
			durationMS = last.Sub(first).Milliseconds()
		}

		resp := traceResponse{
			AssociationID: assocID,
			TotalEvents:   len(events),
			DurationMS:    durationMS,
			Events:        events,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
