package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	ledgerv1 "wallet-system/proto/ledger"
)

type mockLedgerTaskStore struct {
	mu            sync.RWMutex
	tasks         []LedgerTask
	completedIDs  []primitive.ObjectID
	failedIDs     []primitive.ObjectID
	permFailedIDs []primitive.ObjectID
	findErr       error
	markCompErr   error
	markFailedErr error
}

func (m *mockLedgerTaskStore) FindPending(_ context.Context, limit int64) ([]LedgerTask, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.findErr != nil {
		return nil, m.findErr
	}
	if int64(len(m.tasks)) > limit {
		return m.tasks[:limit], nil
	}
	return m.tasks, nil
}

func (m *mockLedgerTaskStore) MarkCompleted(_ context.Context, id primitive.ObjectID, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markCompErr != nil {
		return m.markCompErr
	}
	m.completedIDs = append(m.completedIDs, id)
	return nil
}

func (m *mockLedgerTaskStore) MarkFailed(_ context.Context, id primitive.ObjectID, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markFailedErr != nil {
		return m.markFailedErr
	}
	m.failedIDs = append(m.failedIDs, id)
	return nil
}

func (m *mockLedgerTaskStore) MarkPermanentlyFailed(_ context.Context, id primitive.ObjectID, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markFailedErr != nil {
		return m.markFailedErr
	}
	m.permFailedIDs = append(m.permFailedIDs, id)
	return nil
}

func (m *mockLedgerTaskStore) CompletedCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.completedIDs)
}

func (m *mockLedgerTaskStore) FailedCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.failedIDs)
}

func (m *mockLedgerTaskStore) PermanentlyFailedCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.permFailedIDs)
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
	if errFail == nil || countFail != 0 {
		t.Errorf("Expected ProcessBatch error and count 0 on failure, got count=%d, err=%v", countFail, errFail)
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

type mockLeaseManager struct {
	mu           sync.Mutex
	holderID     string
	hasLease     bool
	acquireErr   error
	releaseErr   error
	acquireCalls int
	releaseCalls int
}

func (m *mockLeaseManager) TryAcquireOrRenew(_ context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acquireCalls++
	return m.hasLease, m.acquireErr
}

func (m *mockLeaseManager) Release(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseCalls++
	m.hasLease = false
	return m.releaseErr
}

func (m *mockLeaseManager) HolderID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.holderID
}

func (m *mockLeaseManager) SetHasLease(val bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hasLease = val
}

func (m *mockLeaseManager) ReleaseCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.releaseCalls
}

func TestLedgerRelay_WithLeaseManager_LeaderElection(t *testing.T) {
	mockSuccess := &mockLedgerClient{
		recordResp: &ledgerv1.RecordTransactionResponse{Success: true},
	}
	mockStore := &mockLedgerTaskStore{
		tasks: []LedgerTask{{
			ID:             primitive.NewObjectID(),
			TransactionID:  "tx-lease-1",
			IdempotencyKey: "idemp-lease-1",
		}},
	}

	lm := &mockLeaseManager{
		holderID: "wallet-pod-test-1",
		hasLease: false, // Initially standby
	}

	relay := NewLedgerRelay(nil, mockSuccess, 10*time.Millisecond, 10, nil, WithLeaseManager(lm))
	relay.store = mockStore

	if relay.IsLeader() {
		t.Errorf("Expected relay to start as non-leader when lease is unheld")
	}

	// 1. Standby pod tick: should NOT process tasks
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	relay.Start(ctx)
	time.Sleep(35 * time.Millisecond)

	if count := mockStore.CompletedCount(); count != 0 {
		t.Errorf("Standby pod should NOT process tasks, completed: %d", count)
	}

	// 2. Transition to Leader
	lm.SetHasLease(true)
	time.Sleep(35 * time.Millisecond)

	if !relay.IsLeader() {
		t.Errorf("Expected relay to transition to leader")
	}
	if count := mockStore.CompletedCount(); count == 0 {
		t.Errorf("Elected leader should have processed tasks")
	}

	// 3. Graceful stop should release lease
	relay.Stop()
	time.Sleep(10 * time.Millisecond)

	if lm.ReleaseCalls() == 0 {
		t.Errorf("Expected Release to be called on graceful shutdown")
	}
}

func TestMongoRelayLeaseManager_NilColSafety(t *testing.T) {
	mgr := NewMongoRelayLeaseManager(nil, "test-instance", 5*time.Second)
	if mgr.HolderID() != "test-instance" {
		t.Errorf("Expected holder ID test-instance, got %s", mgr.HolderID())
	}

	hasLease, err := mgr.TryAcquireOrRenew(context.Background())
	if err != nil || !hasLease {
		t.Errorf("Expected (true, nil) for nil col graceful fallback, got (%v, %v)", hasLease, err)
	}

	if err := mgr.Release(context.Background()); err != nil {
		t.Errorf("Expected nil error on Release with nil col, got %v", err)
	}

	// Test default ttl and instance ID resolution
	defaultMgr := NewMongoRelayLeaseManager(nil, "", 0)
	if defaultMgr.ttl != DefaultLeaseTTL {
		t.Errorf("Expected default ttl %v, got %v", DefaultLeaseTTL, defaultMgr.ttl)
	}
	if defaultMgr.HolderID() == "" {
		t.Errorf("Expected non-empty resolved instance ID")
	}
}

func TestLedgerRelay_CalculateBackoff(t *testing.T) {
	relay := NewLedgerRelay(nil, nil, 100*time.Millisecond, 10, nil, WithMaxBackoff(2*time.Second))

	// 0 consecutive errors should return base pollInterval
	b0 := relay.CalculateBackoff(0)
	if b0 != 100*time.Millisecond {
		t.Errorf("Expected 100ms for 0 errors, got %v", b0)
	}

	// 1 consecutive error: multiplier 2^0 = 1, cap = 100ms, range = 0 -> exactly 100ms
	b1 := relay.CalculateBackoff(1)
	if b1 != 100*time.Millisecond {
		t.Errorf("Expected 100ms for 1 error, got %v", b1)
	}

	// Increasing errors should stay bounded by pollInterval and maxBackoff
	for i := 1; i <= 20; i++ {
		backoff := relay.CalculateBackoff(i)
		if backoff < 100*time.Millisecond {
			t.Errorf("Backoff for attempt %d was below base interval: %v", i, backoff)
		}
		if backoff > 2*time.Second {
			t.Errorf("Backoff for attempt %d exceeded maxBackoff: %v", i, backoff)
		}
	}

	// Edge case: maxBackoff <= pollInterval
	relayNoLimit := NewLedgerRelay(nil, nil, 500*time.Millisecond, 10, nil, WithMaxBackoff(100*time.Millisecond))
	if b := relayNoLimit.CalculateBackoff(5); b != 500*time.Millisecond {
		t.Errorf("Expected pollInterval when maxBackoff <= pollInterval, got %v", b)
	}
}

func TestLedgerRelay_MaxRetriesHeadOfLineUnblocking(t *testing.T) {
	poisonTask := LedgerTask{
		ID:             primitive.NewObjectID(),
		IdempotencyKey: "poison-task-1",
		Retries:        DefaultMaxLedgerTaskRetries - 1, // Will reach max retries on this failure
	}
	mockClientFail := &mockLedgerClient{
		recordErr: fmt.Errorf("ledger hard rejection"),
	}

	mockStore := &mockLedgerTaskStore{
		tasks: []LedgerTask{poisonTask},
	}
	relay := &LedgerRelay{
		store:        mockStore,
		ledgerClient: mockClientFail,
		batchSize:    10,
	}

	count, err := relay.ProcessBatch(context.Background())
	if err == nil {
		t.Fatalf("expected error from ProcessBatch when task fails")
	}
	if count != 0 {
		t.Fatalf("expected 0 processed count, got %d", count)
	}
	if mockStore.PermanentlyFailedCount() != 1 {
		t.Fatalf("expected 1 task marked permanently failed, got %d", mockStore.PermanentlyFailedCount())
	}
}

