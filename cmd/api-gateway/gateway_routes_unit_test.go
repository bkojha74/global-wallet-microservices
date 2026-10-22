package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wallet-system/pkg/coordinator"
	"wallet-system/pkg/webhook"
	walletv1 "wallet-system/proto/wallet"
)

type mockWalletClient struct {
	walletv1.WalletServiceClient
}

func (m *mockWalletClient) HealthCheck(ctx context.Context, in *walletv1.HealthRequest, opts ...grpc.CallOption) (*walletv1.HealthResponse, error) {
	return &walletv1.HealthResponse{Status: "SERVING"}, nil
}

func (m *mockWalletClient) ExchangeCurrency(ctx context.Context, in *walletv1.ExchangeCurrencyRequest, opts ...grpc.CallOption) (*walletv1.ExchangeCurrencyResponse, error) {
	if in.FromAmount <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "amount must be positive")
	}
	return &walletv1.ExchangeCurrencyResponse{
		TransactionId:   "tx-mock-exch-1",
		Status:          walletv1.ExchangeCurrencyResponse_SUCCESS,
		HandledByRegion: "us-test-1",
		ExchangeRate:    0.92,
		DebitedAmount:   &walletv1.Money{Currency: in.FromCurrency, Units: in.FromAmount},
		CreditedAmount:  &walletv1.Money{Currency: in.ToCurrency, Units: int64(float64(in.FromAmount) * 0.92)},
	}, nil
}

func (m *mockWalletClient) UpdateWalletKYC(ctx context.Context, in *walletv1.UpdateWalletKYCRequest, opts ...grpc.CallOption) (*walletv1.UpdateWalletKYCResponse, error) {
	if in.KycTier == "INVALID" {
		return &walletv1.UpdateWalletKYCResponse{Success: false, Message: "invalid kyc_tier"}, nil
	}
	return &walletv1.UpdateWalletKYCResponse{
		Success:      true,
		WalletId:     in.WalletId,
		PreviousTier: "TIER_1",
		CurrentTier:  in.KycTier,
		Message:      "KYC tier updated successfully",
	}, nil
}

func (m *mockWalletClient) GetWalletLimits(ctx context.Context, in *walletv1.GetWalletLimitsRequest, opts ...grpc.CallOption) (*walletv1.GetWalletLimitsResponse, error) {
	return &walletv1.GetWalletLimitsResponse{
		WalletId:               in.WalletId,
		KycTier:                "TIER_1",
		SingleTransactionLimit: 100000,
		DailyRollingLimit:      500000,
		DailyRollingSpent:      50000,
		DailyRollingRemaining:  450000,
		HourlyRateLimit:        10,
		HourlyRateSpent:        1,
		HourlyRateRemaining:    9,
	}, nil
}

func (m *mockWalletClient) DepositFunds(ctx context.Context, in *walletv1.DepositFundsRequest, opts ...grpc.CallOption) (*walletv1.DepositFundsResponse, error) {
	if in.Amount == nil || in.Amount.Units <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "amount must be positive")
	}
	return &walletv1.DepositFundsResponse{
		TransactionId:     "tx-mock-dep-1",
		Status:            walletv1.DepositFundsResponse_SUCCESS,
		HandledByRegion:   "us-test-1",
		CreditedAmount:    in.Amount,
		PaymentRail:       in.PaymentRail,
		ExternalReference: in.Reference,
	}, nil
}

func (m *mockWalletClient) WithdrawFunds(ctx context.Context, in *walletv1.WithdrawFundsRequest, opts ...grpc.CallOption) (*walletv1.WithdrawFundsResponse, error) {
	if in.Amount == nil || in.Amount.Units <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "amount must be positive")
	}
	if in.Amount.Units > 1000000 {
		return &walletv1.WithdrawFundsResponse{
			TransactionId:   "tx-mock-wth-fail",
			Status:          walletv1.WithdrawFundsResponse_FAILED_VELOCITY_LIMIT_EXCEEDED,
			ErrorMessage:    "velocity limit exceeded",
			HandledByRegion: "us-test-1",
		}, nil
	}
	return &walletv1.WithdrawFundsResponse{
		TransactionId:     "tx-mock-wth-1",
		Status:            walletv1.WithdrawFundsResponse_SUCCESS,
		HandledByRegion:   "us-test-1",
		DebitedAmount:     in.Amount,
		PaymentRail:       in.PaymentRail,
		ExternalReference: in.BeneficiaryDetails,
	}, nil
}

func TestGatewayEmitAndWriteJSON(t *testing.T) {
	_ = walletv1.CreateWalletRequest{}
	gw := &Gateway{}

	// Test emit when logger is nil
	gw.emit(context.Background(), "EVT", "INFO", "msg", map[string]any{"k": "v"})
	gw.emitTerminal(context.Background(), "EVT", "INFO", "msg", 10, true, map[string]any{"k": "v"})

	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from writeJSON")
	}
}

func TestResolveServiceAddresses(t *testing.T) {
	t.Setenv("PRIMARY_WALLET_ADDR", "p:50051")
	t.Setenv("STANDBY_WALLET_ADDR", "s:50051")
	t.Setenv("AUTH_SERVICE_ADDR", "a:50053")

	p, s, _, a := resolveGatewayServiceAddresses()
	if p != "p:50051" || s != "s:50051" || a != "a:50053" {
		t.Fatalf("unexpected service addresses: %s %s %s", p, s, a)
	}

	os.Unsetenv("PRIMARY_WALLET_ADDR")
	addr := resolveServiceAddress("PRIMARY_WALLET_ADDR", "wallet-service-primary", "50051")
	if addr == "" {
		t.Fatalf("expected non-empty fallback address")
	}
}

func TestGetClientDialOption(t *testing.T) {
	os.Unsetenv("GRPC_TLS_ENABLED")
	opt, err := getClientDialOption()
	if err != nil || opt == nil {
		t.Fatalf("expected insecure credentials option")
	}

	t.Setenv("GRPC_TLS_ENABLED", "true")
	t.Setenv("GRPC_CLIENT_CERT", "invalid.crt")
	t.Setenv("GRPC_CLIENT_KEY", "invalid.key")
	t.Setenv("GRPC_CA_CERT", "invalid.ca")
	_, err = getClientDialOption()
	if err == nil {
		t.Fatalf("expected error for invalid mTLS certs")
	}
}

func TestHandleWalletGRPCErrorAndIsUnauthenticated(t *testing.T) {
	w := httptest.NewRecorder()
	handleWalletGRPCError(w, status.Error(codes.AlreadyExists, "already exists"), "test")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for AlreadyExists code, got %d", w.Code)
	}

	w2 := httptest.NewRecorder()
	handleWalletGRPCError(w2, status.Error(codes.InvalidArgument, "invalid arg"), "test")
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for InvalidArgument code, got %d", w2.Code)
	}

	if !isUnauthenticated(status.Error(codes.Unauthenticated, "unauth")) {
		t.Fatalf("expected true for isUnauthenticated")
	}
	if isUnauthenticated(errors.New("generic error")) {
		t.Fatalf("expected false for generic error")
	}
}

func TestParseCreateWalletRequest(t *testing.T) {
	// JSON body
	bodyJSON := `{"currency":"USD"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/wallets", bytes.NewBufferString(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	cwReq, err := parseCreateWalletRequest(req, "trace-1")
	if err != nil || cwReq.Currency != "USD" {
		t.Fatalf("failed to parse JSON create wallet request: %v", err)
	}
}

func TestBuildAuthTokenClaimsAndParams(t *testing.T) {
	claims, roles, scopes := buildAuthTokenClaims("user1", "admin", "read write")
	if claims.Subject != "user1" || len(roles) == 0 || len(scopes) == 0 {
		t.Fatalf("unexpected token claims build result")
	}
}

func TestGatewayRoutesRegistration(t *testing.T) {
	gw := &Gateway{
		activeTarget:  coordinator.TargetPrimary,
		primaryClient: &mockWalletClient{},
		standbyClient: &mockWalletClient{},
	}

	mux := http.NewServeMux()
	registerGatewayRoutes(mux, gw)

	// Test GET /readyz
	reqR := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	wR := httptest.NewRecorder()
	mux.ServeHTTP(wR, reqR)
	if wR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /readyz, got %d", wR.Code)
	}

	// Test router wrapper
	router := buildGatewayRouter(gw)
	if router == nil {
		t.Fatalf("expected non-nil router")
	}
}

func TestGatewayRouteValidationBranches(t *testing.T) {
	gw := &Gateway{
		activeTarget:  coordinator.TargetPrimary,
		primaryClient: &mockWalletClient{},
		standbyClient: &mockWalletClient{},
	}

	// 1. handleAuthToken with POST body
	bodyAuth := `{"subject":"bob","roles":["admin"],"scopes":["read"]}`
	reqAuth := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", bytes.NewBufferString(bodyAuth))
	wAuth := httptest.NewRecorder()
	gw.handleAuthToken(wAuth, reqAuth)
	if wAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 for handleAuthToken POST body, got %d", wAuth.Code)
	}

	// 2. parseCreateWalletRequest error paths
	bodyInvalidCurr := `{"currency":"FAKECURR"}`
	reqInvCurr := httptest.NewRequest(http.MethodPost, "/api/v1/wallets", bytes.NewBufferString(bodyInvalidCurr))
	if _, err := parseCreateWalletRequest(reqInvCurr, "tr"); err == nil {
		t.Fatal("expected error for invalid currency in parseCreateWalletRequest")
	}

	bodyNegBal := `{"currency":"USD","initial_balance":-50}`
	reqNegBal := httptest.NewRequest(http.MethodPost, "/api/v1/wallets", bytes.NewBufferString(bodyNegBal))
	if _, err := parseCreateWalletRequest(reqNegBal, "tr"); err == nil {
		t.Fatal("expected error for negative initial balance in parseCreateWalletRequest")
	}

	// 3. handleWallets method not allowed
	reqDelWallets := httptest.NewRequest(http.MethodDelete, "/api/v1/wallets", nil)
	wDelWallets := httptest.NewRecorder()
	gw.handleWallets(wDelWallets, reqDelWallets)
	if wDelWallets.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for DELETE /api/v1/wallets, got %d", wDelWallets.Code)
	}

	// 4. handleGetBalance missing id
	reqGetBalNoID := httptest.NewRequest(http.MethodGet, "/api/v1/wallets", nil)
	wGetBalNoID := httptest.NewRecorder()
	gw.handleGetBalance(wGetBalNoID, reqGetBalNoID)
	if wGetBalNoID.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for get balance missing id, got %d", wGetBalNoID.Code)
	}

	// 5. handleTransfer bad requests
	reqTxPostBadJSON := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", bytes.NewBufferString("bad-json"))
	wTxPostBadJSON := httptest.NewRecorder()
	gw.handleTransfer(wTxPostBadJSON, reqTxPostBadJSON)
	if wTxPostBadJSON.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for transfer bad json, got %d", wTxPostBadJSON.Code)
	}

	reqTxMissingWallets := httptest.NewRequest(http.MethodPost, "/api/v1/transfers", bytes.NewBufferString(`{"amount":100,"currency":"USD"}`))
	wTxMissingWallets := httptest.NewRecorder()
	gw.handleTransfer(wTxMissingWallets, reqTxMissingWallets)
	if wTxMissingWallets.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for transfer missing wallets, got %d", wTxMissingWallets.Code)
	}

	// 6. handleLedger missing wallet_id
	reqLedgerNoID := httptest.NewRequest(http.MethodGet, "/api/v1/ledger", nil)
	wLedgerNoID := httptest.NewRecorder()
	gw.handleLedger(wLedgerNoID, reqLedgerNoID)
	if wLedgerNoID.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for ledger missing wallet_id, got %d", wLedgerNoID.Code)
	}
}

func TestGatewayFailoverHandler(t *testing.T) {
	gw := &Gateway{
		activeTarget: coordinator.TargetPrimary,
	}

	// GET is not allowed (405)
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/admin/failover", nil)
	wGet := httptest.NewRecorder()
	gw.handleFailover(wGet, reqGet)
	if wGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET failover status, got %d", wGet.Code)
	}

	// POST toggles target
	reqPost := httptest.NewRequest(http.MethodPost, "/api/v1/admin/failover", nil)
	wPost := httptest.NewRecorder()
	gw.handleFailover(wPost, reqPost)
	if wPost.Code != http.StatusOK {
		t.Fatalf("expected 200 for POST failover toggle, got %d", wPost.Code)
	}
}

func TestRunGatewayServer_CancelledContext(t *testing.T) {
	t.Setenv("HTTP_PORT", "58080")
	t.Setenv("ENVIRONMENT", "test")
	t.Setenv("MONGO_URI", "mongodb://127.0.0.1:27019/?connectTimeoutMS=100")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := runGatewayServer(ctx)
	t.Logf("runGatewayServer returned: %v", err)
}

func TestGatewayWalletExchangeHandler(t *testing.T) {
	mockClient := &mockWalletClient{}
	gw := &Gateway{
		primaryClient: mockClient,
		activeTarget:  coordinator.TargetPrimary,
	}

	// 1. GET not allowed (405)
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/exchange", nil)
	wGet := httptest.NewRecorder()
	gw.handleWalletExchange(wGet, reqGet)
	if wGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET exchange, got %d", wGet.Code)
	}

	// 2. Missing wallet_id (400)
	bodyNoWallet := bytes.NewBufferString(`{"from_currency":"USD","to_currency":"EUR","amount":100}`)
	reqNoWallet := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/exchange", bodyNoWallet)
	wNoWallet := httptest.NewRecorder()
	gw.handleWalletExchange(wNoWallet, reqNoWallet)
	if wNoWallet.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing wallet_id, got %d", wNoWallet.Code)
	}

	// 3. Same currencies (400)
	bodySameCurr := bytes.NewBufferString(`{"wallet_id":"alice","from_currency":"USD","to_currency":"USD","amount":100}`)
	reqSameCurr := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/exchange", bodySameCurr)
	wSameCurr := httptest.NewRecorder()
	gw.handleWalletExchange(wSameCurr, reqSameCurr)
	if wSameCurr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for identical currencies, got %d", wSameCurr.Code)
	}

	// 4. Successful exchange (200)
	bodySuccess := bytes.NewBufferString(`{"wallet_id":"alice","from_currency":"USD","to_currency":"EUR","from_amount":1000}`)
	reqSuccess := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/exchange", bodySuccess)
	wSuccess := httptest.NewRecorder()
	gw.handleWalletExchange(wSuccess, reqSuccess)
	if wSuccess.Code != http.StatusOK {
		t.Fatalf("expected 200 for successful exchange, got %d (%s)", wSuccess.Code, wSuccess.Body.String())
	}
}

func TestHandleUpdateWalletKYC_Routes(t *testing.T) {
	gw := &Gateway{
		primaryClient: &mockWalletClient{},
		standbyClient: &mockWalletClient{},
		activeTarget:  coordinator.TargetPrimary,
	}

	// 1. GET not allowed (405)
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/kyc", nil)
	wGet := httptest.NewRecorder()
	gw.handleUpdateWalletKYC(wGet, reqGet)
	if wGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET kyc, got %d", wGet.Code)
	}

	// 2. Missing wallet_id (400)
	bodyNoWallet := bytes.NewBufferString(`{"kyc_tier":"TIER_2"}`)
	reqNoWallet := httptest.NewRequest(http.MethodPut, "/api/v1/wallets/kyc", bodyNoWallet)
	wNoWallet := httptest.NewRecorder()
	gw.handleUpdateWalletKYC(wNoWallet, reqNoWallet)
	if wNoWallet.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing wallet_id, got %d", wNoWallet.Code)
	}

	// 3. Success (200)
	bodySuccess := bytes.NewBufferString(`{"wallet_id":"alice","kyc_tier":"TIER_2"}`)
	reqSuccess := httptest.NewRequest(http.MethodPut, "/api/v1/wallets/kyc", bodySuccess)
	wSuccess := httptest.NewRecorder()
	gw.handleUpdateWalletKYC(wSuccess, reqSuccess)
	if wSuccess.Code != http.StatusOK {
		t.Fatalf("expected 200 for successful KYC update, got %d (%s)", wSuccess.Code, wSuccess.Body.String())
	}
}

func TestHandleGetWalletLimits_Routes(t *testing.T) {
	gw := &Gateway{
		primaryClient: &mockWalletClient{},
		standbyClient: &mockWalletClient{},
		activeTarget:  coordinator.TargetPrimary,
	}

	// 1. POST not allowed (405)
	reqPost := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/limits", nil)
	wPost := httptest.NewRecorder()
	gw.handleGetWalletLimits(wPost, reqPost)
	if wPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST limits, got %d", wPost.Code)
	}

	// 2. Missing 'id' parameter (400)
	reqNoID := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/limits", nil)
	wNoID := httptest.NewRecorder()
	gw.handleGetWalletLimits(wNoID, reqNoID)
	if wNoID.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing id query param, got %d", wNoID.Code)
	}

	// 3. Success (200)
	reqSuccess := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/limits?id=alice", nil)
	wSuccess := httptest.NewRecorder()
	gw.handleGetWalletLimits(wSuccess, reqSuccess)
	if wSuccess.Code != http.StatusOK {
		t.Fatalf("expected 200 for limits query, got %d (%s)", wSuccess.Code, wSuccess.Body.String())
	}
}

func TestHandleDepositFunds_Routes(t *testing.T) {
	gw := &Gateway{
		primaryClient: &mockWalletClient{},
		standbyClient: &mockWalletClient{},
		activeTarget:  coordinator.TargetPrimary,
	}

	// 1. GET method not allowed
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/deposit", nil)
	wGet := httptest.NewRecorder()
	gw.handleDepositFunds(wGet, reqGet)
	if wGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET deposit, got %d", wGet.Code)
	}

	// 2. Missing wallet_id
	bodyNoWallet := bytes.NewBufferString(`{"amount":1000,"currency":"USD","payment_rail":"CARD"}`)
	reqNoWallet := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/deposit", bodyNoWallet)
	wNoWallet := httptest.NewRecorder()
	gw.handleDepositFunds(wNoWallet, reqNoWallet)
	if wNoWallet.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing wallet_id, got %d", wNoWallet.Code)
	}

	// 3. Unsupported payment rail
	bodyBadRail := bytes.NewBufferString(`{"wallet_id":"alice","amount":1000,"currency":"USD","payment_rail":"BITCOIN"}`)
	reqBadRail := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/deposit", bodyBadRail)
	wBadRail := httptest.NewRecorder()
	gw.handleDepositFunds(wBadRail, reqBadRail)
	if wBadRail.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad rail, got %d", wBadRail.Code)
	}

	// 4. Success via /api/v1/wallets/deposit
	bodySuccess := bytes.NewBufferString(`{"wallet_id":"alice","amount":5000,"currency":"USD","payment_rail":"CARD","reference":"ref_123"}`)
	reqSuccess := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/deposit", bodySuccess)
	wSuccess := httptest.NewRecorder()
	gw.handleDepositFunds(wSuccess, reqSuccess)
	if wSuccess.Code != http.StatusOK {
		t.Fatalf("expected 200 for successful deposit, got %d (%s)", wSuccess.Code, wSuccess.Body.String())
	}

	// 5. Success via subroute /api/v1/wallets/{id}/deposit
	bodySubroute := bytes.NewBufferString(`{"amount":2500,"currency":"USD","payment_rail":"ACH"}`)
	reqSubroute := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/alice/deposit", bodySubroute)
	wSubroute := httptest.NewRecorder()
	gw.handleWalletSubroutes(wSubroute, reqSubroute)
	if wSubroute.Code != http.StatusOK {
		t.Fatalf("expected 200 for subroute deposit, got %d (%s)", wSubroute.Code, wSubroute.Body.String())
	}
}

func TestHandleWithdrawFunds_Routes(t *testing.T) {
	gw := &Gateway{
		primaryClient: &mockWalletClient{},
		standbyClient: &mockWalletClient{},
		activeTarget:  coordinator.TargetPrimary,
	}

	// 1. GET method not allowed
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/withdraw", nil)
	wGet := httptest.NewRecorder()
	gw.handleWithdrawFunds(wGet, reqGet)
	if wGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET withdraw, got %d", wGet.Code)
	}

	// 2. Missing wallet_id
	bodyNoWallet := bytes.NewBufferString(`{"amount":1000,"currency":"USD","payment_rail":"ACH"}`)
	reqNoWallet := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/withdraw", bodyNoWallet)
	wNoWallet := httptest.NewRecorder()
	gw.handleWithdrawFunds(wNoWallet, reqNoWallet)
	if wNoWallet.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing wallet_id, got %d", wNoWallet.Code)
	}

	// 3. Success via /api/v1/wallets/withdraw
	bodySuccess := bytes.NewBufferString(`{"wallet_id":"alice","amount":5000,"currency":"USD","payment_rail":"ACH","beneficiary_details":"Bank routing 123"}`)
	reqSuccess := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/withdraw", bodySuccess)
	wSuccess := httptest.NewRecorder()
	gw.handleWithdrawFunds(wSuccess, reqSuccess)
	if wSuccess.Code != http.StatusOK {
		t.Fatalf("expected 200 for successful withdraw, got %d (%s)", wSuccess.Code, wSuccess.Body.String())
	}

	// 4. Velocity limit exceeded (429)
	bodyVelocity := bytes.NewBufferString(`{"wallet_id":"alice","amount":5000000,"currency":"USD","payment_rail":"WIRE"}`)
	reqVelocity := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/withdraw", bodyVelocity)
	wVelocity := httptest.NewRecorder()
	gw.handleWithdrawFunds(wVelocity, reqVelocity)
	if wVelocity.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for velocity limit exceeded, got %d (%s)", wVelocity.Code, wVelocity.Body.String())
	}

	// 5. Success via subroute /api/v1/wallets/{id}/withdraw
	bodySubroute := bytes.NewBufferString(`{"amount":1200,"currency":"EUR","payment_rail":"SEPA","beneficiary_details":"IBAN DE12345"}`)
	reqSubroute := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/alice/withdraw", bodySubroute)
	wSubroute := httptest.NewRecorder()
	gw.handleWalletSubroutes(wSubroute, reqSubroute)
	if wSubroute.Code != http.StatusOK {
		t.Fatalf("expected 200 for subroute withdraw, got %d (%s)", wSubroute.Code, wSubroute.Body.String())
	}
}

func TestHandleWebhookSubscriptions(t *testing.T) {
	dispatcher := webhook.NewDispatcher(nil, nil)
	gw := &Gateway{
		webhookDispatcher: dispatcher,
	}

	// 1. Method Not Allowed (PUT)
	reqPut := httptest.NewRequest(http.MethodPut, "/api/v1/webhooks/subscriptions", nil)
	wPut := httptest.NewRecorder()
	gw.handleWebhookSubscriptions(wPut, reqPut)
	if wPut.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for PUT, got %d", wPut.Code)
	}

	// 2. POST with invalid json
	reqBadJSON := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/subscriptions", bytes.NewBufferString(`{invalid`))
	wBadJSON := httptest.NewRecorder()
	gw.handleWebhookSubscriptions(wBadJSON, reqBadJSON)
	if wBadJSON.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad JSON, got %d", wBadJSON.Code)
	}

	// 3. POST with invalid url
	reqBadURL := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/subscriptions", bytes.NewBufferString(`{"target_url":"not-a-valid-url"}`))
	wBadURL := httptest.NewRecorder()
	gw.handleWebhookSubscriptions(wBadURL, reqBadURL)
	if wBadURL.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad URL, got %d", wBadURL.Code)
	}

	// 4. POST successful subscription creation
	reqCreate := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/subscriptions", bytes.NewBufferString(`{"target_url":"https://partner.example.com/webhook","events":["transfer.completed","fraud.flagged"]}`))
	wCreate := httptest.NewRecorder()
	gw.handleWebhookSubscriptions(wCreate, reqCreate)
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 for subscription creation, got %d (%s)", wCreate.Code, wCreate.Body.String())
	}

	// 5. GET subscriptions list
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/subscriptions", nil)
	wGet := httptest.NewRecorder()
	gw.handleWebhookSubscriptions(wGet, reqGet)
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET subscriptions, got %d", wGet.Code)
	}

	// 6. DELETE missing ID
	reqDelMissing := httptest.NewRequest(http.MethodDelete, "/api/v1/webhooks/subscriptions", nil)
	wDelMissing := httptest.NewRecorder()
	gw.handleWebhookSubscriptions(wDelMissing, reqDelMissing)
	if wDelMissing.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for DELETE without id, got %d", wDelMissing.Code)
	}

	// 7. DELETE with valid ID
	subs, err := dispatcher.ListSubscriptions(context.Background(), "")
	if err != nil || len(subs) == 0 {
		t.Fatalf("expected at least 1 subscription, err: %v", err)
	}
	subID := subs[0].ID.Hex()
	reqDel := httptest.NewRequest(http.MethodDelete, "/api/v1/webhooks/subscriptions?id="+subID, nil)
	wDel := httptest.NewRecorder()
	gw.handleWebhookSubscriptions(wDel, reqDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("expected 200 for DELETE with id, got %d (%s)", wDel.Code, wDel.Body.String())
	}
}

func TestHandleWebhookTest(t *testing.T) {
	dispatcher := webhook.NewDispatcher(nil, nil)
	gw := &Gateway{
		webhookDispatcher: dispatcher,
	}

	// Mock partner receiver
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received":true}`))
	}))
	defer mockServer.Close()

	// 1. Method Not Allowed (GET)
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/test", nil)
	wGet := httptest.NewRecorder()
	gw.handleWebhookTest(wGet, reqGet)
	if wGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET test, got %d", wGet.Code)
	}

	// 2. Bad JSON
	reqBadJSON := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/test", bytes.NewBufferString(`bad`))
	wBadJSON := httptest.NewRecorder()
	gw.handleWebhookTest(wBadJSON, reqBadJSON)
	if wBadJSON.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad JSON, got %d", wBadJSON.Code)
	}

	// 3. Invalid target URL
	reqBadURL := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/test", bytes.NewBufferString(`{"target_url":"not-a-url"}`))
	wBadURL := httptest.NewRecorder()
	gw.handleWebhookTest(wBadURL, reqBadURL)
	if wBadURL.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid url, got %d", wBadURL.Code)
	}

	// 4. Ping test endpoint success
	bodyPing := bytes.NewBufferString(`{"target_url":"` + mockServer.URL + `","secret":"sec_test123"}`)
	reqPing := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/test", bodyPing)
	wPing := httptest.NewRecorder()
	gw.handleWebhookTest(wPing, reqPing)
	if wPing.Code != http.StatusOK {
		t.Fatalf("expected 200 for ping, got %d (%s)", wPing.Code, wPing.Body.String())
	}
}

func TestSwaggerAndOpenAPIRoutes(t *testing.T) {
	// 1. Direct handler tests
	// 1a. GET /api-docs/openapi.json
	reqSpec := httptest.NewRequest(http.MethodGet, "/api-docs/openapi.json", nil)
	wSpec := httptest.NewRecorder()
	handleOpenAPISpec(wSpec, reqSpec)
	if wSpec.Code != http.StatusOK {
		t.Fatalf("expected 200 for openapi.json, got %d", wSpec.Code)
	}
	if ct := wSpec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected application/json content-type, got %s", ct)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(wSpec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("failed to parse openapi json: %v", err)
	}
	if doc["openapi"] != "3.0.3" {
		t.Fatalf("expected openapi 3.0.3, got %v", doc["openapi"])
	}

	// 1b. Method Not Allowed on openapi.json
	reqBadMethod := httptest.NewRequest(http.MethodPost, "/api-docs/openapi.json", nil)
	wBadMethod := httptest.NewRecorder()
	handleOpenAPISpec(wBadMethod, reqBadMethod)
	if wBadMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST openapi.json, got %d", wBadMethod.Code)
	}

	// 1c. GET /swagger/
	reqUI := httptest.NewRequest(http.MethodGet, "/swagger/", nil)
	wUI := httptest.NewRecorder()
	handleSwaggerUI(wUI, reqUI)
	if wUI.Code != http.StatusOK {
		t.Fatalf("expected 200 for /swagger/, got %d", wUI.Code)
	}
	if ct := wUI.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("expected text/html content-type, got %s", ct)
	}
	if !strings.Contains(wUI.Body.String(), "SwaggerUIBundle") {
		t.Fatalf("expected HTML body to contain SwaggerUIBundle initialization")
	}

	// 1d. Redirects /swagger and /docs to /swagger/
	for _, p := range []string{"/swagger", "/docs"} {
		reqRedir := httptest.NewRequest(http.MethodGet, p, nil)
		wRedir := httptest.NewRecorder()
		handleSwaggerRedirect(wRedir, reqRedir)
		if wRedir.Code != http.StatusMovedPermanently {
			t.Fatalf("expected 301 for %s, got %d", p, wRedir.Code)
		}
		if loc := wRedir.Header().Get("Location"); loc != "/swagger/" {
			t.Fatalf("expected Location /swagger/, got %s", loc)
		}
	}

	// 2. Full router stack integration (middleware, public path bypass, and CSP)
	gw := &Gateway{
		primaryClient: &mockWalletClient{},
		standbyClient: &mockWalletClient{},
		activeTarget:  coordinator.TargetPrimary,
	}
	router := buildGatewayRouter(gw)

	// Verify unauthenticated request to /api-docs/openapi.json succeeds through router
	reqRouterSpec := httptest.NewRequest(http.MethodGet, "/api-docs/openapi.json", nil)
	wRouterSpec := httptest.NewRecorder()
	router.ServeHTTP(wRouterSpec, reqRouterSpec)
	if wRouterSpec.Code != http.StatusOK {
		t.Fatalf("expected 200 via router for openapi.json, got %d", wRouterSpec.Code)
	}

	// Verify unauthenticated request to /swagger/ succeeds through router with tailored CSP
	reqRouterUI := httptest.NewRequest(http.MethodGet, "/swagger/", nil)
	wRouterUI := httptest.NewRecorder()
	router.ServeHTTP(wRouterUI, reqRouterUI)
	if wRouterUI.Code != http.StatusOK {
		t.Fatalf("expected 200 via router for /swagger/, got %d", wRouterUI.Code)
	}
	csp := wRouterUI.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "cdn.jsdelivr.net") {
		t.Fatalf("expected CSP to allow cdn.jsdelivr.net for swagger UI, got %s", csp)
	}
}

func TestGateway_LedgerMerkleRoutes(t *testing.T) {
	mockLedgerMgmt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/audit/merkle/latest":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"checkpoint_id":"mcp_1","merkle_root":"root123","entry_count":10}`))
		case "/audit/merkle/checkpoints":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"checkpoint_id":"mcp_1","merkle_root":"root123"}]`))
		case "/audit/merkle/proof":
			if r.URL.Query().Get("entry_hash") == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"missing entry_hash"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"entry_hash":"eh1","merkle_root":"root123","verified":true}`))
		case "/audit/merkle/checkpoint":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"checkpoint_id":"mcp_new","merkle_root":"root456"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockLedgerMgmt.Close()

	gw := &Gateway{
		primaryClient: &mockWalletClient{},
		standbyClient: &mockWalletClient{},
		activeTarget:  coordinator.TargetPrimary,
		ledgerMgmtURL: mockLedgerMgmt.URL,
		httpClient:    mockLedgerMgmt.Client(),
	}
	router := buildGatewayRouter(gw)

	t.Run("GET /api/v1/ledger/merkle/latest public bypass", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ledger/merkle/latest", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "root123") {
			t.Errorf("expected root123 in response, got: %s", rec.Body.String())
		}
	})

	t.Run("GET /api/v1/ledger/merkle/proof public bypass", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ledger/merkle/proof?entry_hash=eh1", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"verified":true`) {
			t.Errorf("expected verified true, got: %s", rec.Body.String())
		}
	})

	t.Run("GET /api/v1/ledger/merkle defaults to latest", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ledger/merkle", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("Ledger service unavailable returns 502", func(t *testing.T) {
		badGW := &Gateway{
			ledgerMgmtURL: "http://127.0.0.1:59999",
			httpClient:    &http.Client{Timeout: 50 * time.Millisecond},
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ledger/merkle/latest", nil)
		rec := httptest.NewRecorder()
		badGW.handleLedgerMerkle(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("expected 502 Bad Gateway, got %d", rec.Code)
		}
	})
}
