package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"

	"wallet-system/pkg/coordinator"
	ledgerv1 "wallet-system/proto/ledger"
	walletv1 "wallet-system/proto/wallet"
)

func TestWalletModelEffectiveStatusExtra(t *testing.T) {
	w1 := WalletModel{}
	if w1.EffectiveStatus() != WalletStatusActive {
		t.Fatalf("expected ACTIVE for empty status, got %s", w1.EffectiveStatus())
	}
	w2 := WalletModel{Status: WalletStatusFrozen}
	if w2.EffectiveStatus() != WalletStatusFrozen {
		t.Fatalf("expected FROZEN, got %s", w2.EffectiveStatus())
	}
}

func TestResolveWalletRoleExtra(t *testing.T) {
	os.Unsetenv("COORDINATOR_ROLE")
	if resolveWalletRole(true) != coordinator.TargetPrimary {
		t.Fatalf("expected primary role")
	}
	if resolveWalletRole(false) != coordinator.TargetStandby {
		t.Fatalf("expected standby role")
	}

	t.Setenv("COORDINATOR_ROLE", "custom-role")
	if resolveWalletRole(true) != "custom-role" {
		t.Fatalf("expected custom-role")
	}
}

func TestResolveLedgerAddressExtra(t *testing.T) {
	t.Setenv("LEDGER_SERVICE_ADDR", "custom-ledger:50052")
	if resolveLedgerAddress() != "custom-ledger:50052" {
		t.Fatalf("expected custom-ledger:50052")
	}

	os.Unsetenv("LEDGER_SERVICE_ADDR")
	addr := resolveLedgerAddress()
	if addr == "" {
		t.Fatalf("expected non-empty ledger address")
	}
}

func TestWalletEnvironmentNameExtra(t *testing.T) {
	os.Unsetenv("ENVIRONMENT")
	if environmentName() != "development" {
		t.Fatalf("expected development")
	}
	t.Setenv("ENVIRONMENT", "production")
	if environmentName() != "production" {
		t.Fatalf("expected production")
	}
}

func TestGetOutboundDialOptionAndServerOptions(t *testing.T) {
	os.Unsetenv("GRPC_TLS_ENABLED")
	opt, err := getOutboundDialOption()
	if err != nil || opt == nil {
		t.Fatalf("expected insecure dial option, got error %v", err)
	}
	srvOpts, err := getServerOptions()
	if err != nil || srvOpts != nil {
		t.Fatalf("expected nil server options when TLS disabled")
	}

	t.Setenv("GRPC_TLS_ENABLED", "true")
	t.Setenv("GRPC_CLIENT_CERT", "invalid.crt")
	t.Setenv("GRPC_CLIENT_KEY", "invalid.key")
	t.Setenv("GRPC_CA_CERT", "invalid.ca")
	_, err = getOutboundDialOption()
	if err == nil {
		t.Fatalf("expected mTLS error for invalid cert files")
	}

	t.Setenv("GRPC_SERVER_CERT", "invalid.crt")
	t.Setenv("GRPC_SERVER_KEY", "invalid.key")
	_, err = getServerOptions()
	if err == nil {
		t.Fatalf("expected mTLS server error for invalid cert files")
	}
}

func TestServerIsWritableAndEmit(t *testing.T) {
	srv := &server{
		isActive: true,
	}
	if !srv.isWritable(context.Background()) {
		t.Fatalf("expected active server to be writable")
	}

	// Test emit when srv.logger is nil (exercises nil logger check safely)
	srv.emit(context.Background(), "TEST_EVENT", "INFO", "test msg", nil)
	srv.emitTerminal(context.Background(), "TEST_EVENT", "INFO", "test msg", 10, true, nil)
}

func TestInitWalletOutboxDisabled(t *testing.T) {
	t.Setenv("LOGGING_OUTBOX_ENABLED", "false")
	ob, pub := initWalletOutbox(context.Background(), nil)
	if ob != nil || pub != nil {
		t.Fatalf("expected nil outbox and publisher when disabled")
	}

	t.Setenv("LOGGING_OUTBOX_ENABLED", "true")
	os.Unsetenv("LOGGING_RABBITMQ_URL")
	ob, pub = initWalletOutbox(context.Background(), nil)
	if ob != nil || pub != nil {
		t.Fatalf("expected nil outbox when RABBITMQ_URL missing")
	}
}

func TestStartWalletMetricsServerAndShutdown(t *testing.T) {
	t.Setenv("METRICS_PORT", "9898")
	srv := &server{isActive: true}
	metricsServer := startWalletMetricsServer(srv, "us-east-1", true)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get("http://127.0.0.1:9898/healthz")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK from healthz")
		}
	}

	// Test shutdown helper
	grpcSrv := grpc.NewServer()
	healthSrv := health.NewServer()
	handleWalletShutdown(grpcSrv, healthSrv, metricsServer, nil, nil, nil)
}

func TestAdminWalletStatusHandlersUnit(t *testing.T) {
	srv := &server{isActive: true}

	// Invalid JSON body
	req := httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader("bad-json"))
	w := httptest.NewRecorder()
	srv.handleAdminWalletStatusUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad json, got %d", w.Code)
	}

	// Missing wallet_id
	req = httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader(`{"status":"FROZEN"}`))
	w = httptest.NewRecorder()
	srv.handleAdminWalletStatusUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing wallet_id, got %d", w.Code)
	}

	// Invalid status
	req = httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader(`{"wallet_id":"w1","status":"INVALID"}`))
	w = httptest.NewRecorder()
	srv.handleAdminWalletStatusUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid status, got %d", w.Code)
	}

	// Valid payload with nil DB
	req = httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader(`{"wallet_id":"w1","status":"FROZEN"}`))
	w = httptest.NewRecorder()
	srv.handleAdminWalletStatusUpdate(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for nil DB update, got %d", w.Code)
	}

	// GET admin wallet status (nil DB)
	reqGet := httptest.NewRequest(http.MethodGet, "/admin/wallet/status?wallet_id=w1", nil)
	wGet := httptest.NewRecorder()
	srv.handleAdminWalletStatus(wGet, reqGet)
	if wGet.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for nil DB get, got %d", wGet.Code)
	}

	// GET admin wallet status (missing wallet_id)
	reqGetMissing := httptest.NewRequest(http.MethodGet, "/admin/wallet/status", nil)
	wGetMissing := httptest.NewRecorder()
	srv.handleAdminWalletStatus(wGetMissing, reqGetMissing)
	if wGetMissing.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing wallet_id, got %d", wGetMissing.Code)
	}

	// PUT routing
	reqPut := httptest.NewRequest(http.MethodPut, "/admin/wallet/status", strings.NewReader("bad-json"))
	wPut := httptest.NewRecorder()
	srv.handleAdminWalletStatus(wPut, reqPut)
	if wPut.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for PUT bad json, got %d", wPut.Code)
	}

	// Method Not Allowed
	reqDelete := httptest.NewRequest(http.MethodDelete, "/admin/wallet/status", nil)
	wDelete := httptest.NewRecorder()
	srv.handleAdminWalletStatus(wDelete, reqDelete)
	if wDelete.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for DELETE, got %d", wDelete.Code)
	}
}

func TestDispatchImmediateLedgerUnit(t *testing.T) {
	srv := &server{
		ledgerRelay: nil,
	}
	req := &walletv1.TransferFundsRequest{
		SourceWalletId:      "w1",
		DestinationWalletId: "w2",
		Amount:              &walletv1.Money{Units: 100, Currency: "USD"},
		IdempotencyKey:      "idemp-1",
	}
	task := LedgerTask{TransactionID: "tx1"}

	// 1. Nil relay
	srv.dispatchImmediateLedger(context.Background(), req, task, "tx1")

	// 2. Failing relay
	mockFail := &mockLedgerClient{recordErr: fmt.Errorf("rpc failed")}
	srv.ledgerRelay = &LedgerRelay{ledgerClient: mockFail}
	srv.dispatchImmediateLedger(context.Background(), req, task, "tx1")

	// 3. Succeeding relay
	mockSucc := &mockLedgerClient{recordResp: &ledgerv1.RecordTransactionResponse{Success: true}}
	srv.ledgerRelay = &LedgerRelay{ledgerClient: mockSucc}
	srv.dispatchImmediateLedger(context.Background(), req, task, "tx1")
}

func TestLedgerRelayAndDispatchImmediateUnit(t *testing.T) {
	relay := NewLedgerRelay(nil, nil, 0, 0, nil)
	if relay.pollInterval != 1*time.Second || relay.batchSize != 50 {
		t.Fatalf("expected default pollInterval and batchSize")
	}

	task := LedgerTask{
		ID:            primitive.NewObjectID(),
		TransactionID: "tx1",
	}

	// DispatchImmediate failure path
	mockFail := &mockLedgerClient{recordErr: fmt.Errorf("rpc failed")}
	relayFail := &LedgerRelay{ledgerClient: mockFail}
	err := relayFail.DispatchImmediate(context.Background(), task)
	if err == nil {
		t.Fatalf("expected error from DispatchImmediate on rpc failure")
	}

	// DispatchImmediate success path
	mockSucc := &mockLedgerClient{recordResp: &ledgerv1.RecordTransactionResponse{Success: true}}
	relaySucc := &LedgerRelay{ledgerClient: mockSucc}
	_ = relaySucc.DispatchImmediate(context.Background(), task)
}

func TestRunWalletServer_CancelledContext(t *testing.T) {
	// 1. Connection failure path
	t.Setenv("TEST_MOCK_DB", "false")
	t.Setenv("PORT", "59051")
	t.Setenv("METRICS_PORT", "59898")
	t.Setenv("REGION_NAME", "us-test-1")
	t.Setenv("ENVIRONMENT", "test")
	t.Setenv("MONGO_URI", "mongodb://127.0.0.1:27019/?connectTimeoutMS=100")

	ctx1, cancel1 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel1()
	_ = runWalletServer(ctx1)

	// 2. Mock DB clean startup and shutdown path
	t.Setenv("TEST_MOCK_DB", "true")
	t.Setenv("PORT", "59052")
	t.Setenv("METRICS_PORT", "59899")

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()

	err := runWalletServer(ctx2)
	if err != nil {
		t.Errorf("expected clean shutdown, got: %v", err)
	}
}

func TestWalletServer_ValidationAndStatusChecks(t *testing.T) {
	srv := &server{
		isActive: true,
		region:   "us-east-1",
	}
	ctx := context.Background()

	// 1. validateTransferRequest - nil req
	_, err := srv.validateTransferRequest(ctx, nil, time.Now())
	if err == nil {
		t.Fatalf("expected error for nil transfer request")
	}

	// 2. validateTransferRequest - missing fields
	_, err = srv.validateTransferRequest(ctx, &walletv1.TransferFundsRequest{}, time.Now())
	if err == nil {
		t.Fatalf("expected error for missing fields")
	}

	// 3. validateTransferRequest - invalid amount
	_, err = srv.validateTransferRequest(ctx, &walletv1.TransferFundsRequest{
		IdempotencyKey:      "k1",
		SourceWalletId:      "w1",
		DestinationWalletId: "w2",
		Amount:              &walletv1.Money{Units: -100, Currency: "USD"},
	}, time.Now())
	if err == nil {
		t.Fatalf("expected error for negative amount")
	}

	// 4. validateTransferRequest - identical wallets
	respIdentical, errIdentical := srv.validateTransferRequest(ctx, &walletv1.TransferFundsRequest{
		IdempotencyKey:      "k1",
		SourceWalletId:      "w1",
		DestinationWalletId: "w1",
		Amount:              &walletv1.Money{Units: 100, Currency: "USD"},
	}, time.Now())
	if errIdentical != nil || respIdentical == nil || respIdentical.Status == walletv1.TransferFundsResponse_SUCCESS {
		t.Fatalf("expected failed response for identical wallets")
	}

	// 5. validateTransferRequest - standby write rejected
	standbySrv := &server{isActive: false, region: "us-west-2"}
	_, errStandby := standbySrv.validateTransferRequest(ctx, &walletv1.TransferFundsRequest{
		IdempotencyKey:      "k1",
		SourceWalletId:      "w1",
		DestinationWalletId: "w2",
		Amount:              &walletv1.Money{Units: 100, Currency: "USD"},
	}, time.Now())
	if errStandby == nil {
		t.Fatalf("expected standby error")
	}

	// 6. checkWalletStatus
	msgF, errF := checkWalletStatus(WalletModel{ID: "w1", Status: WalletStatusFrozen}, "source")
	if errF == nil || msgF == "" {
		t.Fatalf("expected error for FROZEN status")
	}

	msgC, errC := checkWalletStatus(WalletModel{ID: "w1", Status: WalletStatusClosed}, "destination")
	if errC == nil || msgC == "" {
		t.Fatalf("expected error for CLOSED status")
	}

	msgA, errA := checkWalletStatus(WalletModel{ID: "w1", Status: WalletStatusActive}, "source")
	if errA != nil || msgA != "" {
		t.Fatalf("expected no error for ACTIVE status")
	}
}


