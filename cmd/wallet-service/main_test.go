package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	ledgerv1 "wallet-system/proto/ledger"
	walletv1 "wallet-system/proto/wallet"
)

func TestTransferFundsRejectsIdenticalWalletsWithoutDatabase(t *testing.T) {
	service := &server{region: "test-region"}
	response, err := service.TransferFunds(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey:      "duplicate-wallet-test",
		SourceWalletId:      "alice",
		DestinationWalletId: "alice",
		Amount:              &walletv1.Money{Currency: "USD", Units: 10},
	})

	if err != nil {
		t.Fatalf("expected application response, got error: %v", err)
	}
	if response.Status != walletv1.TransferFundsResponse_INTERNAL_ERROR {
		t.Fatalf("expected INTERNAL_ERROR, got %s", response.Status.String())
	}
	if response.ErrorMessage != "source and destination wallets cannot be identical" {
		t.Fatalf("unexpected error message: %q", response.ErrorMessage)
	}
	if response.HandledByRegion != "test-region" {
		t.Fatalf("expected test region, got %q", response.HandledByRegion)
	}
}

func TestTransferFundsRejectsMissingWalletIDs(t *testing.T) {
	service := &server{region: "test-region"}
	response, err := service.TransferFunds(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey: "invalid-transfer-test",
		Amount:         &walletv1.Money{Currency: "USD", Units: 50},
	})

	if response != nil {
		t.Fatalf("expected no response, got %+v", response)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "required") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestTransferFundsRejectsInvalidCurrency(t *testing.T) {
	service := &server{region: "test-region"}
	response, err := service.TransferFunds(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey:      "invalid-curr-test",
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              &walletv1.Money{Currency: "FAKECURR", Units: 50},
	})

	if response != nil {
		t.Fatalf("expected no response, got %+v", response)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "invalid or unsupported currency") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestTransferFundsRejectsNonPositiveAmount(t *testing.T) {
	service := &server{region: "test-region"}
	response, err := service.TransferFunds(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey:      "zero-amount-test",
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              &walletv1.Money{Currency: "USD", Units: 0},
	})

	if response != nil {
		t.Fatalf("expected no response, got %+v", response)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestCreateWalletValidation(t *testing.T) {
	service := &server{region: "test-region"}

	// Invalid currency
	_, err := service.CreateWallet(context.Background(), &walletv1.CreateWalletRequest{
		WalletId:       "user-1",
		Currency:       "XYZ",
		InitialBalance: 100,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for bad currency, got %v", err)
	}

	// Negative balance
	_, err = service.CreateWallet(context.Background(), &walletv1.CreateWalletRequest{
		WalletId:       "user-2",
		Currency:       "USD",
		InitialBalance: -50,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for negative balance, got %v", err)
	}

	// Missing wallet ID
	_, err = service.CreateWallet(context.Background(), &walletv1.CreateWalletRequest{
		WalletId:       "",
		Currency:       "USD",
		InitialBalance: 100,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty wallet ID, got %v", err)
	}
}

func TestGetBalanceRejectsMissingWalletID(t *testing.T) {
	service := &server{region: "test-region"}
	_, err := service.GetBalance(context.Background(), &walletv1.GetBalanceRequest{WalletId: ""})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty wallet ID, got %v", err)
	}
}

func TestResolveWalletRole(t *testing.T) {
	t.Setenv("COORDINATOR_ROLE", "CUSTOM_ROLE")
	if r := resolveWalletRole(true); r != "CUSTOM_ROLE" {
		t.Fatalf("expected CUSTOM_ROLE, got %s", r)
	}

	t.Setenv("COORDINATOR_ROLE", "")
	if r := resolveWalletRole(true); r != "PRIMARY" {
		t.Fatalf("expected PRIMARY when isActive=true, got %s", r)
	}
	if r := resolveWalletRole(false); r != "STANDBY" {
		t.Fatalf("expected STANDBY when isActive=false, got %s", r)
	}
}

func TestResolveLedgerAddress(t *testing.T) {
	t.Setenv("LEDGER_SERVICE_ADDR", "custom-ledger:50052")
	if addr := resolveLedgerAddress(); addr != "custom-ledger:50052" {
		t.Fatalf("expected custom-ledger:50052, got %s", addr)
	}

	t.Setenv("LEDGER_SERVICE_ADDR", "")
	addrDefault := resolveLedgerAddress()
	if addrDefault == "" {
		t.Fatal("expected non-empty fallback ledger address")
	}
}

func TestSetupWalletGRPCServer(t *testing.T) {
	grpcServer, healthServer, lis, err := setupWalletGRPCServer("0")
	if err != nil {
		t.Fatalf("failed to setup wallet gRPC server: %v", err)
	}
	defer lis.Close()
	defer grpcServer.Stop()

	if grpcServer == nil || healthServer == nil {
		t.Fatal("expected non-nil grpcServer and healthServer")
	}
}

func TestHandleTransferFailure(t *testing.T) {
	s := &server{region: "us-east-1"}
	req := &walletv1.TransferFundsRequest{
		IdempotencyKey:      "fail-key-1",
		SourceWalletId:      "w-src",
		DestinationWalletId: "w-dst",
		Amount:              &walletv1.Money{Units: 50, Currency: "USD"},
	}
	state := transferExecutionState{
		finalTxnID:    "tx-fail-1",
		txnStatus:     walletv1.TransferFundsResponse_SUCCESS,
		sourceDebited: true,
	}

	resp := s.handleTransferFailure(context.Background(), req, &state, status.Error(codes.Aborted, "db abort"), 10)
	if resp == nil {
		t.Fatal("expected non-nil failure response")
	}
	if resp.Status != walletv1.TransferFundsResponse_INTERNAL_ERROR {
		t.Fatalf("expected INTERNAL_ERROR, got %v", resp.Status)
	}
	if resp.HandledByRegion != "us-east-1" {
		t.Fatalf("expected us-east-1, got %s", resp.HandledByRegion)
	}
}

func TestHandleTransferSuccess(t *testing.T) {
	s := &server{region: "us-east-1"}
	req := &walletv1.TransferFundsRequest{
		IdempotencyKey:      "succ-key-1",
		SourceWalletId:      "w-src",
		DestinationWalletId: "w-dst",
		Amount:              &walletv1.Money{Units: 50, Currency: "USD"},
	}
	state := transferExecutionState{
		finalTxnID: "tx-succ-1",
		txnStatus:  walletv1.TransferFundsResponse_SUCCESS,
	}

	// Should execute cleanly without panics
	s.handleTransferSuccess(context.Background(), req, &state, 15)
}

func TestEnvironmentName(t *testing.T) {
	t.Setenv("ENVIRONMENT", "staging")
	if env := environmentName(); env != "staging" {
		t.Fatalf("expected staging, got %s", env)
	}

	t.Setenv("ENVIRONMENT", "")
	if env := environmentName(); env != "development" {
		t.Fatalf("expected development, got %s", env)
	}
}

func TestCheckWalletStatus(t *testing.T) {
	// Active wallet
	activeWallet := WalletModel{ID: "w-act", Status: WalletStatusActive}
	msg, err := checkWalletStatus(activeWallet, "source")
	if err != nil || msg != "" {
		t.Fatalf("expected active wallet to succeed, got %v, %s", err, msg)
	}

	// Frozen wallet
	frozenWallet := WalletModel{ID: "w-frz", Status: WalletStatusFrozen}
	msg, err = checkWalletStatus(frozenWallet, "source")
	if err == nil || !strings.Contains(msg, "FROZEN") {
		t.Fatalf("expected frozen wallet error, got %v, %s", err, msg)
	}

	// Closed wallet
	closedWallet := WalletModel{ID: "w-cls", Status: WalletStatusClosed}
	msg, err = checkWalletStatus(closedWallet, "destination")
	if err == nil || !strings.Contains(msg, "CLOSED") {
		t.Fatalf("expected closed wallet error, got %v, %s", err, msg)
	}
}

func TestValidateTransferRequestBranches(t *testing.T) {
	sActive := &server{region: "us-east-1", isActive: true}
	sStandby := &server{region: "us-west-2", isActive: false}

	// 1. nil request
	_, err := sActive.validateTransferRequest(context.Background(), nil, time.Now())
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for nil req, got %v", err)
	}

	// 2. missing fields
	_, err = sActive.validateTransferRequest(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey: "",
	}, time.Now())
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing fields, got %v", err)
	}

	// 3. invalid amount
	_, err = sActive.validateTransferRequest(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey:      "k1",
		SourceWalletId:      "w1",
		DestinationWalletId: "w2",
		Amount:              &walletv1.Money{Units: -10, Currency: "USD"},
	}, time.Now())
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for negative amount, got %v", err)
	}

	// 4. identical wallets
	resp, err := sActive.validateTransferRequest(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey:      "k2",
		SourceWalletId:      "w1",
		DestinationWalletId: "w1",
		Amount:              &walletv1.Money{Units: 50, Currency: "USD"},
	}, time.Now())
	if err != nil || resp == nil || resp.Status != walletv1.TransferFundsResponse_INTERNAL_ERROR {
		t.Fatalf("expected INTERNAL_ERROR response for identical wallets, got resp=%+v, err=%v", resp, err)
	}

	// 5. standby replica write rejection
	respStandby, errStandby := sStandby.validateTransferRequest(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey:      "k3",
		SourceWalletId:      "w1",
		DestinationWalletId: "w2",
		Amount:              &walletv1.Money{Units: 50, Currency: "USD"},
	}, time.Now())
	if status.Code(errStandby) != codes.FailedPrecondition || respStandby == nil {
		t.Fatalf("expected FailedPrecondition on standby, got resp=%+v, err=%v", respStandby, errStandby)
	}
}

func TestIsWritable(t *testing.T) {
	srv := &server{isActive: true}
	if !srv.isWritable(context.Background()) {
		t.Fatal("expected isWritable true when isActive=true")
	}

	srv.isActive = false
	if srv.isWritable(context.Background()) {
		t.Fatal("expected isWritable false when isActive=false")
	}
}

func TestAdminWalletStatusHandlers(t *testing.T) {
	srv := &server{}

	// 1. Method Not Allowed
	reqDelete := httptest.NewRequest(http.MethodDelete, "/admin/wallet/status", nil)
	wDelete := httptest.NewRecorder()
	srv.handleAdminWalletStatus(wDelete, reqDelete)
	if wDelete.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", wDelete.Code)
	}

	// 2. GET missing wallet_id
	reqGetMissing := httptest.NewRequest(http.MethodGet, "/admin/wallet/status", nil)
	wGetMissing := httptest.NewRecorder()
	srv.handleAdminWalletStatus(wGetMissing, reqGetMissing)
	if wGetMissing.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", wGetMissing.Code)
	}

	// 3. POST invalid JSON
	reqBadJSON := httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader("bad-json"))
	wBadJSON := httptest.NewRecorder()
	srv.handleAdminWalletStatus(wBadJSON, reqBadJSON)
	if wBadJSON.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad json, got %d", wBadJSON.Code)
	}

	// 4. POST missing wallet_id
	reqNoID := httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader(`{"status":"ACTIVE"}`))
	wNoID := httptest.NewRecorder()
	srv.handleAdminWalletStatus(wNoID, reqNoID)
	if wNoID.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing wallet_id, got %d", wNoID.Code)
	}

	// 5. POST invalid status
	reqBadStatus := httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader(`{"wallet_id":"w1","status":"UNKNOWN"}`))
	wBadStatus := httptest.NewRecorder()
	srv.handleAdminWalletStatus(wBadStatus, reqBadStatus)
	if wBadStatus.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid status, got %d", wBadStatus.Code)
	}
}

func TestInitWalletOutboxAndMetricsServer(t *testing.T) {
	ctx := context.Background()

	// 1. initWalletOutbox disabled
	t.Setenv("LOGGING_OUTBOX_ENABLED", "false")
	ob1, pub1 := initWalletOutbox(ctx, nil)
	if ob1 != nil || pub1 != nil {
		t.Fatal("expected nil outbox when disabled")
	}

	// 2. initWalletOutbox enabled but missing URL
	t.Setenv("LOGGING_OUTBOX_ENABLED", "true")
	t.Setenv("LOGGING_RABBITMQ_URL", "")
	ob2, pub2 := initWalletOutbox(ctx, nil)
	if ob2 != nil || pub2 != nil {
		t.Fatal("expected nil outbox when URL empty")
	}

	// 3. startWalletMetricsServer
	srv := &server{region: "us-east-1", isActive: true}
	metricsSrv := startWalletMetricsServer(srv, "us-east-1", true)
	defer metricsSrv.Close()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	metricsSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /healthz, got %d", rec.Code)
	}
}

type fakeLedgerSyncClient struct {
	ledgerv1.LedgerServiceClient
	recordErr  error
	recordResp *ledgerv1.RecordTransactionResponse
}

func (f *fakeLedgerSyncClient) RecordTransaction(_ context.Context, _ *ledgerv1.RecordTransactionRequest, _ ...grpc.CallOption) (*ledgerv1.RecordTransactionResponse, error) {
	return f.recordResp, f.recordErr
}

func TestDispatchImmediateLedger(t *testing.T) {
	ctx := context.Background()
	req := &walletv1.TransferFundsRequest{
		IdempotencyKey:      "idemp-dispatch-1",
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              &walletv1.Money{Currency: "USD", Units: 100},
	}
	task := LedgerTask{
		TransactionID:       "tx-123",
		IdempotencyKey:      "idemp-dispatch-1",
		SourceWalletID:      "alice",
		DestinationWalletID: "bob",
		Amount:              100,
		Currency:            "USD",
		Region:              "us-east-1",
	}

	// 1. ledgerRelay is nil -> no-op
	srvNil := &server{logger: observability.NewMemoryLogger()}
	srvNil.dispatchImmediateLedger(ctx, req, task, "tx-123")

	// 2. ledgerRelay with failed RPC
	fakeFail := &fakeLedgerSyncClient{
		recordErr: errors.New("rpc failed"),
	}
	relayFail := &LedgerRelay{
		ledgerClient: fakeFail,
		logger:       observability.NewMemoryLogger(),
	}
	srvFail := &server{
		ledgerRelay: relayFail,
		logger:      observability.NewMemoryLogger(),
	}
	srvFail.dispatchImmediateLedger(ctx, req, task, "tx-123")

	// 3. ledgerRelay with rejected response
	fakeReject := &fakeLedgerSyncClient{
		recordResp: &ledgerv1.RecordTransactionResponse{Success: false, ErrorMessage: "rejected"},
	}
	relayReject := &LedgerRelay{
		ledgerClient: fakeReject,
		logger:       observability.NewMemoryLogger(),
	}
	srvReject := &server{
		ledgerRelay: relayReject,
		logger:      observability.NewMemoryLogger(),
	}
	srvReject.dispatchImmediateLedger(ctx, req, task, "tx-123")
}

func TestHandleTransferFailureAndSuccess(t *testing.T) {
	srv := &server{
		region: "us-east-1",
		logger: observability.NewMemoryLogger(),
	}
	ctx := context.Background()

	req := &walletv1.TransferFundsRequest{
		IdempotencyKey:      "idemp-fail-1",
		SourceWalletId:      "w1",
		DestinationWalletId: "w2",
		Amount:              &walletv1.Money{Currency: "USD", Units: 100},
	}

	// Failure with sourceDebited = true
	stateFail1 := &transferExecutionState{
		finalTxnID:    "tx-fail-1",
		sourceDebited: true,
		txnStatus:     walletv1.TransferFundsResponse_SUCCESS,
	}
	respFail1 := srv.handleTransferFailure(ctx, req, stateFail1, errors.New("aborted"), 50)
	if respFail1.Status != walletv1.TransferFundsResponse_INTERNAL_ERROR {
		t.Fatalf("expected INTERNAL_ERROR status on failure, got %v", respFail1.Status)
	}

	// Failure with sourceDebited = false
	stateFail2 := &transferExecutionState{
		finalTxnID:    "tx-fail-2",
		sourceDebited: false,
		txnStatus:     walletv1.TransferFundsResponse_FAILED_INSUFFICIENT_FUNDS,
		txnErrMsg:     "Insufficient funds",
	}
	respFail2 := srv.handleTransferFailure(ctx, req, stateFail2, errors.New("insufficient funds"), 20)
	if respFail2.Status != walletv1.TransferFundsResponse_FAILED_INSUFFICIENT_FUNDS {
		t.Fatalf("expected FAILED_INSUFFICIENT_FUNDS status, got %v", respFail2.Status)
	}

	// Success handling
	stateSuccess := &transferExecutionState{
		finalTxnID: "tx-success-1",
		txnStatus:  walletv1.TransferFundsResponse_SUCCESS,
	}
	srv.handleTransferSuccess(ctx, req, stateSuccess, 30)
}

func TestAdminWalletStatusHandlersDetailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 1)
	if err != nil {
		t.Skipf("MongoDB unavailable: %v", err)
		return
	}
	defer func() { _ = client.Disconnect(ctx) }()

	walletDB := client.Database("test_admin_status_pkg")
	_ = walletDB.Drop(ctx)

	srv := &server{
		mongoClient: client,
		region:      "us-east-1",
		logger:      observability.NewMemoryLogger(),
	}

	// 1. handleAdminWalletStatusGet missing wallet_id -> 400
	reqGetNoID := httptest.NewRequest(http.MethodGet, "/admin/wallet/status", nil)
	recGetNoID := httptest.NewRecorder()
	srv.handleAdminWalletStatusGet(recGetNoID, reqGetNoID)
	if recGetNoID.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", recGetNoID.Code)
	}

	// 2. handleAdminWalletStatusGet not found -> 404
	reqGetNotFound := httptest.NewRequest(http.MethodGet, "/admin/wallet/status?wallet_id=nonexistent", nil)
	recGetNotFound := httptest.NewRecorder()
	srv.handleAdminWalletStatusGet(recGetNotFound, reqGetNotFound)
	if recGetNotFound.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d", recGetNotFound.Code)
	}

	// 3. handleAdminWalletStatusUpdate bad JSON -> 400
	reqUpdateBadJSON := httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader("not json"))
	recUpdateBadJSON := httptest.NewRecorder()
	srv.handleAdminWalletStatusUpdate(recUpdateBadJSON, reqUpdateBadJSON)
	if recUpdateBadJSON.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for bad JSON, got %d", recUpdateBadJSON.Code)
	}

	// 4. handleAdminWalletStatusUpdate missing wallet_id -> 400
	reqUpdateNoID := httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader(`{"status":"FROZEN"}`))
	recUpdateNoID := httptest.NewRecorder()
	srv.handleAdminWalletStatusUpdate(recUpdateNoID, reqUpdateNoID)
	if recUpdateNoID.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for missing wallet_id, got %d", recUpdateNoID.Code)
	}

	// 5. handleAdminWalletStatusUpdate invalid status -> 400
	reqUpdateBadStatus := httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader(`{"wallet_id":"w1","status":"INVALID_STATUS"}`))
	recUpdateBadStatus := httptest.NewRecorder()
	srv.handleAdminWalletStatusUpdate(recUpdateBadStatus, reqUpdateBadStatus)
	if recUpdateBadStatus.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid status, got %d", recUpdateBadStatus.Code)
	}

	// 6. handleAdminWalletStatusUpdate not found -> 404
	reqUpdateNotFound := httptest.NewRequest(http.MethodPost, "/admin/wallet/status", strings.NewReader(`{"wallet_id":"nonexistent","status":"FROZEN"}`))
	recUpdateNotFound := httptest.NewRecorder()
	srv.handleAdminWalletStatusUpdate(recUpdateNotFound, reqUpdateNotFound)
	if recUpdateNotFound.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d", recUpdateNotFound.Code)
	}
}
