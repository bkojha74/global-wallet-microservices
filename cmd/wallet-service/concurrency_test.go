package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"wallet-system/pkg/coordinator"
	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	ledgerv1 "wallet-system/proto/ledger"
	walletv1 "wallet-system/proto/wallet"
)

func TestStandbyWriteFencing(t *testing.T) {
	ctx := context.Background()
	srv := &server{
		region:   "eu-west-1",
		isActive: false, // Standby replica
	}

	// 1. CreateWallet must be rejected on standby
	_, err := srv.CreateWallet(ctx, &walletv1.CreateWalletRequest{
		WalletId:       "test-standby-wallet",
		Currency:       "USD",
		InitialBalance: 100,
	})
	if err == nil {
		t.Fatal("expected error on standby CreateWallet, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	if !strings.Contains(st.Message(), "instance is standby replica") {
		t.Fatalf("unexpected error message: %s", st.Message())
	}

	// 2. TransferFunds must be rejected on standby
	resp, err := srv.TransferFunds(ctx, &walletv1.TransferFundsRequest{
		IdempotencyKey:      "standby-transfer-test",
		SourceWalletId:      "wallet-a",
		DestinationWalletId: "wallet-b",
		Amount:              &walletv1.Money{Currency: "USD", Units: 50},
	})
	if err == nil {
		t.Fatal("expected error on standby TransferFunds, got nil")
	}
	st, ok = status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	if resp != nil && resp.Status != walletv1.TransferFundsResponse_INTERNAL_ERROR {
		t.Fatalf("expected INTERNAL_ERROR in proto response, got %v", resp.Status)
	}

	// 3. HealthCheck must report STANDBY and is_active=false
	healthResp, err := srv.HealthCheck(ctx, &walletv1.HealthRequest{})
	if err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}
	if healthResp.Status != "STANDBY" || healthResp.IsActive != false {
		t.Fatalf("expected STANDBY/false, got %s/%t", healthResp.Status, healthResp.IsActive)
	}
}

func TestDynamicCoordinatorPromotion(t *testing.T) {
	ctx := context.Background()
	coord := coordinator.NewMemoryFailoverCoordinator(coordinator.TargetStandby)

	srv := &server{
		region:      "us-east-1",
		isActive:    true, // Boot active, but coordinator controls cluster state
		targetRole:  coordinator.TargetPrimary,
		coordinator: coord,
	}

	// Initially coordinator active target is STANDBY, so our PRIMARY role is not writable
	if srv.isWritable(ctx) {
		t.Fatal("expected server to be non-writable when coordinator target is STANDBY")
	}

	healthResp, err := srv.HealthCheck(ctx, &walletv1.HealthRequest{})
	if err != nil || healthResp.Status != "STANDBY" || healthResp.IsActive != false {
		t.Fatalf("expected STANDBY, got %v / %+v", err, healthResp)
	}

	// Promote PRIMARY via coordinator
	if err := coord.SetActiveTarget(ctx, coordinator.TargetPrimary); err != nil {
		t.Fatalf("failed to set active target: %v", err)
	}

	// Server should now be dynamically writable
	if !srv.isWritable(ctx) {
		t.Fatal("expected server to be writable after coordinator promotion")
	}

	healthResp, err = srv.HealthCheck(ctx, &walletv1.HealthRequest{})
	if err != nil || healthResp.Status != "ACTIVE" || healthResp.IsActive != true {
		t.Fatalf("expected ACTIVE, got %v / %+v", err, healthResp)
	}
}

func TestStandardGRPCHealthProtocol(t *testing.T) {
	ctx := context.Background()
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("wallet.v1.WalletService", grpc_health_v1.HealthCheckResponse_SERVING)

	// Check overall status
	resp, err := healthServer.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING, got %v", resp.Status)
	}

	// Check specific service
	resp, err = healthServer.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "wallet.v1.WalletService"})
	if err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING, got %v", resp.Status)
	}

	// Simulate shutdown
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	resp, err = healthServer.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("expected NOT_SERVING, got %v", resp.Status)
	}
}

func TestConcurrentOverdraftRaceLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable for live concurrency test: %v", err)
		return
	}
	defer func() { _ = client.Disconnect(ctx) }()

	database := client.Database("test_concurrency_wallet_db")
	_ = database.Drop(ctx)
	defer func() { _ = database.Drop(ctx) }()

	if err := db.EnsureWalletIndexes(ctx, database); err != nil {
		t.Fatalf("failed to ensure indexes: %v", err)
	}

	srv := &server{
		mongoClient: client,
		dbName:      "test_concurrency_wallet_db",
		region:      "test-concurrency-region",
		isActive:    true,
	}

	nowNano := time.Now().UnixNano()
	sourceWallet := fmt.Sprintf("alice-race-%d", nowNano)
	destWallet := fmt.Sprintf("bob-race-%d", nowNano)

	// Seed source wallet with 100 USD (Units: 100)
	_, err = srv.CreateWallet(ctx, &walletv1.CreateWalletRequest{
		WalletId:       sourceWallet,
		Currency:       "USD",
		InitialBalance: 100,
	})
	if err != nil {
		t.Fatalf("failed to seed source wallet: %v", err)
	}

	// Seed destination wallet with 0 USD
	_, err = srv.CreateWallet(ctx, &walletv1.CreateWalletRequest{
		WalletId:       destWallet,
		Currency:       "USD",
		InitialBalance: 0,
	})
	if err != nil {
		t.Fatalf("failed to seed destination wallet: %v", err)
	}

	// Concurrently launch 50 transfers of $10 each
	// Exactly 10 must succeed ($100 / $10 = 10); 40 must fail with INSUFFICIENT_FUNDS
	var wg sync.WaitGroup
	var successCount int64
	var insufficientFundsCount int64
	var otherFailures int64

	concurrency := 50
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := &walletv1.TransferFundsRequest{
				IdempotencyKey:      fmt.Sprintf("race-transfer-%d-%d", nowNano, idx),
				SourceWalletId:      sourceWallet,
				DestinationWalletId: destWallet,
				Amount:              &walletv1.Money{Currency: "USD", Units: 10},
			}
			resp, err := srv.TransferFunds(context.Background(), req)
			if err != nil {
				atomic.AddInt64(&otherFailures, 1)
				return
			}
			if resp.Status == walletv1.TransferFundsResponse_SUCCESS {
				atomic.AddInt64(&successCount, 1)
			} else if resp.Status == walletv1.TransferFundsResponse_FAILED_INSUFFICIENT_FUNDS {
				atomic.AddInt64(&insufficientFundsCount, 1)
			} else {
				atomic.AddInt64(&otherFailures, 1)
			}
		}(i)
	}
	wg.Wait()

	t.Logf("Concurrency results: %d successes, %d insufficient funds, %d other failures",
		successCount, insufficientFundsCount, otherFailures)

	if successCount != 10 {
		t.Fatalf("expected exactly 10 successful transfers, got %d", successCount)
	}
	if insufficientFundsCount+otherFailures != 40 {
		t.Fatalf("expected exactly 40 rejected/aborted transfers, got %d", insufficientFundsCount+otherFailures)
	}

	// Verify final balances
	balSource, err := srv.GetBalance(ctx, &walletv1.GetBalanceRequest{WalletId: sourceWallet})
	if err != nil {
		t.Fatalf("failed to fetch source balance: %v", err)
	}
	if len(balSource.Balances) == 0 || balSource.Balances[0].Units != 0 {
		t.Fatalf("expected source balance 0, got %+v", balSource.Balances)
	}

	balDest, err := srv.GetBalance(ctx, &walletv1.GetBalanceRequest{WalletId: destWallet})
	if err != nil {
		t.Fatalf("failed to fetch dest balance: %v", err)
	}
	if len(balDest.Balances) == 0 || balDest.Balances[0].Units != 100 {
		t.Fatalf("expected dest balance 100, got %+v", balDest.Balances)
	}
}

func TestConcurrentIdempotentReplayLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable for live idempotency test: %v", err)
		return
	}
	defer func() { _ = client.Disconnect(ctx) }()

	database := client.Database("test_idemp_wallet_db")
	_ = database.Drop(ctx)
	defer func() { _ = database.Drop(ctx) }()

	if err := db.EnsureWalletIndexes(ctx, database); err != nil {
		t.Fatalf("failed to ensure indexes: %v", err)
	}

	srv := &server{
		mongoClient: client,
		dbName:      "test_idemp_wallet_db",
		region:      "test-idemp-region",
		isActive:    true,
	}

	nowNano := time.Now().UnixNano()
	sourceWallet := fmt.Sprintf("charlie-idemp-%d", nowNano)
	destWallet := fmt.Sprintf("dave-idemp-%d", nowNano)

	_, _ = srv.CreateWallet(ctx, &walletv1.CreateWalletRequest{WalletId: sourceWallet, Currency: "USD", InitialBalance: 100})
	_, _ = srv.CreateWallet(ctx, &walletv1.CreateWalletRequest{WalletId: destWallet, Currency: "USD", InitialBalance: 0})

	// 20 concurrent goroutines with identical idempotency key
	idempKey := fmt.Sprintf("shared-idemp-race-key-%d", nowNano)
	var wg sync.WaitGroup
	var initialSuccess int64
	var duplicateSuccess int64
	var failures int64

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := &walletv1.TransferFundsRequest{
				IdempotencyKey:      idempKey,
				SourceWalletId:      sourceWallet,
				DestinationWalletId: destWallet,
				Amount:              &walletv1.Money{Currency: "USD", Units: 25},
			}
			resp, err := srv.TransferFunds(context.Background(), req)
			if err != nil {
				atomic.AddInt64(&failures, 1)
				return
			}
			if resp.Status == walletv1.TransferFundsResponse_SUCCESS {
				atomic.AddInt64(&initialSuccess, 1)
			} else if resp.Status == walletv1.TransferFundsResponse_REJECTED_DUPLICATE {
				atomic.AddInt64(&duplicateSuccess, 1)
			} else {
				atomic.AddInt64(&failures, 1)
			}
		}()
	}
	wg.Wait()

	t.Logf("Idempotency results: %d initial success, %d duplicate responses, %d failures",
		initialSuccess, duplicateSuccess, failures)

	if initialSuccess+duplicateSuccess != 20 {
		t.Fatalf("expected all 20 calls to succeed or return duplicate, got success=%d duplicate=%d failures=%d",
			initialSuccess, duplicateSuccess, failures)
	}

	// Balance must be debited exactly once (100 - 25 = 75)
	bal, err := srv.GetBalance(ctx, &walletv1.GetBalanceRequest{WalletId: sourceWallet})
	if err != nil {
		t.Fatalf("failed to fetch balance: %v", err)
	}
	if len(bal.Balances) == 0 || bal.Balances[0].Units != 75 {
		t.Fatalf("expected balance 75, got %+v", bal.Balances)
	}
}

func TestTransferFundsEdgeCasesLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer func() { _ = client.Disconnect(ctx) }()

	database := client.Database("test_wallet_edge_cases_db")
	_ = database.Drop(ctx)
	defer func() { _ = database.Drop(ctx) }()

	_ = db.EnsureWalletIndexes(ctx, database)

	srv := &server{
		mongoClient: client,
		dbName:      "test_wallet_edge_cases_db",
		region:      "test-edge-region",
		isActive:    true,
	}

	now := time.Now().UnixNano()
	wSrc := fmt.Sprintf("src-%d", now)
	wDst := fmt.Sprintf("dst-%d", now)
	wEUR := fmt.Sprintf("eur-%d", now)
	wFrozen := fmt.Sprintf("frz-%d", now)

	_, _ = srv.CreateWallet(ctx, &walletv1.CreateWalletRequest{WalletId: wSrc, Currency: "USD", InitialBalance: 500})
	_, _ = srv.CreateWallet(ctx, &walletv1.CreateWalletRequest{WalletId: wDst, Currency: "USD", InitialBalance: 500})
	_, _ = srv.CreateWallet(ctx, &walletv1.CreateWalletRequest{WalletId: wEUR, Currency: "EUR", InitialBalance: 500})
	_, _ = srv.CreateWallet(ctx, &walletv1.CreateWalletRequest{WalletId: wFrozen, Currency: "USD", InitialBalance: 500})
	_ = srv.UpdateWalletStatus(ctx, wFrozen, WalletStatusFrozen)

	// 1. Missing source wallet
	resp1, _ := srv.TransferFunds(ctx, &walletv1.TransferFundsRequest{
		IdempotencyKey:      fmt.Sprintf("k-missing-src-%d", now),
		SourceWalletId:      "nonexistent-src",
		DestinationWalletId: wDst,
		Amount:              &walletv1.Money{Units: 50, Currency: "USD"},
	})
	if resp1 == nil || resp1.Status != walletv1.TransferFundsResponse_FAILED_INSUFFICIENT_FUNDS {
		t.Fatalf("expected FAILED_INSUFFICIENT_FUNDS for missing source, got %+v", resp1)
	}

	// 2. Missing destination wallet
	resp2, _ := srv.TransferFunds(ctx, &walletv1.TransferFundsRequest{
		IdempotencyKey:      fmt.Sprintf("k-missing-dst-%d", now),
		SourceWalletId:      wSrc,
		DestinationWalletId: "nonexistent-dst",
		Amount:              &walletv1.Money{Units: 50, Currency: "USD"},
	})
	if resp2 == nil || resp2.Status != walletv1.TransferFundsResponse_INTERNAL_ERROR {
		t.Fatalf("expected INTERNAL_ERROR for missing destination, got %+v", resp2)
	}

	// 3. Frozen source wallet
	resp3, _ := srv.TransferFunds(ctx, &walletv1.TransferFundsRequest{
		IdempotencyKey:      fmt.Sprintf("k-frozen-src-%d", now),
		SourceWalletId:      wFrozen,
		DestinationWalletId: wDst,
		Amount:              &walletv1.Money{Units: 50, Currency: "USD"},
	})
	if resp3 == nil || resp3.Status != walletv1.TransferFundsResponse_INTERNAL_ERROR {
		t.Fatalf("expected INTERNAL_ERROR for frozen source, got %+v", resp3)
	}

	// 4. Frozen destination wallet
	resp4, _ := srv.TransferFunds(ctx, &walletv1.TransferFundsRequest{
		IdempotencyKey:      fmt.Sprintf("k-frozen-dst-%d", now),
		SourceWalletId:      wSrc,
		DestinationWalletId: wFrozen,
		Amount:              &walletv1.Money{Units: 50, Currency: "USD"},
	})
	if resp4 == nil || resp4.Status != walletv1.TransferFundsResponse_INTERNAL_ERROR {
		t.Fatalf("expected INTERNAL_ERROR for frozen destination, got %+v", resp4)
	}

	// 5. Currency mismatch on destination
	resp5, _ := srv.TransferFunds(ctx, &walletv1.TransferFundsRequest{
		IdempotencyKey:      fmt.Sprintf("k-curr-mismatch-%d", now),
		SourceWalletId:      wSrc,
		DestinationWalletId: wEUR,
		Amount:              &walletv1.Money{Units: 50, Currency: "USD"},
	})
	if resp5 == nil || resp5.Status != walletv1.TransferFundsResponse_INTERNAL_ERROR {
		t.Fatalf("expected INTERNAL_ERROR for currency mismatch, got %+v", resp5)
	}
}

func TestHandleWalletShutdownLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}

	grpcServer := grpc.NewServer()
	healthServer := health.NewServer()
	metricsServer := &http.Server{}

	handleWalletShutdown(grpcServer, healthServer, metricsServer, nil, nil, client)
}

func TestDispatchImmediateLedgerSuccessLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer func() { _ = client.Disconnect(ctx) }()

	taskCol := client.Database("banking_db").Collection("ledger_tasks")
	task := LedgerTask{
		TransactionID:       fmt.Sprintf("tx-live-success-%d", time.Now().UnixNano()),
		IdempotencyKey:      fmt.Sprintf("idemp-live-success-%d", time.Now().UnixNano()),
		SourceWalletID:      "alice",
		DestinationWalletID: "bob",
		Amount:              100,
		Currency:            "USD",
		Region:              "us-east-1",
		Status:              LedgerTaskStatusPending,
		CreatedAt:           time.Now().UTC(),
	}
	_, _ = taskCol.InsertOne(ctx, task)

	fakeSuccess := &fakeLedgerSyncClient{
		recordResp: &ledgerv1.RecordTransactionResponse{Success: true},
	}
	relay := &LedgerRelay{
		col:          taskCol,
		ledgerClient: fakeSuccess,
		logger:       observability.NewMemoryLogger(),
	}
	srv := &server{
		ledgerRelay: relay,
		logger:      observability.NewMemoryLogger(),
	}
	req := &walletv1.TransferFundsRequest{
		IdempotencyKey:      task.IdempotencyKey,
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              &walletv1.Money{Currency: "USD", Units: 100},
	}
	srv.dispatchImmediateLedger(ctx, req, task, task.TransactionID)
}
