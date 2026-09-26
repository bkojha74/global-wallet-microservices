package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"google.golang.org/grpc"

	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	ledgerv1 "wallet-system/proto/ledger"
)

type mockLedgerClient struct {
	ledgerv1.LedgerServiceClient
	recordErr  error
	recordResp *ledgerv1.RecordTransactionResponse
}

func (m *mockLedgerClient) RecordTransaction(_ context.Context, _ *ledgerv1.RecordTransactionRequest, _ ...grpc.CallOption) (*ledgerv1.RecordTransactionResponse, error) {
	return m.recordResp, m.recordErr
}

func TestLedgerRelayLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 1)
	if err != nil {
		t.Skipf("MongoDB unavailable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	walletDB := client.Database("test_wallet_relay_pkg")
	_ = walletDB.Drop(ctx)

	tasksCol := walletDB.Collection("ledger_tasks")

	task := LedgerTask{
		ID:                  primitive.NewObjectID(),
		TransactionID:       "tx-relay-100",
		IdempotencyKey:      "idemp-relay-100",
		SourceWalletID:      "alice",
		DestinationWalletID: "bob",
		Amount:              500,
		Currency:            "USD",
		Region:              "us-east-1",
		Status:              LedgerTaskStatusPending,
		CreatedAt:           time.Now().UTC(),
	}
	_, err = tasksCol.InsertOne(ctx, task)
	if err != nil {
		t.Fatalf("InsertOne failed: %v", err)
	}

	// 1. Success path
	mockSuccess := &mockLedgerClient{
		recordResp: &ledgerv1.RecordTransactionResponse{Success: true},
	}
	logger := observability.NewMemoryLogger()
	relay := NewLedgerRelay(walletDB, mockSuccess, 100*time.Millisecond, 10, logger)

	// DispatchImmediate
	if err := relay.DispatchImmediate(ctx, task); err != nil {
		t.Fatalf("DispatchImmediate failed: %v", err)
	}

	// ProcessBatch
	count, errBatch := relay.ProcessBatch(ctx)
	if errBatch != nil {
		t.Fatalf("ProcessBatch failed: %v, count=%d", errBatch, count)
	}

	// 2. Failure path
	mockFail := &mockLedgerClient{
		recordErr: errors.New("rpc timeout"),
	}
	relayFail := NewLedgerRelay(walletDB, mockFail, 0, 0, logger)
	if err := relayFail.DispatchImmediate(ctx, task); err == nil {
		t.Fatal("expected error on DispatchImmediate when RPC fails")
	}

	// Start & Stop
	relayFail.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	relayFail.Stop()
}
