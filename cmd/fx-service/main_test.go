package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wallet-system/pkg/observability"
	fxv1 "wallet-system/proto/fx"
)

func TestThirdPartyRateProviders(t *testing.T) {
	ctx := context.Background()

	// 1. Mock Frankfurter ECB 3rd-party API server
	frankfurterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"amount":1.0,"base":"USD","date":"2026-09-30","rates":{"EUR":0.915,"GBP":0.785,"JPY":149.5}}`))
	}))
	defer frankfurterSrv.Close()

	fp := NewFrankfurterProvider(frankfurterSrv.URL, 2*time.Second)
	snap, err := fp.FetchRates(ctx, "USD")
	if err != nil {
		t.Fatalf("expected Frankfurter provider to succeed, got: %v", err)
	}
	if snap.Rates["EUR"] != 0.915 || snap.Rates["JPY"] != 149.5 {
		t.Fatalf("unexpected rates from Frankfurter: %+v", snap.Rates)
	}

	// 2. Mock Open ExchangeRate-API server
	erSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"success","base_code":"USD","rates":{"EUR":0.921,"BHD":0.376,"INR":83.75}}`))
	}))
	defer erSrv.Close()

	erp := NewExchangeRateAPIProvider(erSrv.URL, "test-api-key", 2*time.Second)
	snapER, err := erp.FetchRates(ctx, "USD")
	if err != nil {
		t.Fatalf("expected ExchangeRate-API provider to succeed, got: %v", err)
	}
	if snapER.Rates["BHD"] != 0.376 || snapER.Rates["INR"] != 83.75 {
		t.Fatalf("unexpected rates from ExchangeRate-API: %+v", snapER.Rates)
	}

	// 3. CompositeRateProvider fallback when primary fails
	failingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failingSrv.Close()

	comp := &CompositeRateProvider{
		Providers: []RateProvider{
			NewFrankfurterProvider(failingSrv.URL, time.Second),
			fp,
		},
		Fallback: &BaselineProvider{},
	}
	mergedSnap, err := comp.FetchRates(ctx, "USD")
	if err != nil {
		t.Fatalf("expected composite provider to succeed via fallback chain: %v", err)
	}
	if mergedSnap.ProviderName != "frankfurter-ecb" {
		t.Fatalf("expected frankfurter-ecb provider name, got %s", mergedSnap.ProviderName)
	}
	// Ensure BHD (not in Frankfurter mock) is filled by InstitutionalBaselineRates
	if mergedSnap.Rates["BHD"] != 0.376 {
		t.Fatalf("expected baseline BHD rate 0.376 to be merged, got %f", mergedSnap.Rates["BHD"])
	}
}

func TestFXEngineQuotesTriangulationAndIdempotency(t *testing.T) {
	ctx := context.Background()
	engine := NewFXEngine(nil, &BaselineProvider{}, 25, 0) // 25 bps spread

	// 1. GetRate USD -> EUR
	rateResp, err := engine.GetRate(ctx, "USD", "EUR")
	if err != nil {
		t.Fatalf("GetRate USD/EUR failed: %v", err)
	}
	if rateResp.MidRate != 0.92 {
		t.Fatalf("expected USD/EUR mid_rate 0.92, got %f", rateResp.MidRate)
	}
	if rateResp.EffectiveRate >= rateResp.MidRate {
		t.Fatalf("expected effective_rate (%f) < mid_rate (%f) with 25 bps spread", rateResp.EffectiveRate, rateResp.MidRate)
	}

	// 2. Triangulation EUR -> GBP (both non-USD)
	crossRate, err := engine.GetRate(ctx, "EUR", "GBP")
	if err != nil {
		t.Fatalf("GetRate EUR/GBP failed: %v", err)
	}
	if crossRate.MidRate <= 0.80 || crossRate.MidRate >= 0.90 {
		t.Fatalf("unexpected triangulated EUR/GBP mid_rate: %f", crossRate.MidRate)
	}

	// 3. CreateQuote & ConvertCurrency with locked quote
	quote, err := engine.CreateQuote(ctx, "USD", "EUR", 1000, "partner-acme", 60)
	if err != nil {
		t.Fatalf("CreateQuote failed: %v", err)
	}
	if !strings.HasPrefix(quote.QuoteId, "fxq_") || quote.Status != QuoteStatusActive {
		t.Fatalf("unexpected quote: %+v", quote)
	}
	if quote.TargetAmount <= 0 {
		t.Fatalf("expected positive target amount, got %d", quote.TargetAmount)
	}

	conv1, err := engine.ConvertCurrency(ctx, &fxv1.ConvertCurrencyRequest{
		IdempotencyKey: "fx-idemp-001",
		QuoteId:        quote.QuoteId,
		BaseCurrency:   "USD",
		TargetCurrency: "EUR",
		SourceAmount:   1000,
		ClientId:       "partner-acme",
	})
	if err != nil {
		t.Fatalf("ConvertCurrency with quote failed: %v", err)
	}
	if conv1.Status != ConversionStatusCompleted || conv1.TargetAmount != quote.TargetAmount {
		t.Fatalf("unexpected conversion result: %+v", conv1)
	}

	// 4. Idempotent Replay with same IdempotencyKey returns DUPLICATE_REPLAY
	convReplay, err := engine.ConvertCurrency(ctx, &fxv1.ConvertCurrencyRequest{
		IdempotencyKey: "fx-idemp-001",
		QuoteId:        quote.QuoteId,
		BaseCurrency:   "USD",
		TargetCurrency: "EUR",
		SourceAmount:   1000,
	})
	if err != nil {
		t.Fatalf("expected idempotent replay to succeed, got: %v", err)
	}
	if convReplay.Status != ConversionStatusReplay || convReplay.ConversionId != conv1.ConversionId {
		t.Fatalf("expected DUPLICATE_REPLAY with same conversion_id, got %+v", convReplay)
	}

	// 5. Re-using an already EXECUTED quote with a different idempotency key must fail
	_, err = engine.ConvertCurrency(ctx, &fxv1.ConvertCurrencyRequest{
		IdempotencyKey: "fx-idemp-002",
		QuoteId:        quote.QuoteId,
		BaseCurrency:   "USD",
		TargetCurrency: "EUR",
		SourceAmount:   1000,
	})
	if err == nil || !strings.Contains(err.Error(), "already been executed") {
		t.Fatalf("expected error re-using executed quote, got: %v", err)
	}
}

func TestFXB2BHTTPServerAndAuth(t *testing.T) {
	engine := NewFXEngine(nil, &BaselineProvider{}, 0, 0) // 0 spread for exact 1:0.92 check
	srv := &fxServer{
		engine: engine,
		logger: observability.NewMemoryLogger(),
	}
	handler := buildFXHTTPHandler(srv, "secret-partner-key")

	// 1. /healthz is public (no API key needed)
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recHealth := httptest.NewRecorder()
	handler.ServeHTTP(recHealth, reqHealth)
	if recHealth.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on /healthz, got %d", recHealth.Code)
	}

	// 2. /api/v1/fx/rates without API key returns 401 Unauthorized
	reqUnauth := httptest.NewRequest(http.MethodGet, "/api/v1/fx/rates?base=USD&target=EUR", nil)
	recUnauth := httptest.NewRecorder()
	handler.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without API key, got %d", recUnauth.Code)
	}

	// 3. /api/v1/fx/rates with X-API-Key returns 200 OK
	reqRate := httptest.NewRequest(http.MethodGet, "/api/v1/fx/rates?base=USD&target=EUR", nil)
	reqRate.Header.Set("X-API-Key", "secret-partner-key")
	recRate := httptest.NewRecorder()
	handler.ServeHTTP(recRate, reqRate)
	if recRate.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with X-API-Key, got %d: %s", recRate.Code, recRate.Body.String())
	}

	// 4. POST /api/v1/fx/quotes -> 201 Created
	quotePayload := `{"base_currency":"USD","target_currency":"EUR","source_amount":1000,"client_id":"fintech-partner-1"}`
	reqQuote := httptest.NewRequest(http.MethodPost, "/api/v1/fx/quotes", bytes.NewBufferString(quotePayload))
	reqQuote.Header.Set("Authorization", "Bearer secret-partner-key")
	recQuote := httptest.NewRecorder()
	handler.ServeHTTP(recQuote, reqQuote)
	if recQuote.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for quote, got %d: %s", recQuote.Code, recQuote.Body.String())
	}

	var createdQuote fxv1.Quote
	if err := json.Unmarshal(recQuote.Body.Bytes(), &createdQuote); err != nil {
		t.Fatalf("failed to decode quote response: %v", err)
	}
	if createdQuote.TargetAmount != 920 {
		t.Fatalf("expected 920 EUR target_amount for 1000 USD at 0.92, got %d", createdQuote.TargetAmount)
	}

	// 5. POST /api/v1/fx/convert -> 200 OK
	convPayload := `{"idempotency_key":"http-conv-1","quote_id":"` + createdQuote.QuoteId + `"}`
	reqConv := httptest.NewRequest(http.MethodPost, "/api/v1/fx/convert", bytes.NewBufferString(convPayload))
	reqConv.Header.Set("X-API-Key", "secret-partner-key")
	recConv := httptest.NewRecorder()
	handler.ServeHTTP(recConv, reqConv)
	if recConv.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for convert, got %d: %s", recConv.Code, recConv.Body.String())
	}

	// 6. POST /api/v1/fx/rates/refresh manual rate override
	overridePayload := `{"base_currency":"USD","target_currency":"EUR","mid_rate":0.95,"spread_bps":10}`
	reqRefresh := httptest.NewRequest(http.MethodPost, "/api/v1/fx/rates/refresh", bytes.NewBufferString(overridePayload))
	reqRefresh.Header.Set("X-API-Key", "secret-partner-key")
	recRefresh := httptest.NewRecorder()
	handler.ServeHTTP(recRefresh, reqRefresh)
	if recRefresh.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for manual rate override, got %d: %s", recRefresh.Code, recRefresh.Body.String())
	}

	// 7. CORS preflight OPTIONS request
	reqOptions := httptest.NewRequest(http.MethodOptions, "/api/v1/fx/rates", nil)
	recOptions := httptest.NewRecorder()
	handler.ServeHTTP(recOptions, reqOptions)
	if recOptions.Code != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for CORS OPTIONS, got %d", recOptions.Code)
	}
	if recOptions.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected CORS Access-Control-Allow-Origin: *")
	}

	// 8. Multi-key auth support
	multiKeyHandler := buildFXHTTPHandler(srv, "key1, key2, key3")
	reqMulti := httptest.NewRequest(http.MethodGet, "/api/v1/fx/currencies", nil)
	reqMulti.Header.Set("X-API-Key", "key2")
	recMulti := httptest.NewRecorder()
	multiKeyHandler.ServeHTTP(recMulti, reqMulti)
	if recMulti.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with secondary key in multi-key config, got %d", recMulti.Code)
	}
}

func TestFXScaleAwareQuoteCreation(t *testing.T) {
	ctx := context.Background()
	engine := NewFXEngine(nil, &BaselineProvider{}, 0, 0) // 0 spread

	// USD (scale 2) -> JPY (scale 0)
	// $10.00 USD (1000 cents) at 150.00 JPY/USD = 1500 JPY
	quoteJPY, err := engine.CreateQuote(ctx, "USD", "JPY", 1000, "client-scale-test", 60)
	if err != nil {
		t.Fatalf("CreateQuote USD->JPY failed: %v", err)
	}
	if quoteJPY.TargetAmount != 1500 {
		t.Fatalf("expected 1500 JPY for 1000 USD cents at 150.0 rate, got %d", quoteJPY.TargetAmount)
	}

	// USD (scale 2) -> BHD (scale 3)
	// $10.00 USD (1000 cents) at 0.376 BHD/USD = 3760 fils
	quoteBHD, err := engine.CreateQuote(ctx, "USD", "BHD", 1000, "client-scale-test", 60)
	if err != nil {
		t.Fatalf("CreateQuote USD->BHD failed: %v", err)
	}
	if quoteBHD.TargetAmount != 3760 {
		t.Fatalf("expected 3760 BHD fils for 1000 USD cents at 0.376 rate, got %d", quoteBHD.TargetAmount)
	}
}
