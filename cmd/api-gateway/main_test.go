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
