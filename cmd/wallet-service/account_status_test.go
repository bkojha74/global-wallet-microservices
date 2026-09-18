package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWalletModelEffectiveStatus(t *testing.T) {
	tests := []struct {
		name     string
		model    WalletModel
		expected string
	}{
		{
			name:     "empty status defaults to ACTIVE",
			model:    WalletModel{ID: "w-1", Status: ""},
			expected: WalletStatusActive,
		},
		{
			name:     "explicit ACTIVE status",
			model:    WalletModel{ID: "w-2", Status: WalletStatusActive},
			expected: WalletStatusActive,
		},
		{
			name:     "explicit FROZEN status",
			model:    WalletModel{ID: "w-3", Status: WalletStatusFrozen},
			expected: WalletStatusFrozen,
		},
		{
			name:     "explicit CLOSED status",
			model:    WalletModel{ID: "w-4", Status: WalletStatusClosed},
			expected: WalletStatusClosed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := tc.model.EffectiveStatus()
			if actual != tc.expected {
				t.Fatalf("expected status %s, got %s", tc.expected, actual)
			}
		})
	}
}

func TestAdminWalletStatusValidation(t *testing.T) {
	srv := &server{region: "test-region"}

	// 1. Method not allowed (DELETE)
	req := httptest.NewRequest(http.MethodDelete, "/admin/wallet/status", nil)
	rec := httptest.NewRecorder()
	srv.handleAdminWalletStatus(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", rec.Code)
	}

	// 2. GET missing wallet_id
	req = httptest.NewRequest(http.MethodGet, "/admin/wallet/status", nil)
	rec = httptest.NewRecorder()
	srv.handleAdminWalletStatus(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for GET without wallet_id, got %d", rec.Code)
	}
	var getErrResp AdminWalletStatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&getErrResp)
	if getErrResp.Success || getErrResp.Message == "" {
		t.Fatalf("expected error response for missing wallet_id: %+v", getErrResp)
	}

	// 3. POST malformed JSON
	req = httptest.NewRequest(http.MethodPost, "/admin/wallet/status", bytes.NewBufferString("not-valid-json"))
	rec = httptest.NewRecorder()
	srv.handleAdminWalletStatus(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for malformed JSON, got %d", rec.Code)
	}

	// 4. POST missing wallet_id
	payload, _ := json.Marshal(AdminWalletStatusRequest{WalletID: "", Status: "FROZEN"})
	req = httptest.NewRequest(http.MethodPost, "/admin/wallet/status", bytes.NewReader(payload))
	rec = httptest.NewRecorder()
	srv.handleAdminWalletStatus(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for empty wallet_id, got %d", rec.Code)
	}

	// 5. POST invalid status string
	payload, _ = json.Marshal(AdminWalletStatusRequest{WalletID: "alice", Status: "INVALID_STATUS"})
	req = httptest.NewRequest(http.MethodPost, "/admin/wallet/status", bytes.NewReader(payload))
	rec = httptest.NewRecorder()
	srv.handleAdminWalletStatus(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid status, got %d", rec.Code)
	}
}
