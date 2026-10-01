package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"wallet-system/pkg/observability"
	walletv1 "wallet-system/proto/wallet"
)

func TestBalanceCache_HitAndMiss(t *testing.T) {
	cache := NewBalanceCache(5*time.Second, 100, "test-wallet")
	defer cache.Close()

	// 1. Initial lookup should be a cache miss
	resp, hit := cache.Get("wallet_123")
	if hit || resp != nil {
		t.Fatalf("expected cache miss, got hit=%v, resp=%v", hit, resp)
	}

	// 2. Set balance
	orig := &walletv1.GetBalanceResponse{
		WalletId: "wallet_123",
		Balances: []*walletv1.Money{
			{Currency: "USD", Units: 10000},
		},
		Status:          "ACTIVE",
		HandledByRegion: "us-east-1",
	}
	cache.Set("wallet_123", orig)

	// 3. Subsequent lookup should be a cache hit
	cached, hit := cache.Get("wallet_123")
	if !hit || cached == nil {
		t.Fatalf("expected cache hit, got hit=%v, cached=%v", hit, cached)
	}
	if cached.WalletId != "wallet_123" || cached.Balances[0].Units != 10000 {
		t.Errorf("unexpected cached balance data: %+v", cached)
	}

	// 4. Verify cloned copy independence (mutating retrieved object does not mutate cache)
	cached.Balances[0].Units = 99999
	recheck, hit := cache.Get("wallet_123")
	if !hit || recheck.Balances[0].Units != 10000 {
		t.Errorf("cache pollution detected! Expected 10000, got %d", recheck.Balances[0].Units)
	}
}

func TestBalanceCache_Expiration(t *testing.T) {
	cache := NewBalanceCache(50*time.Millisecond, 100, "test-wallet")
	defer cache.Close()

	cache.Set("wallet_exp", &walletv1.GetBalanceResponse{
		WalletId: "wallet_exp",
		Balances: []*walletv1.Money{{Currency: "EUR", Units: 500}},
	})

	// Immediate lookup hits
	if _, hit := cache.Get("wallet_exp"); !hit {
		t.Fatal("expected immediate lookup to hit")
	}

	// Wait past TTL
	time.Sleep(100 * time.Millisecond)

	// Lookup should now miss and lazily prune
	if _, hit := cache.Get("wallet_exp"); hit {
		t.Fatal("expected expired key to miss")
	}
}

func TestBalanceCache_Invalidation(t *testing.T) {
	cache := NewBalanceCache(10*time.Second, 100, "test-wallet")
	defer cache.Close()

	cache.Set("alice", &walletv1.GetBalanceResponse{WalletId: "alice"})
	cache.Set("bob", &walletv1.GetBalanceResponse{WalletId: "bob"})
	cache.Set("charlie", &walletv1.GetBalanceResponse{WalletId: "charlie"})

	// Invalidate alice and bob
	cache.Invalidate("alice", "bob")

	if _, hit := cache.Get("alice"); hit {
		t.Error("expected alice to be invalidated")
	}
	if _, hit := cache.Get("bob"); hit {
		t.Error("expected bob to be invalidated")
	}
	if _, hit := cache.Get("charlie"); !hit {
		t.Error("expected charlie to remain in cache")
	}
}

func TestBalanceCache_MaxEntriesEviction(t *testing.T) {
	max := 5
	cache := NewBalanceCache(10*time.Second, max, "test-wallet")
	defer cache.Close()

	for i := 0; i < 10; i++ {
		cache.Set(fmt.Sprintf("w_%d", i), &walletv1.GetBalanceResponse{WalletId: fmt.Sprintf("w_%d", i)})
	}

	_, _, _, count := cache.Stats()
	if count > max {
		t.Errorf("expected count <= %d, got %d", max, count)
	}
}

func TestBalanceCache_ThreadSafety(t *testing.T) {
	cache := NewBalanceCache(1*time.Second, 100, "test-wallet")
	defer cache.Close()

	var wg sync.WaitGroup
	workers := 20
	iterations := 100

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			wID := fmt.Sprintf("wallet_%d", workerID%5)
			for j := 0; j < iterations; j++ {
				cache.Set(wID, &walletv1.GetBalanceResponse{
					WalletId: wID,
					Balances: []*walletv1.Money{{Currency: "USD", Units: int64(j)}},
				})
				cache.Get(wID)
				if j%10 == 0 {
					cache.Invalidate(wID)
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestServer_GetBalance_CacheReadThrough(t *testing.T) {
	cache := NewBalanceCache(10*time.Second, 50, "test-wallet")
	defer cache.Close()

	srv := &server{
		region:       "us-east-1",
		balanceCache: cache,
		logger:       observability.NewMemoryLogger(),
	}

	// 1. Manually prime cache
	cache.Set("cached_wallet", &walletv1.GetBalanceResponse{
		WalletId:        "cached_wallet",
		Balances:        []*walletv1.Money{{Currency: "USD", Units: 4200}},
		Status:          "ACTIVE",
		HandledByRegion: "us-east-1",
	})

	// 2. Query GetBalance — should return directly from cache without hitting DB
	resp, err := srv.GetBalance(context.Background(), &walletv1.GetBalanceRequest{WalletId: "cached_wallet"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Balances[0].Units != 4200 {
		t.Errorf("expected 4200 from cache, got %d", resp.Balances[0].Units)
	}

	hits, _, _, _ := cache.Stats()
	if hits != 1 {
		t.Errorf("expected 1 cache hit, got %d", hits)
	}
}

func TestServer_TransferSuccess_InvalidatesCache(t *testing.T) {
	cache := NewBalanceCache(10*time.Second, 50, "test-wallet")
	defer cache.Close()

	srv := &server{
		region:       "us-east-1",
		balanceCache: cache,
		logger:       observability.NewMemoryLogger(),
	}

	cache.Set("alice_w", &walletv1.GetBalanceResponse{WalletId: "alice_w", Balances: []*walletv1.Money{{Currency: "USD", Units: 100}}})
	cache.Set("bob_w", &walletv1.GetBalanceResponse{WalletId: "bob_w", Balances: []*walletv1.Money{{Currency: "USD", Units: 50}}})

	// Simulate successful transfer completion
	srv.handleTransferSuccess(context.Background(), &walletv1.TransferFundsRequest{
		SourceWalletId:      "alice_w",
		DestinationWalletId: "bob_w",
		Amount:              &walletv1.Money{Currency: "USD", Units: 20},
	}, &transferExecutionState{
		txnStatus: walletv1.TransferFundsResponse_SUCCESS,
	}, 15)

	// Both wallets should now be evicted from cache
	if _, hit := cache.Get("alice_w"); hit {
		t.Error("expected alice_w to be evicted from cache after transfer")
	}
	if _, hit := cache.Get("bob_w"); hit {
		t.Error("expected bob_w to be evicted from cache after transfer")
	}

	_, _, invalids, _ := cache.Stats()
	if invalids != 2 {
		t.Errorf("expected 2 invalidations, got %d", invalids)
	}
}
