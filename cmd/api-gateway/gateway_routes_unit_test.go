package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wallet-system/pkg/coordinator"
	walletv1 "wallet-system/proto/wallet"
)

type mockWalletClient struct {
	walletv1.WalletServiceClient
}

func (m *mockWalletClient) HealthCheck(ctx context.Context, in *walletv1.HealthRequest, opts ...grpc.CallOption) (*walletv1.HealthResponse, error) {
	return &walletv1.HealthResponse{Status: "SERVING"}, nil
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

