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
	"google.golang.org/grpc/metadata"

	ledgerv1 "wallet-system/proto/ledger"
	walletv1 "wallet-system/proto/wallet"
)

type fakeWalletClient struct {
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

func (f *fakeWalletClient) GetBalance(context.Context, *walletv1.GetBalanceRequest, ...grpc.CallOption) (*walletv1.GetBalanceResponse, error) {
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
