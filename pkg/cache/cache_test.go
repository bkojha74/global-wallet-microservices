package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	walletv1 "wallet-system/proto/wallet"
)

type mockInvalidationBus struct {
	mu          sync.Mutex
	published   [][]string
	subscribers []func(walletIDs []string)
	closeCalled bool
}

func (m *mockInvalidationBus) PublishInvalidation(_ context.Context, walletIDs ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published = append(m.published, walletIDs)
	return nil
}

func (m *mockInvalidationBus) Subscribe(_ context.Context, onInvalidate func(walletIDs []string)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subscribers = append(m.subscribers, onInvalidate)
	return nil
}

func (m *mockInvalidationBus) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalled = true
	return nil
}

func (m *mockInvalidationBus) simulateBroadcast(walletIDs ...string) {
	m.mu.Lock()
	subs := make([]func(walletIDs []string), len(m.subscribers))
	copy(subs, m.subscribers)
	m.mu.Unlock()

	for _, s := range subs {
		s(walletIDs)
	}
}

func TestMemoryCache_BasicOperations(t *testing.T) {
	c := NewMemoryCache(5*time.Second, 100)
	defer c.Close()

	ctx := context.Background()

	// Initial miss
	if _, hit, _ := c.Get(ctx, "w1"); hit {
		t.Fatal("expected miss for w1")
	}

	resp := &walletv1.GetBalanceResponse{
		WalletId: "w1",
		Balances: []*walletv1.Money{
			{Currency: "USD", Units: 500},
		},
		Status: "ACTIVE",
	}

	// Set and hit
	if err := c.Set(ctx, "w1", resp, 5*time.Second); err != nil {
		t.Fatalf("unexpected set error: %v", err)
	}

	got, hit, err := c.Get(ctx, "w1")
	if err != nil || !hit || got == nil {
		t.Fatalf("expected hit for w1, got hit=%v err=%v", hit, err)
	}
	if got.WalletId != "w1" || len(got.Balances) != 1 || got.Balances[0].Units != 500 {
		t.Fatalf("unexpected cached payload: %+v", got)
	}

	// Verify deep copy mutation resistance
	got.Balances[0].Units = 999
	gotAgain, _, _ := c.Get(ctx, "w1")
	if gotAgain.Balances[0].Units != 500 {
		t.Fatalf("expected cached value to remain 500, got %d", gotAgain.Balances[0].Units)
	}

	// Invalidate
	if err := c.Invalidate(ctx, "w1"); err != nil {
		t.Fatalf("unexpected invalidate error: %v", err)
	}

	if _, hit, _ := c.Get(ctx, "w1"); hit {
		t.Fatal("expected miss after invalidate")
	}

	hits, misses, invalids, _ := c.Stats()
	if hits < 2 || misses < 2 || invalids < 1 {
		t.Fatalf("unexpected stats: hits=%d misses=%d invalids=%d", hits, misses, invalids)
	}
}

func TestMemoryCache_Expiration(t *testing.T) {
	c := NewMemoryCache(50*time.Millisecond, 100)
	defer c.Close()

	ctx := context.Background()
	resp := &walletv1.GetBalanceResponse{
		WalletId: "w_expire",
		Balances: []*walletv1.Money{{Currency: "EUR", Units: 100}},
	}

	_ = c.Set(ctx, "w_expire", resp, 50*time.Millisecond)
	if _, hit, _ := c.Get(ctx, "w_expire"); !hit {
		t.Fatal("expected immediate hit")
	}

	time.Sleep(75 * time.Millisecond)

	if _, hit, _ := c.Get(ctx, "w_expire"); hit {
		t.Fatal("expected entry to be expired")
	}
}

func TestMemoryCache_EvictionAtCapacity(t *testing.T) {
	c := NewMemoryCache(1*time.Minute, 2)
	defer c.Close()

	ctx := context.Background()
	_ = c.Set(ctx, "w1", &walletv1.GetBalanceResponse{WalletId: "w1"}, 1*time.Minute)
	_ = c.Set(ctx, "w2", &walletv1.GetBalanceResponse{WalletId: "w2"}, 1*time.Minute)

	// Inserting 3rd must evict 1 entry to maintain cap
	_ = c.Set(ctx, "w3", &walletv1.GetBalanceResponse{WalletId: "w3"}, 1*time.Minute)

	_, _, _, count := c.Stats()
	if count > 2 {
		t.Fatalf("expected count <= 2, got %d", count)
	}
}

func TestTieredCache_L1L2FallbackAndBusInvalidation(t *testing.T) {
	ctx := context.Background()

	l1 := NewMemoryCache(5*time.Second, 100)
	defer l1.Close()

	l2 := NewMemoryCache(5*time.Second, 100)
	defer l2.Close()

	bus := &mockInvalidationBus{}

	tc := NewTieredCache(ctx, TieredCacheConfig{
		L1:  l1,
		L2:  l2,
		Bus: bus,
	})
	defer tc.Close()

	resp := &walletv1.GetBalanceResponse{
		WalletId: "user-123",
		Balances: []*walletv1.Money{{Currency: "USD", Units: 1250}},
	}

	// 1. Simulate balance populated in L2 only (e.g. from another pod)
	_ = l2.Set(ctx, "user-123", resp, 5*time.Second)

	// 2. Query TieredCache: should miss L1, hit L2, and backfill L1
	got, hit, err := tc.Get(ctx, "user-123")
	if err != nil || !hit || got == nil {
		t.Fatalf("expected hit via L2 fallback, got hit=%v err=%v", hit, err)
	}

	// 3. Subsequent query should hit L1 directly
	l1Resp, l1Hit, _ := l1.Get(ctx, "user-123")
	if !l1Hit || l1Resp == nil || l1Resp.Balances[0].Units != 1250 {
		t.Fatal("expected L1 to have been backfilled from L2")
	}

	// 4. Test Invalidate call on TieredCache
	_ = tc.Invalidate(ctx, "user-123")

	// Verify L1 evicted
	if _, hit, _ = l1.Get(ctx, "user-123"); hit {
		t.Fatal("expected L1 to be evicted")
	}
	// Verify L2 evicted
	if _, hit, _ = l2.Get(ctx, "user-123"); hit {
		t.Fatal("expected L2 to be evicted")
	}
	// Verify broadcast sent over bus
	if len(bus.published) != 1 || len(bus.published[0]) != 1 || bus.published[0][0] != "user-123" {
		t.Fatalf("expected bus invalidation published for user-123, got: %v", bus.published)
	}

	// 5. Test incoming bus event from another pod evicting local L1
	_ = l1.Set(ctx, "peer-wallet", resp, 5*time.Second)
	if _, hit, _ := l1.Get(ctx, "peer-wallet"); !hit {
		t.Fatal("expected peer-wallet in L1")
	}

	bus.simulateBroadcast("peer-wallet")

	if _, hit, _ := l1.Get(ctx, "peer-wallet"); hit {
		t.Fatal("expected peer-wallet to be evicted by incoming bus broadcast")
	}
}

func TestRedisCache_NilSafe(t *testing.T) {
	var rc *RedisCache
	ctx := context.Background()

	if _, hit, err := rc.Get(ctx, "w1"); hit || err != nil {
		t.Fatalf("expected nil-safe Get to return false, nil; got hit=%v err=%v", hit, err)
	}
	if err := rc.Set(ctx, "w1", nil, time.Second); err != nil {
		t.Fatalf("expected nil-safe Set to return nil, got %v", err)
	}
	if err := rc.Invalidate(ctx, "w1"); err != nil {
		t.Fatalf("expected nil-safe Invalidate to return nil, got %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("expected nil-safe Close to return nil, got %v", err)
	}
}

func TestAMQPInvalidationBus_NilSafe(t *testing.T) {
	var bus *AMQPInvalidationBus
	ctx := context.Background()

	if err := bus.PublishInvalidation(ctx, "w1"); err != nil {
		t.Fatalf("expected nil bus publish to return nil, got %v", err)
	}
	if err := bus.Subscribe(ctx, nil); err == nil {
		t.Fatal("expected error on nil subscriber")
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("expected nil bus close to return nil, got %v", err)
	}
}
