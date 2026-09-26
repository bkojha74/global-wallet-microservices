package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	ledgerv1 "wallet-system/proto/ledger"
)

type mockLedgerTaskStore struct {
	tasks         []LedgerTask
	completedIDs  []primitive.ObjectID
	failedIDs     []primitive.ObjectID
	findErr       error
	markCompErr   error
	markFailedErr error
}

func (m *mockLedgerTaskStore) FindPending(_ context.Context, limit int64) ([]LedgerTask, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	if int64(len(m.tasks)) > limit {
		return m.tasks[:limit], nil
	}
	return m.tasks, nil
}

func (m *mockLedgerTaskStore) MarkCompleted(_ context.Context, id primitive.ObjectID, _ time.Time) error {
	if m.markCompErr != nil {
		return m.markCompErr
	}
	m.completedIDs = append(m.completedIDs, id)
	return nil
}

func (m *mockLedgerTaskStore) MarkFailed(_ context.Context, id primitive.ObjectID, _ string) error {
	if m.markFailedErr != nil {
		return m.markFailedErr
	}
	m.failedIDs = append(m.failedIDs, id)
	return nil
}

func TestLedgerRelay_OfflineUnit(t *testing.T) {
	// 1. Check constructor defaults with nil db
	mockSuccess := &mockLedgerClient{
		recordResp: &ledgerv1.RecordTransactionResponse{Success: true},
	}
	relay := NewLedgerRelay(nil, mockSuccess, 0, 0, nil)
	if relay.pollInterval != 1*time.Second {
		t.Errorf("Expected default pollInterval 1s, got %v", relay.pollInterval)
	}
	if relay.batchSize != 50 {
		t.Errorf("Expected default batchSize 50, got %d", relay.batchSize)
	}

	task := LedgerTask{
		ID:                  primitive.NewObjectID(),
		TransactionID:       "tx-offline-1",
		IdempotencyKey:      "idemp-offline-1",
		SourceWalletID:      "alice",
		DestinationWalletID: "bob",
		Amount:              100,
		Currency:            "USD",
		Region:              "us-east-1",
	}

	// 2. DispatchImmediate with nil database collection and successful RPC
	err := relay.DispatchImmediate(context.Background(), task)
	if err != nil {
		t.Errorf("Expected nil error when col is nil and RPC succeeds, got %v", err)
	}

	// 3. DispatchImmediate error handling (RPC error)
	mockFailErr := &mockLedgerClient{
		recordErr: errors.New("connection failed"),
	}
	relayFailErr := NewLedgerRelay(nil, mockFailErr, 100*time.Millisecond, 10, nil)
	err = relayFailErr.DispatchImmediate(context.Background(), task)
	if err == nil {
		t.Errorf("Expected error when RPC fails")
	}

	// 4. DispatchImmediate error handling (Response failure)
	mockFailResp := &mockLedgerClient{
		recordResp: &ledgerv1.RecordTransactionResponse{Success: false, ErrorMessage: "insufficient funds"},
	}
	relayFailResp := NewLedgerRelay(nil, mockFailResp, 100*time.Millisecond, 10, nil)
	err = relayFailResp.DispatchImmediate(context.Background(), task)
	if err == nil {
		t.Errorf("Expected error when RPC response success is false")
	}

	// 5. ProcessBatch with mock store (Success branch)
	mockStore := &mockLedgerTaskStore{
		tasks: []LedgerTask{task},
	}
	relayWithStore := NewLedgerRelay(nil, mockSuccess, 100*time.Millisecond, 10, nil)
	relayWithStore.store = mockStore

	count, err := relayWithStore.ProcessBatch(context.Background())
	if err != nil || count != 1 {
		t.Errorf("Expected ProcessBatch to process 1 task, got count=%d, err=%v", count, err)
	}
	if len(mockStore.completedIDs) != 1 {
		t.Errorf("Expected 1 completed ID in mock store")
	}

	// 6. ProcessBatch with mock store (Failure branch)
	mockStoreFail := &mockLedgerTaskStore{
		tasks: []LedgerTask{task},
	}
	relayWithStoreFail := NewLedgerRelay(nil, mockFailErr, 100*time.Millisecond, 10, nil)
	relayWithStoreFail.store = mockStoreFail

	countFail, errFail := relayWithStoreFail.ProcessBatch(context.Background())
	if errFail != nil || countFail != 0 {
		t.Errorf("Expected ProcessBatch count 0 on failure, got count=%d, err=%v", countFail, errFail)
	}
	if len(mockStoreFail.failedIDs) != 1 {
		t.Errorf("Expected 1 failed ID in mock store")
	}

	// 7. ProcessBatch with store error
	mockStoreErr := &mockLedgerTaskStore{
		findErr: errors.New("db query error"),
	}
	relayWithStoreErr := NewLedgerRelay(nil, mockSuccess, 100*time.Millisecond, 10, nil)
	relayWithStoreErr.store = mockStoreErr
	if _, err := relayWithStoreErr.ProcessBatch(context.Background()); err == nil {
		t.Errorf("Expected error from ProcessBatch when FindPending fails")
	}

	// 8. mongoLedgerTaskStore coverage with nil col
	nilStore := &mongoLedgerTaskStore{col: nil}
	if tasks, err := nilStore.FindPending(context.Background(), 10); err != nil || tasks != nil {
		t.Errorf("Expected nil tasks and nil error for nil col in FindPending")
	}
	if err := nilStore.MarkCompleted(context.Background(), primitive.NewObjectID(), time.Now()); err != nil {
		t.Errorf("Expected nil error for nil col in MarkCompleted")
	}
	if err := nilStore.MarkFailed(context.Background(), primitive.NewObjectID(), "err"); err != nil {
		t.Errorf("Expected nil error for nil col in MarkFailed")
	}

	// 9. Start and Stop background relay loop with fast interval
	relayFast := NewLedgerRelay(nil, mockSuccess, 5*time.Millisecond, 5, nil)
	relayFast.store = &mockLedgerTaskStore{tasks: []LedgerTask{task}}
	ctx, cancel := context.WithCancel(context.Background())
	relayFast.Start(ctx)
	time.Sleep(20 * time.Millisecond)
	cancel()
	relayFast.Stop()
}
