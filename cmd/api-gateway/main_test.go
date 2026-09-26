package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"wallet-system/pkg/auth"
	"wallet-system/pkg/coordinator"
	authv1 "wallet-system/proto/auth"
	ledgerv1 "wallet-system/proto/ledger"
	walletv1 "wallet-system/proto/wallet"
)

type fakeWalletClient struct {
	getBalanceRequest     *walletv1.GetBalanceRequest
	getBalanceResponse    *walletv1.GetBalanceResponse
	getBalanceError       error
	createWalletRequest   *walletv1.CreateWalletRequest
	transferFundsRequest  *walletv1.TransferFundsRequest
	transferContext       context.Context
	createWalletResponse  *walletv1.CreateWalletResponse
	transferFundsResponse *walletv1.TransferFundsResponse
	createWalletError     error
	transferFundsError    error
}

func (f *fakeWalletClient) CreateWallet(_ context.Context, req *walletv1.CreateWalletRequest, _ ...grpc.CallOption) (*walletv1.CreateWalletResponse, error) {
	f.createWalletRequest = req
	return f.createWalletResponse, f.createWalletError
}

func (f *fakeWalletClient) GetBalance(_ context.Context, req *walletv1.GetBalanceRequest, _ ...grpc.CallOption) (*walletv1.GetBalanceResponse, error) {
	f.getBalanceRequest = req
	if f.getBalanceError != nil {
		return nil, f.getBalanceError
	}
	if f.getBalanceResponse != nil {
		return f.getBalanceResponse, nil
	}
	return nil, errors.New("GetBalance not implemented in fake")
}

func (f *fakeWalletClient) TransferFunds(ctx context.Context, req *walletv1.TransferFundsRequest, _ ...grpc.CallOption) (*walletv1.TransferFundsResponse, error) {
	f.transferFundsRequest = req
	f.transferContext = ctx
	return f.transferFundsResponse, f.transferFundsError
}

func (f *fakeWalletClient) HealthCheck(context.Context, *walletv1.HealthRequest, ...grpc.CallOption) (*walletv1.HealthResponse, error) {
	return nil, errors.New("HealthCheck not implemented in fake")
}

type fakeLedgerClient struct {
	getLedgerRequest  *ledgerv1.GetLedgerRequest
	getLedgerResponse *ledgerv1.GetLedgerResponse
	getLedgerError    error
}

func (f *fakeLedgerClient) RecordTransaction(context.Context, *ledgerv1.RecordTransactionRequest, ...grpc.CallOption) (*ledgerv1.RecordTransactionResponse, error) {
	return nil, errors.New("RecordTransaction not implemented in fake")
}

func (f *fakeLedgerClient) GetLedgerEntries(_ context.Context, req *ledgerv1.GetLedgerRequest, _ ...grpc.CallOption) (*ledgerv1.GetLedgerResponse, error) {
	f.getLedgerRequest = req
	return f.getLedgerResponse, f.getLedgerError
}

func TestHandleTransferRejectsInvalidCurrency(t *testing.T) {
	gateway := &Gateway{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", strings.NewReader(`{"idempotency_key":"k1","source_wallet_id":"alice","destination_wallet_id":"bob","amount":10,"currency":"FAKE"}`))
	response := httptest.NewRecorder()

	gateway.handleTransfer(response, req)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for fake currency, got %d", response.Code)
	}
}

func TestHandleCreateWalletRejectsInvalidCurrency(t *testing.T) {
	gateway := &Gateway{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/wallets", strings.NewReader(`{"wallet_id":"alice","currency":"FAKE","initial_balance":100}`))
	response := httptest.NewRecorder()

	gateway.handleCreateWallet(response, req)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for fake currency, got %d", response.Code)
	}
}

func TestHandleLedgerPaginatedResponse(t *testing.T) {
	ledgerClient := &fakeLedgerClient{
		getLedgerResponse: &ledgerv1.GetLedgerResponse{
			Entries: []*ledgerv1.LedgerEntry{
				{TransactionId: "tx-1", IdempotencyKey: "k1", Amount: 50, Currency: "USD"},
			},
			NextPageToken: "next-token-abc",
			TotalCount:    10,
		},
	}
	gateway := &Gateway{ledgerClient: ledgerClient}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ledger?wallet_id=alice&limit=5&page_token=page1", nil)
	response := httptest.NewRecorder()

	gateway.handleLedger(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", response.Code)
	}
	if ledgerClient.getLedgerRequest.WalletId != "alice" || ledgerClient.getLedgerRequest.Limit != 5 || ledgerClient.getLedgerRequest.PageToken != "page1" {
		t.Fatalf("unexpected ledger request parameters: %+v", ledgerClient.getLedgerRequest)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}
	if body["wallet_id"] != "alice" || body["next_page_token"] != "next-token-abc" || body["total_count"] != float64(10) {
		t.Fatalf("unexpected response JSON: %+v", body)
	}
}

func TestHandleCreateWalletConvertsJSONToProto(t *testing.T) {
	client := &fakeWalletClient{
		createWalletResponse: &walletv1.CreateWalletResponse{
			Success:         true,
			Message:         "created",
			HandledByRegion: "test-region",
		},
	}
	gateway := &Gateway{activeTarget: "PRIMARY", primaryClient: client}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/wallets", strings.NewReader(`{"wallet_id":"alice","currency":"USD","initial_balance":1000}`))
	response := httptest.NewRecorder()

	gateway.handleCreateWallet(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", response.Code, response.Body.String())
	}
	if client.createWalletRequest == nil {
		t.Fatal("expected wallet protobuf request")
	}
	if client.createWalletRequest.WalletId != "alice" || client.createWalletRequest.Currency != "USD" || client.createWalletRequest.InitialBalance != 1000 {
		t.Fatalf("unexpected protobuf request: %+v", client.createWalletRequest)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response JSON: %v", err)
	}
	if body["success"] != true || body["routed_gateway"] != "PRIMARY" || body["handled_region"] != "test-region" {
		t.Fatalf("unexpected response JSON: %v", body)
	}
}

func TestHandleCreateWalletReturns409WhenWalletAlreadyExists(t *testing.T) {
	client := &fakeWalletClient{
		createWalletError: status.Errorf(codes.AlreadyExists, "wallet alice already exists"),
	}
	gateway := &Gateway{activeTarget: "PRIMARY", primaryClient: client}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/wallets", strings.NewReader(`{"wallet_id":"alice","currency":"USD","initial_balance":1000}`))
	response := httptest.NewRecorder()

	gateway.handleCreateWallet(response, req)

	if response.Code != http.StatusConflict {
		t.Fatalf("expected status 409 Conflict, got %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "wallet alice already exists") {
		t.Fatalf("unexpected error message: %s", response.Body.String())
	}
}

func TestHandleTransferConvertsJSONToProtoAndResponseToJSON(t *testing.T) {
	client := &fakeWalletClient{
		transferFundsResponse: &walletv1.TransferFundsResponse{
			TransactionId:   "tx-123",
			Status:          walletv1.TransferFundsResponse_SUCCESS,
			HandledByRegion: "test-region",
		},
	}
	gateway := &Gateway{activeTarget: "STANDBY", standbyClient: client}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", strings.NewReader(`{"idempotency_key":"key-1","source_wallet_id":"alice","destination_wallet_id":"bob","amount":25,"currency":"USD"}`))
	response := httptest.NewRecorder()

	gateway.handleTransfer(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", response.Code, response.Body.String())
	}
	protoRequest := client.transferFundsRequest
	if protoRequest == nil || protoRequest.Amount == nil {
		t.Fatal("expected transfer protobuf request with amount")
	}
	if protoRequest.IdempotencyKey != "key-1" || protoRequest.SourceWalletId != "alice" || protoRequest.DestinationWalletId != "bob" || protoRequest.Amount.Currency != "USD" || protoRequest.Amount.Units != 25 {
		t.Fatalf("unexpected protobuf request: %+v", protoRequest)
	}
	metadataValues, ok := metadata.FromOutgoingContext(client.transferContext)
	associationValues := metadataValues.Get("x-association-id")
	idempotencyValues := metadataValues.Get("x-idempotency-key")
	if !ok || len(associationValues) != 1 || associationValues[0] == "" || len(idempotencyValues) != 1 || idempotencyValues[0] != "key-1" {
		t.Fatalf("expected correlation metadata, got %v", metadataValues)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response JSON: %v", err)
	}
	if body["transaction_id"] != "tx-123" || body["status"] != "SUCCESS" || body["routed_gateway"] != "STANDBY" {
		t.Fatalf("unexpected response JSON: %v", body)
	}
}

func TestHandleTransferRejectsInvalidJSON(t *testing.T) {
	gateway := &Gateway{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", strings.NewReader("not-json"))
	response := httptest.NewRecorder()

	gateway.handleTransfer(response, req)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", response.Code)
	}
}

func TestHandleLedgerRejectsMissingWalletID(t *testing.T) {
	gateway := &Gateway{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ledger", nil)
	response := httptest.NewRecorder()

	gateway.handleLedger(response, req)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for missing wallet_id, got %d", response.Code)
	}
}

// fakeMainAuthClient is a test stub for authv1.AuthServiceClient used in main_test.go.
type fakeMainAuthClient struct {
	issueResponse *authv1.IssueTokenResponse
	issueErr      error
}

func (f *fakeMainAuthClient) IssueToken(_ context.Context, _ *authv1.IssueTokenRequest, _ ...grpc.CallOption) (*authv1.IssueTokenResponse, error) {
	return f.issueResponse, f.issueErr
}
func (f *fakeMainAuthClient) ValidateToken(_ context.Context, _ *authv1.ValidateTokenRequest, _ ...grpc.CallOption) (*authv1.ValidateTokenResponse, error) {
	return &authv1.ValidateTokenResponse{Valid: true}, nil
}
func (f *fakeMainAuthClient) RefreshToken(_ context.Context, _ *authv1.RefreshTokenRequest, _ ...grpc.CallOption) (*authv1.IssueTokenResponse, error) {
	return nil, nil
}
func (f *fakeMainAuthClient) RevokeToken(_ context.Context, _ *authv1.RevokeTokenRequest, _ ...grpc.CallOption) (*authv1.RevokeTokenResponse, error) {
	return &authv1.RevokeTokenResponse{Success: true}, nil
}
func (f *fakeMainAuthClient) Authorize(_ context.Context, _ *authv1.AuthorizeRequest, _ ...grpc.CallOption) (*authv1.AuthorizeResponse, error) {
	return &authv1.AuthorizeResponse{Allowed: true}, nil
}
func (f *fakeMainAuthClient) HealthCheck(_ context.Context, _ *authv1.AuthHealthRequest, _ ...grpc.CallOption) (*authv1.AuthHealthResponse, error) {
	return &authv1.AuthHealthResponse{Status: "SERVING"}, nil
}

func TestHandleLoginSuccess(t *testing.T) {
	fakeAC := &fakeMainAuthClient{
		issueResponse: &authv1.IssueTokenResponse{
			AccessToken:   "test-access-token",
			RefreshToken:  "test-refresh-token",
			TokenType:     "Bearer",
			ExpiresIn:     900,
			GrantedScopes: []string{auth.ScopeWalletRead, auth.ScopeWalletTransfer},
			Subject:       "alice",
		},
	}
	gateway := &Gateway{authClient: fakeAC}

	body := strings.NewReader(`{"username":"alice","password":"secret"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	gateway.handleLogin(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode json: %v", err)
	}
	if resp["access_token"] != "test-access-token" {
		t.Fatalf("unexpected access_token: %v", resp["access_token"])
	}
	if resp["subject"] != "alice" {
		t.Fatalf("unexpected subject: %v", resp["subject"])
	}
}

func TestHandleTransferRejectsIDORViolation(t *testing.T) {
	gateway := &Gateway{}
	// Alice attempts to spend Bob's money
	reqBody := `{"idempotency_key":"k1","source_wallet_id":"bob","destination_wallet_id":"charlie","amount":50,"currency":"USD"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", strings.NewReader(reqBody))

	// Alice's claims in context
	aliceClaims := &auth.Claims{Subject: "alice", Roles: []string{auth.RoleUser}}
	ctx := ContextWithClaims(req.Context(), aliceClaims)
	req = req.WithContext(ctx)

	response := httptest.NewRecorder()
	gateway.handleTransfer(response, req)

	if response.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for IDOR violation, got %d", response.Code)
	}
}

func TestHandleTransferFlexibleFieldNames(t *testing.T) {
	client := &fakeWalletClient{
		transferFundsResponse: &walletv1.TransferFundsResponse{
			TransactionId:   "tx-123",
			Status:          walletv1.TransferFundsResponse_SUCCESS,
			HandledByRegion: "test-region",
		},
	}
	gateway := &Gateway{activeTarget: "PRIMARY", primaryClient: client}

	// Payload uses source_wallet and dest_wallet shorthand
	reqBody := `{"idempotency_key":"k-flex-1","source_wallet":"alice","dest_wallet":"bob","amount":25,"currency":"USD"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", strings.NewReader(reqBody))

	aliceClaims := &auth.Claims{Subject: "alice", Roles: []string{auth.RoleUser}}
	ctx := ContextWithClaims(req.Context(), aliceClaims)
	req = req.WithContext(ctx)

	response := httptest.NewRecorder()
	gateway.handleTransfer(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", response.Code, response.Body.String())
	}
	if client.transferFundsRequest == nil {
		t.Fatal("expected gRPC transferFundsRequest to be populated")
	}
	if client.transferFundsRequest.SourceWalletId != "alice" {
		t.Fatalf("expected source_wallet_id 'alice', got %q", client.transferFundsRequest.SourceWalletId)
	}
	if client.transferFundsRequest.DestinationWalletId != "bob" {
		t.Fatalf("expected destination_wallet_id 'bob', got %q", client.transferFundsRequest.DestinationWalletId)
	}
}

func TestHandleFailoverRestrictedToAdmin(t *testing.T) {
	gateway := &Gateway{activeTarget: "PRIMARY"}

	// Normal user fails
	userClaims := &auth.Claims{Subject: "alice", Roles: []string{auth.RoleUser}}
	reqUser := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/failover", nil)
	reqUser = reqUser.WithContext(ContextWithClaims(reqUser.Context(), userClaims))
	recUser := httptest.NewRecorder()

	gateway.handleFailover(recUser, reqUser)
	if recUser.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin user on failover, got %d", recUser.Code)
	}

	// Admin succeeds
	adminClaims := &auth.Claims{Subject: "ops", Roles: []string{auth.RoleAdmin}}
	reqAdmin := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/failover", nil)
	reqAdmin = reqAdmin.WithContext(ContextWithClaims(reqAdmin.Context(), adminClaims))
	recAdmin := httptest.NewRecorder()

	gateway.handleFailover(recAdmin, reqAdmin)
	if recAdmin.Code != http.StatusOK {
		t.Fatalf("expected 200 for admin on failover, got %d", recAdmin.Code)
	}
}

func TestResolveServiceAddress(t *testing.T) {
	t.Setenv("TEST_SERVICE_ADDR", "custom-host:9999")
	addr := resolveServiceAddress("TEST_SERVICE_ADDR", "nonexistent-host", "1234")
	if addr != "custom-host:9999" {
		t.Fatalf("expected custom-host:9999, got %s", addr)
	}

	addrDefault := resolveServiceAddress("NONEXISTENT_ENV_VAR", "invalid-domain-xyz-12345", "5000")
	if addrDefault != "127.0.0.1:5000" {
		t.Fatalf("expected 127.0.0.1:5000, got %s", addrDefault)
	}
}

func TestResolveGatewayServiceAddresses(t *testing.T) {
	t.Setenv("PRIMARY_WALLET_ADDR", "p:1")
	t.Setenv("STANDBY_WALLET_ADDR", "s:2")
	t.Setenv("LEDGER_ADDR", "l:3")
	t.Setenv("AUTH_SERVICE_ADDR", "a:4")

	p, s, l, a := resolveGatewayServiceAddresses()
	if p != "p:1" || s != "s:2" || l != "l:3" || a != "a:4" {
		t.Fatalf("unexpected addresses: %s, %s, %s, %s", p, s, l, a)
	}
}

func TestHandleWalletsMethodNotAllowed(t *testing.T) {
	gw := &Gateway{}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/wallets", nil)
	rec := httptest.NewRecorder()
	gw.handleWallets(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", rec.Code)
	}
}

func TestHandleReadyz(t *testing.T) {
	fakeClient := &fakeWalletClient{}
	gw := &Gateway{
		activeTarget:  "PRIMARY",
		primaryClient: fakeClient,
	}

	// 1. Error path
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	gw.handleReadyz(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when client fails, got %d", rec.Code)
	}
}

func TestInitFailoverCoordinatorFallback(t *testing.T) {
	coord, cleanup := initFailoverCoordinator("")
	defer cleanup()
	if coord == nil {
		t.Fatal("expected memory coordinator")
	}
	target, err := coord.GetActiveTarget(context.Background())
	if err != nil || target == "" {
		t.Fatalf("expected valid target from memory coordinator, got %s, err %v", target, err)
	}
}

func TestBuildGatewayRouter(t *testing.T) {
	gw := &Gateway{
		activeTarget: "PRIMARY",
	}
	handler := buildGatewayRouter(gw)
	if handler == nil {
		t.Fatal("expected non-nil gateway router handler")
	}

	// Test public endpoint through router
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /healthz, got %d", rec.Code)
	}
}

func TestHandleWalletGRPCError(t *testing.T) {
	// 1. AlreadyExists -> 409 Conflict
	recConflict := httptest.NewRecorder()
	handleWalletGRPCError(recConflict, status.Error(codes.AlreadyExists, "already exists"), "test")
	if recConflict.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", recConflict.Code)
	}

	// 2. InvalidArgument -> 400 Bad Request
	recBad := httptest.NewRecorder()
	handleWalletGRPCError(recBad, status.Error(codes.InvalidArgument, "invalid input"), "test")
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recBad.Code)
	}

	// 3. Generic internal error -> 500
	recInternal := httptest.NewRecorder()
	handleWalletGRPCError(recInternal, errors.New("something crashed"), "test")
	if recInternal.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recInternal.Code)
	}
}

func TestHandleGetBalanceEndpoint(t *testing.T) {
	client := &fakeWalletClient{
		getBalanceResponse: &walletv1.GetBalanceResponse{
			WalletId:        "bob",
			Balances:        []*walletv1.Money{{Currency: "USD", Units: 250}},
			Status:          "ACTIVE",
			HandledByRegion: "us-east-1",
		},
	}
	gw := &Gateway{
		activeTarget:  "PRIMARY",
		primaryClient: client,
	}

	// 1. Method Not Allowed
	reqPost := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/balance?id=bob", nil)
	recPost := httptest.NewRecorder()
	gw.handleGetBalance(recPost, reqPost)
	if recPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", recPost.Code)
	}

	// 2. Missing 'id' parameter
	reqNoID := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/balance", nil)
	recNoID := httptest.NewRecorder()
	gw.handleGetBalance(recNoID, reqNoID)
	if recNoID.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for missing id, got %d", recNoID.Code)
	}

	// 3. Successful balance retrieval
	reqOK := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/balance?id=bob", nil)
	recOK := httptest.NewRecorder()
	gw.handleGetBalance(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recOK.Code)
	}
	if client.getBalanceRequest.WalletId != "bob" {
		t.Fatalf("expected walletId bob, got %s", client.getBalanceRequest.WalletId)
	}

	// 4. gRPC error
	client.getBalanceError = errors.New("db timeout")
	client.getBalanceResponse = nil
	reqErr := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/balance?id=bob", nil)
	recErr := httptest.NewRecorder()
	gw.handleGetBalance(recErr, reqErr)
	if recErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on gRPC error, got %d", recErr.Code)
	}
}

func TestParseCreateWalletRequestErrors(t *testing.T) {
	// 1. Invalid JSON body
	reqBadJSON := httptest.NewRequest(http.MethodPost, "/api/v1/wallets", strings.NewReader("bad-json"))
	_, err := parseCreateWalletRequest(reqBadJSON, "trace-1")
	if err == nil {
		t.Fatal("expected error for bad json")
	}

	// 2. Invalid currency
	reqBadCurr := httptest.NewRequest(http.MethodPost, "/api/v1/wallets", strings.NewReader(`{"wallet_id":"alice","currency":"NOTREAL","initial_balance":100}`))
	_, err = parseCreateWalletRequest(reqBadCurr, "trace-2")
	if err == nil || !strings.Contains(err.Error(), "Invalid or unsupported currency") {
		t.Fatalf("expected unsupported currency error, got %v", err)
	}

	// 3. Negative balance
	reqNeg := httptest.NewRequest(http.MethodPost, "/api/v1/wallets", strings.NewReader(`{"wallet_id":"alice","currency":"USD","initial_balance":-10}`))
	_, err = parseCreateWalletRequest(reqNeg, "trace-3")
	if err == nil || !strings.Contains(err.Error(), "cannot be negative") {
		t.Fatalf("expected negative balance error, got %v", err)
	}
}

func TestHandleTransferValidationErrors(t *testing.T) {
	gw := &Gateway{}

	// 1. Method Not Allowed
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/transfers", nil)
	recGet := httptest.NewRecorder()
	gw.handleTransfer(recGet, reqGet)
	if recGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recGet.Code)
	}

	// 2. Missing source or destination
	reqMissing := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", strings.NewReader(`{"source_wallet_id":"alice","destination_wallet_id":"","amount":10,"currency":"USD"}`))
	recMissing := httptest.NewRecorder()
	gw.handleTransfer(recMissing, reqMissing)
	if recMissing.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing dest wallet, got %d", recMissing.Code)
	}
}

func TestHandleClusterStatus(t *testing.T) {
	primaryClient := &fakeWalletClient{}
	standbyClient := &fakeWalletClient{}
	gw := &Gateway{
		activeTarget:   "PRIMARY",
		primaryClient:  primaryClient,
		standbyClient:  standbyClient,
		primaryAddress: "localhost:50051",
		standbyAddress: "localhost:50053",
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/status", nil)
	rec := httptest.NewRecorder()
	gw.handleClusterStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
}

func TestHandleLedgerEdgeCases(t *testing.T) {
	fakeClient := &fakeLedgerClient{}
	gw := &Gateway{ledgerClient: fakeClient}

	// 1. Method Not Allowed
	reqPost := httptest.NewRequest(http.MethodPost, "/api/v1/ledger", nil)
	recPost := httptest.NewRecorder()
	gw.handleLedger(recPost, reqPost)
	if recPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", recPost.Code)
	}

	// 2. IDOR rejection
	claimsCtx := ContextWithClaims(context.Background(), &auth.Claims{Subject: "alice"})
	reqIDOR := httptest.NewRequest(http.MethodGet, "/api/v1/ledger?wallet_id=bob", nil).WithContext(claimsCtx)
	recIDOR := httptest.NewRecorder()
	gw.handleLedger(recIDOR, reqIDOR)
	if recIDOR.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for IDOR, got %d", recIDOR.Code)
	}

	// 3. Ledger client failure
	fakeClient.getLedgerError = errors.New("ledger db down")
	reqErr := httptest.NewRequest(http.MethodGet, "/api/v1/ledger?wallet_id=alice", nil)
	recErr := httptest.NewRecorder()
	gw.handleLedger(recErr, reqErr)
	if recErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on ledger client error, got %d", recErr.Code)
	}
}

func TestHandleCreateWalletAndBalanceIDOR(t *testing.T) {
	gw := &Gateway{}
	claimsCtx := ContextWithClaims(context.Background(), &auth.Claims{Subject: "alice"})

	// 1. Create wallet Method Not Allowed
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/wallets", nil)
	recGet := httptest.NewRecorder()
	gw.handleCreateWallet(recGet, reqGet)
	if recGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", recGet.Code)
	}

	// 2. Create wallet IDOR
	reqIDOR := httptest.NewRequest(http.MethodPost, "/api/v1/wallets", strings.NewReader(`{"wallet_id":"bob","currency":"USD","initial_balance":100}`)).WithContext(claimsCtx)
	recIDOR := httptest.NewRecorder()
	gw.handleCreateWallet(recIDOR, reqIDOR)
	if recIDOR.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden on IDOR create wallet, got %d", recIDOR.Code)
	}

	// 3. Get balance IDOR
	reqBalIDOR := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/balance?id=bob", nil).WithContext(claimsCtx)
	recBalIDOR := httptest.NewRecorder()
	gw.handleGetBalance(recBalIDOR, reqBalIDOR)
	if recBalIDOR.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden on IDOR get balance, got %d", recBalIDOR.Code)
	}

	// 4. Transfer gRPC failure
	failClient := &fakeWalletClient{
		transferFundsError: errors.New("network disconnect"),
	}
	gwFail := &Gateway{
		activeTarget:  "PRIMARY",
		primaryClient: failClient,
	}
	reqTxFail := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", strings.NewReader(`{"source_wallet_id":"alice","destination_wallet_id":"bob","amount":10,"currency":"USD"}`))
	recTxFail := httptest.NewRecorder()
	gwFail.handleTransfer(recTxFail, reqTxFail)
	if recTxFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on transfer gRPC failure, got %d", recTxFail.Code)
	}
}

type errFailoverCoord struct {
	*coordinator.MemoryFailoverCoordinator
	failSet bool
}

func (e *errFailoverCoord) SetActiveTarget(ctx context.Context, target string) error {
	if e.failSet {
		return errors.New("coord error")
	}
	return e.MemoryFailoverCoordinator.SetActiveTarget(ctx, target)
}

func TestHandleFailoverAndCoordinatorRoutes(t *testing.T) {
	memCoord := coordinator.NewMemoryFailoverCoordinator(coordinator.TargetPrimary)
	primaryClient := &fakeWalletClient{}
	standbyClient := &fakeWalletClient{}

	gw := &Gateway{
		coordinator:    memCoord,
		primaryClient:  primaryClient,
		standbyClient:  standbyClient,
		primaryAddress: "localhost:50051",
		standbyAddress: "localhost:50053",
	}

	// 1. Method Not Allowed (GET)
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/failover", nil)
	recGet := httptest.NewRecorder()
	gw.handleFailover(recGet, reqGet)
	if recGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", recGet.Code)
	}

	// 2. Forbidden without admin role or cluster:admin scope
	userClaimsCtx := ContextWithClaims(context.Background(), &auth.Claims{Subject: "alice", Roles: []string{auth.RoleUser}})
	reqForbidden := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/failover", nil).WithContext(userClaimsCtx)
	recForbidden := httptest.NewRecorder()
	gw.handleFailover(recForbidden, reqForbidden)
	if recForbidden.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d", recForbidden.Code)
	}

	// 3. Allowed with admin role -> toggles PRIMARY to STANDBY
	adminClaimsCtx := ContextWithClaims(context.Background(), &auth.Claims{Subject: "admin1", Roles: []string{auth.RoleAdmin}})
	reqAdmin := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/failover", nil).WithContext(adminClaimsCtx)
	recAdmin := httptest.NewRecorder()
	gw.handleFailover(recAdmin, reqAdmin)
	if recAdmin.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recAdmin.Code)
	}
	if !strings.Contains(recAdmin.Body.String(), `"active_routed_target":"STANDBY"`) {
		t.Fatalf("expected routed target STANDBY, got %s", recAdmin.Body.String())
	}

	// 4. handleClusterStatus with coordinator returning target
	recStatus := httptest.NewRecorder()
	reqStatus := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/status", nil)
	gw.handleClusterStatus(recStatus, reqStatus)
	if recStatus.Code != http.StatusOK || !strings.Contains(recStatus.Body.String(), `"current_routed_target":"STANDBY"`) {
		t.Fatalf("expected current_routed_target STANDBY in cluster status, got %s", recStatus.Body.String())
	}

	// 5. Allowed with cluster:admin scope -> toggles STANDBY back to PRIMARY
	scopeClaimsCtx := ContextWithClaims(context.Background(), &auth.Claims{Subject: "operator", Scopes: []string{auth.ScopeClusterAdmin}})
	reqScope := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/failover", nil).WithContext(scopeClaimsCtx)
	recScope := httptest.NewRecorder()
	gw.handleFailover(recScope, reqScope)
	if recScope.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recScope.Code)
	}
	if !strings.Contains(recScope.Body.String(), `"active_routed_target":"PRIMARY"`) {
		t.Fatalf("expected routed target PRIMARY, got %s", recScope.Body.String())
	}

	// 6. Coordinator SetActiveTarget failure -> 500 InternalServerError
	errCoord := &errFailoverCoord{
		MemoryFailoverCoordinator: coordinator.NewMemoryFailoverCoordinator(coordinator.TargetPrimary),
		failSet:                   true,
	}
	gwErr := &Gateway{coordinator: errCoord}
	reqErr := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/failover", nil)
	recErr := httptest.NewRecorder()
	gwErr.handleFailover(recErr, reqErr)
	if recErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on coordinator error, got %d", recErr.Code)
	}

	// 7. resolveCurrentTarget fallback to default TargetPrimary when coordinator is nil and activeTarget is empty
	gwDefault := &Gateway{}
	if target := gwDefault.resolveCurrentTarget(context.Background()); target != coordinator.TargetPrimary {
		t.Fatalf("expected fallback to TargetPrimary, got %s", target)
	}
}

type fakeAuthServiceClient struct {
	authv1.AuthServiceClient
	issueTokenResp   *authv1.IssueTokenResponse
	issueTokenErr    error
	refreshTokenResp *authv1.IssueTokenResponse
	refreshTokenErr  error
	revokeTokenResp  *authv1.RevokeTokenResponse
	revokeTokenErr   error
}

func (f *fakeAuthServiceClient) IssueToken(_ context.Context, _ *authv1.IssueTokenRequest, _ ...grpc.CallOption) (*authv1.IssueTokenResponse, error) {
	return f.issueTokenResp, f.issueTokenErr
}

func (f *fakeAuthServiceClient) RefreshToken(_ context.Context, _ *authv1.RefreshTokenRequest, _ ...grpc.CallOption) (*authv1.IssueTokenResponse, error) {
	return f.refreshTokenResp, f.refreshTokenErr
}

func (f *fakeAuthServiceClient) RevokeToken(_ context.Context, _ *authv1.RevokeTokenRequest, _ ...grpc.CallOption) (*authv1.RevokeTokenResponse, error) {
	return f.revokeTokenResp, f.revokeTokenErr
}

func TestGatewayAuthEndpoints(t *testing.T) {
	authClient := &fakeAuthServiceClient{
		issueTokenResp: &authv1.IssueTokenResponse{
			AccessToken:   "access-123",
			RefreshToken:  "refresh-123",
			TokenType:     "Bearer",
			ExpiresIn:     3600,
			GrantedScopes: []string{"wallet:read"},
			Subject:       "alice",
		},
		refreshTokenResp: &authv1.IssueTokenResponse{
			AccessToken:   "access-456",
			RefreshToken:  "refresh-456",
			TokenType:     "Bearer",
			ExpiresIn:     3600,
			GrantedScopes: []string{"wallet:read"},
			Subject:       "alice",
		},
		revokeTokenResp: &authv1.RevokeTokenResponse{
			Success: true,
			Message: "revoked",
		},
	}

	gw := &Gateway{authClient: authClient}

	// 1. handleLogin - Method Not Allowed
	reqLogGet := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil)
	recLogGet := httptest.NewRecorder()
	gw.handleLogin(recLogGet, reqLogGet)
	if recLogGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recLogGet.Code)
	}

	// 2. handleLogin - Invalid Body
	reqLogBad := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader("bad-json"))
	recLogBad := httptest.NewRecorder()
	gw.handleLogin(recLogBad, reqLogBad)
	if recLogBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recLogBad.Code)
	}

	// 3. handleLogin - Success
	reqLogSuccess := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"alice","password":"pwd"}`))
	recLogSuccess := httptest.NewRecorder()
	gw.handleLogin(recLogSuccess, reqLogSuccess)
	if recLogSuccess.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recLogSuccess.Code)
	}

	// 4. handleLogin - Unauthenticated error
	authClient.issueTokenErr = status.Error(codes.Unauthenticated, "invalid credentials")
	recLogUnauth := httptest.NewRecorder()
	reqLogUnauth := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"alice","password":"wrong"}`))
	gw.handleLogin(recLogUnauth, reqLogUnauth)
	if recLogUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recLogUnauth.Code)
	}
	authClient.issueTokenErr = nil

	// 5. handleRefresh - Method Not Allowed
	reqRefGet := httptest.NewRequest(http.MethodGet, "/api/v1/auth/refresh", nil)
	recRefGet := httptest.NewRecorder()
	gw.handleRefresh(recRefGet, reqRefGet)
	if recRefGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recRefGet.Code)
	}

	// 6. handleRefresh - Invalid Body
	reqRefBad := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", strings.NewReader("bad-json"))
	recRefBad := httptest.NewRecorder()
	gw.handleRefresh(recRefBad, reqRefBad)
	if recRefBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recRefBad.Code)
	}

	// 7. handleRefresh - Success
	reqRefSuccess := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", strings.NewReader(`{"refresh_token":"tok-123"}`))
	recRefSuccess := httptest.NewRecorder()
	gw.handleRefresh(recRefSuccess, reqRefSuccess)
	if recRefSuccess.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recRefSuccess.Code)
	}

	// 8. handleRefresh - Unauthenticated error
	authClient.refreshTokenErr = status.Error(codes.Unauthenticated, "expired token")
	recRefUnauth := httptest.NewRecorder()
	reqRefUnauth := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", strings.NewReader(`{"refresh_token":"bad-token"}`))
	gw.handleRefresh(recRefUnauth, reqRefUnauth)
	if recRefUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recRefUnauth.Code)
	}
	authClient.refreshTokenErr = nil

	// 9. handleLogout - Method Not Allowed
	reqOutGet := httptest.NewRequest(http.MethodGet, "/api/v1/auth/logout", nil)
	recOutGet := httptest.NewRecorder()
	gw.handleLogout(recOutGet, reqOutGet)
	if recOutGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recOutGet.Code)
	}

	// 10. handleLogout - Missing Bearer
	reqOutNoToken := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	recOutNoToken := httptest.NewRecorder()
	gw.handleLogout(recOutNoToken, reqOutNoToken)
	if recOutNoToken.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recOutNoToken.Code)
	}

	// 11. handleLogout - Success
	reqOutSuccess := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	reqOutSuccess.Header.Set("Authorization", "Bearer valid-token-123")
	recOutSuccess := httptest.NewRecorder()
	gw.handleLogout(recOutSuccess, reqOutSuccess)
	if recOutSuccess.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recOutSuccess.Code)
	}

	// 12. handleAuthToken - Method Not Allowed
	reqTokenDel := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/token", nil)
	recTokenDel := httptest.NewRecorder()
	gw.handleAuthToken(recTokenDel, reqTokenDel)
	if recTokenDel.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recTokenDel.Code)
	}

	// 13. handleAuthToken - GET with query params
	reqTokenGet := httptest.NewRequest(http.MethodGet, "/api/v1/auth/token?sub=alice&role=USER&scope=wallet:read", nil)
	recTokenGet := httptest.NewRecorder()
	gw.handleAuthToken(recTokenGet, reqTokenGet)
	if recTokenGet.Code != http.StatusOK || !strings.Contains(recTokenGet.Body.String(), `"access_token"`) {
		t.Fatalf("expected 200 with token, got %d: %s", recTokenGet.Code, recTokenGet.Body.String())
	}

	// 14. handleAuthToken - POST with JSON body
	reqTokenPost := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader(`{"subject":"admin1","roles":["ADMIN"],"scopes":["cluster:admin"]}`))
	recTokenPost := httptest.NewRecorder()
	gw.handleAuthToken(recTokenPost, reqTokenPost)
	if recTokenPost.Code != http.StatusOK || !strings.Contains(recTokenPost.Body.String(), `"access_token"`) {
		t.Fatalf("expected 200 with token, got %d: %s", recTokenPost.Code, recTokenPost.Body.String())
	}

	// 15. handleAuthToken - default fallback
	reqTokenDefault := httptest.NewRequest(http.MethodGet, "/api/v1/auth/token", nil)
	recTokenDefault := httptest.NewRecorder()
	gw.handleAuthToken(recTokenDefault, reqTokenDefault)
	if recTokenDefault.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recTokenDefault.Code)
	}
}

func TestInitFailoverCoordinatorAndResolver(t *testing.T) {
	// 1. initFailoverCoordinator empty URI -> memory coordinator
	coord, cleanup := initFailoverCoordinator("")
	if coord == nil {
		t.Fatal("expected non-nil failover coordinator")
	}
	cleanup()

	// 2. resolveGatewayServiceAddresses
	p, s, l, a := resolveGatewayServiceAddresses()
	if p == "" || s == "" || l == "" || a == "" {
		t.Fatalf("expected non-empty addresses, got %s, %s, %s, %s", p, s, l, a)
	}

	// 3. handleWallets method not allowed
	gw := &Gateway{}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/wallets", nil)
	rec := httptest.NewRecorder()
	gw.handleWallets(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", rec.Code)
	}
}





