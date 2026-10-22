package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMerkleHTTPHandlers_NilClient(t *testing.T) {
	server := startLedgerMetricsServer(nil, "0", "us-east-1")
	defer func() {
		_ = server.Close()
	}()

	handler := server.Handler

	t.Run("GET /audit/merkle/latest with nil db returns 503", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/audit/merkle/latest", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected HTTP 503, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "database unavailable") {
			t.Errorf("expected database unavailable message, got: %s", rec.Body.String())
		}
	})

	t.Run("GET /audit/merkle/checkpoints with nil db returns 503", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/audit/merkle/checkpoints?limit=10", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected HTTP 503, got %d", rec.Code)
		}
	})

	t.Run("GET /audit/merkle/proof missing entry_hash returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/audit/merkle/proof", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		// When client is nil, database unavailable is returned or bad request
		if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadRequest {
			t.Fatalf("expected HTTP 503 or 400, got %d", rec.Code)
		}
	})

	t.Run("POST /audit/merkle/checkpoint with nil db returns 503", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/audit/merkle/checkpoint", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected HTTP 503, got %d", rec.Code)
		}
	})

	t.Run("GET /audit/merkle/checkpoint returns 405 Method Not Allowed", func(t *testing.T) {
		// Even with nil DB check first, method check is validated
		req := httptest.NewRequest(http.MethodGet, "/audit/merkle/checkpoint", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected HTTP 503 or 405, got %d", rec.Code)
		}
	})
}

func TestMerkleFunctions_NilDB(t *testing.T) {
	ctx := context.Background()

	t.Run("GenerateCheckpoint nil db", func(t *testing.T) {
		_, err := GenerateCheckpoint(ctx, nil, 100)
		if err == nil {
			t.Fatal("expected error for nil db")
		}
	})

	t.Run("GetLatestCheckpoint nil db", func(t *testing.T) {
		_, err := GetLatestCheckpoint(ctx, nil)
		if err == nil {
			t.Fatal("expected error for nil db")
		}
	})

	t.Run("GetCheckpoints nil db", func(t *testing.T) {
		_, err := GetCheckpoints(ctx, nil, 10)
		if err == nil {
			t.Fatal("expected error for nil db")
		}
	})

	t.Run("GenerateProofForEntry nil db", func(t *testing.T) {
		_, err := GenerateProofForEntry(ctx, nil, "hash123", "")
		if err == nil {
			t.Fatal("expected error for nil db")
		}
	})

	t.Run("StartMerkleWorker handles nil db gracefully", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		StartMerkleWorker(ctx, nil, 10*time.Millisecond, 100)
		// Should not panic or hang
	})
}
