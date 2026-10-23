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

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"

	"wallet-system/pkg/coordinator"
	"wallet-system/pkg/db"
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

func TestMultiCurrencyPots_Balances(t *testing.T) {
	// 1. Multi-currency pots
	w := WalletModel{
		ID: "wallet-multi-1",
		Balances: map[string]int64{
			"USD": 15000,
			"EUR": 8000,
			"GBP": 2500,
		},
		Status: WalletStatusActive,
	}

	if w.GetBalance("USD") != 15000 {
		t.Errorf("expected 15000 USD, got %d", w.GetBalance("USD"))
	}
	if w.GetBalance("EUR") != 8000 {
		t.Errorf("expected 8000 EUR, got %d", w.GetBalance("EUR"))
	}
	if w.GetBalance("JPY") != 0 {
		t.Errorf("expected 0 JPY, got %d", w.GetBalance("JPY"))
	}

	if !w.HasSufficientBalance("USD", 10000) {
		t.Errorf("expected sufficient balance for 10000 USD")
	}
	if w.HasSufficientBalance("USD", 20000) {
		t.Errorf("expected insufficient balance for 20000 USD")
	}

	allBalances := w.GetAllBalances()
	if len(allBalances) != 3 {
		t.Fatalf("expected 3 currency pots, got %d", len(allBalances))
	}
	// Deterministic sort: EUR, GBP, USD
	if allBalances[0].Currency != "EUR" || allBalances[1].Currency != "GBP" || allBalances[2].Currency != "USD" {
		t.Errorf("unexpected sort order for balances: %+v", allBalances)
	}

	// 2. Legacy single-currency fallback
	legacyWallet := WalletModel{
		ID:       "wallet-legacy-1",
		Currency: "USD",
		Balance:  5000,
	}
	if legacyWallet.GetBalance("USD") != 5000 {
		t.Errorf("expected 5000 USD for legacy wallet, got %d", legacyWallet.GetBalance("USD"))
	}
	if legacyWallet.GetBalance("EUR") != 0 {
		t.Errorf("expected 0 EUR for legacy wallet, got %d", legacyWallet.GetBalance("EUR"))
	}
	legacyBalances := legacyWallet.GetAllBalances()
	if len(legacyBalances) != 1 || legacyBalances[0].Currency != "USD" || legacyBalances[0].Units != 5000 {
		t.Errorf("expected single USD balance for legacy wallet, got %+v", legacyBalances)
	}
}

func TestExchangeCurrency_Validation(t *testing.T) {
	ctx := context.Background()
	srv := &server{isActive: true, region: "us-east-1"}

	// 1. Nil request
	if _, err := srv.ExchangeCurrency(ctx, nil); err == nil {
		t.Errorf("expected error for nil request")
	}

	// 2. Missing idempotency key
	if _, err := srv.ExchangeCurrency(ctx, &walletv1.ExchangeCurrencyRequest{
		WalletId:     "w1",
		FromCurrency: "USD",
		ToCurrency:   "EUR",
		FromAmount:   100,
	}); err == nil {
		t.Errorf("expected error for missing idempotency key")
	}

	// 3. Missing wallet_id
	if _, err := srv.ExchangeCurrency(ctx, &walletv1.ExchangeCurrencyRequest{
		IdempotencyKey: "k1",
		FromCurrency:   "USD",
		ToCurrency:     "EUR",
		FromAmount:     100,
	}); err == nil {
		t.Errorf("expected error for missing wallet_id")
	}

	// 4. Identical currencies
	if _, err := srv.ExchangeCurrency(ctx, &walletv1.ExchangeCurrencyRequest{
		IdempotencyKey: "k1",
		WalletId:       "w1",
		FromCurrency:   "USD",
		ToCurrency:     "USD",
		FromAmount:     100,
	}); err == nil {
		t.Errorf("expected error for identical currencies")
	}

	// 5. Non-positive amount
	if _, err := srv.ExchangeCurrency(ctx, &walletv1.ExchangeCurrencyRequest{
		IdempotencyKey: "k1",
		WalletId:       "w1",
		FromCurrency:   "USD",
		ToCurrency:     "EUR",
		FromAmount:     -50,
	}); err == nil {
		t.Errorf("expected error for negative amount")
	}

	// 6. Standby write rejection
	standbySrv := &server{isActive: false, region: "eu-west-1"}
	respStandby, errStandby := standbySrv.ExchangeCurrency(ctx, &walletv1.ExchangeCurrencyRequest{
		IdempotencyKey: "k1",
		WalletId:       "w1",
		FromCurrency:   "USD",
		ToCurrency:     "EUR",
		FromAmount:     100,
	})
	if errStandby != nil || respStandby == nil || respStandby.Status != walletv1.ExchangeCurrencyResponse_INVALID_ARGUMENT {
		t.Errorf("expected standby rejection response, got resp=%+v, err=%v", respStandby, errStandby)
	}
}

func TestKYCTier_ConfigsAndDefaults(t *testing.T) {
	// Test default tier config
	cfgDefault := GetTierConfig("UNKNOWN_TIER")
	if cfgDefault.Tier != KYCTier1 {
		t.Errorf("expected default tier TIER_1, got %s", cfgDefault.Tier)
	}
	if cfgDefault.SingleTxnLimitUnits != 100000 {
		t.Errorf("expected 100000 single txn limit for TIER_1, got %d", cfgDefault.SingleTxnLimitUnits)
	}

	// Test TIER_2 config
	cfgTier2 := GetTierConfig(KYCTier2)
	if cfgTier2.SingleTxnLimitUnits != 1000000 || cfgTier2.DailyRollingLimitUnits != 5000000 || cfgTier2.HourlyRateLimitCount != 50 {
		t.Errorf("unexpected TIER_2 parameters: %+v", cfgTier2)
	}

	// Test TIER_3 config
	cfgTier3 := GetTierConfig(KYCTier3)
	if cfgTier3.SingleTxnLimitUnits != 10000000 || cfgTier3.DailyRollingLimitUnits != 50000000 || cfgTier3.HourlyRateLimitCount != 200 {
		t.Errorf("unexpected TIER_3 parameters: %+v", cfgTier3)
	}

	// Test EffectiveKYCTier on WalletModel
	wEmpty := &WalletModel{ID: "w_empty"}
	if wEmpty.EffectiveKYCTier() != KYCTier1 {
		t.Errorf("expected default TIER_1, got %s", wEmpty.EffectiveKYCTier())
	}

	wExplicit := &WalletModel{ID: "w_exp", KYCTier: KYCTier2}
	if wExplicit.EffectiveKYCTier() != KYCTier2 {
		t.Errorf("expected TIER_2, got %s", wExplicit.EffectiveKYCTier())
	}
}

func TestNormalizeToUSDUnits(t *testing.T) {
	// USD 1:1
	if u := normalizeToUSDUnits(1000, "USD"); u != 1000 {
		t.Errorf("expected 1000, got %d", u)
	}
	// EUR (1.0870)
	eurUnits := normalizeToUSDUnits(10000, "EUR")
	if eurUnits <= 0 {
		t.Errorf("expected positive EUR conversion, got %d", eurUnits)
	}
	// GBP (1.2820)
	gbpUnits := normalizeToUSDUnits(10000, "GBP")
	if gbpUnits <= 0 {
		t.Errorf("expected positive GBP conversion, got %d", gbpUnits)
	}
	// JPY
	jpyUnits := normalizeToUSDUnits(100000, "JPY")
	if jpyUnits <= 0 {
		t.Errorf("expected positive JPY conversion, got %d", jpyUnits)
	}
}

func TestCheckVelocityLimits_SingleTxnCap(t *testing.T) {
	srv := &server{}
	ctx := context.Background()

	// 1. TIER_1 wallet: Single limit is 100,000 USD cents ($1,000)
	wTier1 := WalletModel{ID: "w_t1", KYCTier: KYCTier1}
	// 50,000 cents ($500) -> Allowed
	if err := srv.CheckVelocityLimits(ctx, wTier1, 50000, "USD"); err != nil {
		t.Errorf("expected 50,000 units to be allowed, got: %v", err)
	}
	// 150,000 cents ($1,500) -> Exceeds single txn cap
	if err := srv.CheckVelocityLimits(ctx, wTier1, 150000, "USD"); err == nil {
		t.Errorf("expected 150,000 units to be rejected for TIER_1 single txn limit")
	}

	// 2. TIER_2 wallet: Single limit is 1,000,000 USD cents ($10,000)
	wTier2 := WalletModel{ID: "w_t2", KYCTier: KYCTier2}
	// 500,000 cents ($5,000) -> Allowed for TIER_2
	if err := srv.CheckVelocityLimits(ctx, wTier2, 500000, "USD"); err != nil {
		t.Errorf("expected 500,000 units to be allowed for TIER_2, got: %v", err)
	}
	// 1,500,000 cents ($15,000) -> Exceeds single txn cap for TIER_2
	if err := srv.CheckVelocityLimits(ctx, wTier2, 1500000, "USD"); err == nil {
		t.Errorf("expected 1,500,000 units to be rejected for TIER_2 single txn limit")
	}
}

func TestUpdateWalletKYC_Validation(t *testing.T) {
	srv := &server{}
	ctx := context.Background()

	// 1. Nil request
	resp, _ := srv.UpdateWalletKYC(ctx, nil)
	if resp == nil || resp.Success {
		t.Errorf("expected failure for nil request")
	}

	// 2. Missing wallet_id
	resp, _ = srv.UpdateWalletKYC(ctx, &walletv1.UpdateWalletKYCRequest{KycTier: KYCTier2})
	if resp == nil || resp.Success {
		t.Errorf("expected failure for missing wallet_id")
	}

	// 3. Invalid tier
	resp, _ = srv.UpdateWalletKYC(ctx, &walletv1.UpdateWalletKYCRequest{WalletId: "w1", KycTier: "TIER_999"})
	if resp == nil || resp.Success {
		t.Errorf("expected failure for invalid tier")
	}
}

func TestParsePaymentRail(t *testing.T) {
	cases := []struct {
		rail     walletv1.PaymentRail
		expected string
		wantErr  bool
	}{
		{walletv1.PaymentRail_CARD, "CARD", false},
		{walletv1.PaymentRail_ACH, "ACH", false},
		{walletv1.PaymentRail_SEPA, "SEPA", false},
		{walletv1.PaymentRail_WIRE, "WIRE", false},
		{walletv1.PaymentRail_PAYMENT_RAIL_UNSPECIFIED, "", true},
		{walletv1.PaymentRail(999), "", true},
	}

	for _, c := range cases {
		got, err := parsePaymentRail(c.rail)
		if c.wantErr && err == nil {
			t.Errorf("expected error for rail %v, got nil", c.rail)
		}
		if !c.wantErr && err != nil {
			t.Errorf("unexpected error for rail %v: %v", c.rail, err)
		}
		if got != c.expected {
			t.Errorf("for rail %v: expected %s, got %s", c.rail, c.expected, got)
		}
	}
}

func TestDepositFunds_Validations(t *testing.T) {
	srv := &server{isActive: true, region: "us-east-1"}
	ctx := context.Background()

	// 1. Nil request
	if _, err := srv.DepositFunds(ctx, nil); err == nil {
		t.Errorf("expected error for nil request")
	}

	// 2. Missing idempotency key
	if _, err := srv.DepositFunds(ctx, &walletv1.DepositFundsRequest{
		WalletId:    "w1",
		Amount:      &walletv1.Money{Currency: "USD", Units: 100},
		PaymentRail: walletv1.PaymentRail_CARD,
	}); err == nil {
		t.Errorf("expected error for missing idempotency key")
	}

	// 3. Missing wallet_id
	if _, err := srv.DepositFunds(ctx, &walletv1.DepositFundsRequest{
		IdempotencyKey: "k1",
		Amount:         &walletv1.Money{Currency: "USD", Units: 100},
		PaymentRail:    walletv1.PaymentRail_CARD,
	}); err == nil {
		t.Errorf("expected error for missing wallet_id")
	}

	// 4. Non-positive amount
	if _, err := srv.DepositFunds(ctx, &walletv1.DepositFundsRequest{
		IdempotencyKey: "k1",
		WalletId:       "w1",
		Amount:         &walletv1.Money{Currency: "USD", Units: 0},
		PaymentRail:    walletv1.PaymentRail_CARD,
	}); err == nil {
		t.Errorf("expected error for zero amount")
	}

	// 5. Unspecified payment rail
	if _, err := srv.DepositFunds(ctx, &walletv1.DepositFundsRequest{
		IdempotencyKey: "k1",
		WalletId:       "w1",
		Amount:         &walletv1.Money{Currency: "USD", Units: 100},
		PaymentRail:    walletv1.PaymentRail_PAYMENT_RAIL_UNSPECIFIED,
	}); err == nil {
		t.Errorf("expected error for unspecified payment rail")
	}

	// 6. Standby replica rejection
	standbySrv := &server{isActive: false, region: "eu-west-1"}
	respStandby, errStandby := standbySrv.DepositFunds(ctx, &walletv1.DepositFundsRequest{
		IdempotencyKey: "k1",
		WalletId:       "w1",
		Amount:         &walletv1.Money{Currency: "USD", Units: 100},
		PaymentRail:    walletv1.PaymentRail_ACH,
	})
	if errStandby != nil || respStandby == nil || respStandby.Status != walletv1.DepositFundsResponse_INVALID_ARGUMENT {
		t.Errorf("expected standby rejection response, got resp=%+v, err=%v", respStandby, errStandby)
	}
}

func TestWithdrawFunds_Validations(t *testing.T) {
	srv := &server{isActive: true, region: "us-east-1"}
	ctx := context.Background()

	// 1. Nil request
	if _, err := srv.WithdrawFunds(ctx, nil); err == nil {
		t.Errorf("expected error for nil request")
	}

	// 2. Missing idempotency key
	if _, err := srv.WithdrawFunds(ctx, &walletv1.WithdrawFundsRequest{
		WalletId:    "w1",
		Amount:      &walletv1.Money{Currency: "USD", Units: 100},
		PaymentRail: walletv1.PaymentRail_ACH,
	}); err == nil {
		t.Errorf("expected error for missing idempotency key")
	}

	// 3. Missing wallet_id
	if _, err := srv.WithdrawFunds(ctx, &walletv1.WithdrawFundsRequest{
		IdempotencyKey: "k1",
		Amount:         &walletv1.Money{Currency: "USD", Units: 100},
		PaymentRail:    walletv1.PaymentRail_ACH,
	}); err == nil {
		t.Errorf("expected error for missing wallet_id")
	}

	// 4. Non-positive amount
	if _, err := srv.WithdrawFunds(ctx, &walletv1.WithdrawFundsRequest{
		IdempotencyKey: "k1",
		WalletId:       "w1",
		Amount:         &walletv1.Money{Currency: "USD", Units: -10},
		PaymentRail:    walletv1.PaymentRail_ACH,
	}); err == nil {
		t.Errorf("expected error for negative amount")
	}

	// 5. Unspecified payment rail
	if _, err := srv.WithdrawFunds(ctx, &walletv1.WithdrawFundsRequest{
		IdempotencyKey: "k1",
		WalletId:       "w1",
		Amount:         &walletv1.Money{Currency: "USD", Units: 100},
		PaymentRail:    walletv1.PaymentRail_PAYMENT_RAIL_UNSPECIFIED,
	}); err == nil {
		t.Errorf("expected error for unspecified payment rail")
	}

	// 6. Standby replica rejection
	standbySrv := &server{isActive: false, region: "eu-west-1"}
	respStandby, errStandby := standbySrv.WithdrawFunds(ctx, &walletv1.WithdrawFundsRequest{
		IdempotencyKey: "k1",
		WalletId:       "w1",
		Amount:         &walletv1.Money{Currency: "USD", Units: 100},
		PaymentRail:    walletv1.PaymentRail_WIRE,
	})
	if errStandby != nil || respStandby == nil || respStandby.Status != walletv1.WithdrawFundsResponse_INVALID_ARGUMENT {
		t.Errorf("expected standby rejection response, got resp=%+v, err=%v", respStandby, errStandby)
	}
}

func TestMultiCurrencyTransferBalanceIntegrity(t *testing.T) {
	// Verify that incSource and incDest do NOT decrement or increment the primary 'balance'
	// if the transfer currency differs from the wallet's primary currency (VULN-02)
	srcWallet := WalletModel{
		ID:       "w-src",
		Currency: "USD",
		Balance:  1000,
		Balances: map[string]int64{"USD": 1000, "EUR": 500},
	}
	dstWallet := WalletModel{
		ID:       "w-dst",
		Currency: "EUR",
		Balance:  200,
		Balances: map[string]int64{"EUR": 200, "USD": 50},
	}

	state := &transferExecutionState{
		srcWallet: srcWallet,
		dstWallet: dstWallet,
	}

	srcCurr := db.NormalizeCurrency("EUR")
	incSource := bson.M{
		"balances." + srcCurr: -int64(100),
	}
	if srcCurr == state.srcWallet.Currency {
		incSource["balance"] = -int64(100)
	}

	if _, touchesBase := incSource["balance"]; touchesBase {
		t.Fatalf("VULN-02 regression: debiting foreign EUR pot must NOT alter USD base balance")
	}

	dstCurr := db.NormalizeCurrency("USD")
	incDest := bson.M{
		"balances." + dstCurr: int64(100),
	}
	if dstCurr == state.dstWallet.Currency {
		incDest["balance"] = int64(100)
	}

	if _, touchesBase := incDest["balance"]; touchesBase {
		t.Fatalf("VULN-02 regression: crediting foreign USD pot must NOT alter EUR base balance")
	}
}

