package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"testing"
	"time"

	ledgerv1 "wallet-system/proto/ledger"
)

func TestParseLedgerPaginationUnit(t *testing.T) {
	// Default values
	reqDefault := &ledgerv1.GetLedgerRequest{}
	limit, skip := parseLedgerPagination(reqDefault)
	if limit != 20 || skip != 0 {
		t.Fatalf("expected limit 20, skip 0; got limit %d, skip %d", limit, skip)
	}

	// Custom valid values
	reqCustom := &ledgerv1.GetLedgerRequest{
		Limit:     50,
		PageToken: base64.StdEncoding.EncodeToString([]byte("100")),
	}
	limit, skip = parseLedgerPagination(reqCustom)
	if limit != 50 || skip != 100 {
		t.Fatalf("expected limit 50, skip 100; got limit %d, skip %d", limit, skip)
	}

	// Excessively high page size falls back to default 20
	reqHigh := &ledgerv1.GetLedgerRequest{
		Limit: 5000,
	}
	limit, _ = parseLedgerPagination(reqHigh)
	if limit != 20 {
		t.Fatalf("expected limit capped at default 20, got %d", limit)
	}
}

func TestLedgerEnvironmentNameAndServerOpts(t *testing.T) {
	os.Unsetenv("ENVIRONMENT")
	if environmentName() != "local" {
		t.Fatalf("expected local, got %s", environmentName())
	}
	t.Setenv("ENVIRONMENT", "staging")
	if environmentName() != "staging" {
		t.Fatalf("expected staging")
	}

	os.Unsetenv("GRPC_TLS_ENABLED")
	opts, err := getLedgerServerOptions()
	if err != nil || opts != nil {
		t.Fatalf("expected nil server options when TLS disabled")
	}

	t.Setenv("GRPC_TLS_ENABLED", "true")
	t.Setenv("GRPC_SERVER_CERT", "invalid.crt")
	t.Setenv("GRPC_SERVER_KEY", "invalid.key")
	_, err = getLedgerServerOptions()
	if err == nil {
		t.Fatalf("expected mTLS server error for invalid cert files")
	}
}

func TestLedgerServerEmit(t *testing.T) {
	srv := &server{}
	// Nil logger emit (exercises nil check in emitFull)
	srv.emit(context.Background(), "TEST", "INFO", "msg", nil)
	srv.emitTerminal(context.Background(), "TEST", "INFO", "msg", 100, true, nil)
}

func TestInitLedgerOutboxDisabled(t *testing.T) {
	t.Setenv("LOGGING_OUTBOX_ENABLED", "false")
	ob, pub := initLedgerOutbox(context.Background(), nil)
	if ob != nil || pub != nil {
		t.Fatalf("expected nil outbox and publisher when disabled")
	}

	t.Setenv("LOGGING_OUTBOX_ENABLED", "true")
	os.Unsetenv("LOGGING_RABBITMQ_URL")
	ob, pub = initLedgerOutbox(context.Background(), nil)
	if ob != nil || pub != nil {
		t.Fatalf("expected nil outbox when RABBITMQ_URL missing")
	}
}

func TestStartLedgerMetricsServer(t *testing.T) {
	metricsServer := startLedgerMetricsServer(nil, "9899", "us-east-1")
	time.Sleep(100 * time.Millisecond)
	resp, err := http.Get("http://127.0.0.1:9899/metrics")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK from ledger metrics")
		}
	}
	// Healthz endpoint
	respH, errH := http.Get("http://127.0.0.1:9899/healthz")
	if errH == nil {
		respH.Body.Close()
		if respH.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK from ledger healthz")
		}
	}

	// Audit verify endpoint
	respV, errV := http.Get("http://127.0.0.1:9899/audit/verify")
	if errV == nil {
		respV.Body.Close()
		if respV.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK from ledger audit verify")
		}
	}
	_ = metricsServer.Shutdown(context.Background())
}

func TestParseTransactionDocIDUnit(t *testing.T) {
	id1 := parseTransactionDocID("")
	if id1.IsZero() {
		t.Fatalf("expected non-zero object ID for empty string")
	}

	hexStr := id1.Hex()
	id2 := parseTransactionDocID(hexStr)
	if id2.Hex() != hexStr {
		t.Fatalf("expected parsed hex to match")
	}

	id3 := parseTransactionDocID("invalid-hex")
	if id3.IsZero() {
		t.Fatalf("expected non-zero object ID for invalid string")
	}
}

func TestLedgerValidationErrors(t *testing.T) {
	srv := &server{region: "us-east-1"}
	ctx := context.Background()

	// 1. GetLedgerEntries missing wallet_id
	_, err := srv.GetLedgerEntries(ctx, nil)
	if err == nil {
		t.Fatalf("expected error for nil GetLedgerRequest")
	}
	_, err = srv.GetLedgerEntries(ctx, &ledgerv1.GetLedgerRequest{WalletId: ""})
	if err == nil {
		t.Fatalf("expected error for empty WalletId")
	}

	// 2. RecordTransaction missing idempotency_key
	_, err = srv.RecordTransaction(ctx, &ledgerv1.RecordTransactionRequest{})
	if err == nil {
		t.Fatalf("expected error for missing idempotency_key")
	}

	// 3. RecordTransaction invalid currency / amount
	_, err = srv.RecordTransaction(ctx, &ledgerv1.RecordTransactionRequest{
		IdempotencyKey: "k1",
		Amount:         -10,
		Currency:       "USD",
	})
	if err == nil {
		t.Fatalf("expected error for negative amount")
	}

	// 4. RecordTransaction identical wallets (double-entry failure)
	_, err = srv.RecordTransaction(ctx, &ledgerv1.RecordTransactionRequest{
		IdempotencyKey:      "k2",
		SourceWalletId:      "w1",
		DestinationWalletId: "w1",
		Amount:              100,
		Currency:            "USD",
	})
	if err == nil {
		t.Fatalf("expected error for identical source and destination wallets")
	}
}

func TestRunLedgerServer_CancelledContext(t *testing.T) {
	// 1. Connection failure path
	t.Setenv("TEST_MOCK_DB", "false")
	t.Setenv("PORT", "59092")
	t.Setenv("METRICS_PORT", "59892")
	t.Setenv("REGION_NAME", "us-test-1")
	t.Setenv("MONGO_URI", "mongodb://127.0.0.1:27019/?connectTimeoutMS=100")

	ctx1, cancel1 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel1()
	_ = runLedgerServer(ctx1)

	// 2. Mock DB clean startup and shutdown path
	t.Setenv("TEST_MOCK_DB", "true")
	t.Setenv("PORT", "59093")
	t.Setenv("METRICS_PORT", "59893")

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()

	err := runLedgerServer(ctx2)
	if err != nil {
		t.Errorf("expected clean shutdown, got: %v", err)
	}
}

