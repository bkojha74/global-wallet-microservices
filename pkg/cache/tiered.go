package cache

import (
	"context"
	"log"
	"time"

	walletv1 "wallet-system/proto/wallet"
)

// TieredCache combines a sub-millisecond local L1 MemoryCache, an optional centralized L2 RedisCache,
// and an optional AMQP InvalidationBus to guarantee multi-pod cache coherence.
type TieredCache struct {
	l1         *MemoryCache
	l2         DistributedCache
	bus        InvalidationBus
	defaultTTL time.Duration
}

// TieredCacheConfig holds dependencies for creating a TieredCache.
type TieredCacheConfig struct {
	L1         *MemoryCache
	L2         DistributedCache
	Bus        InvalidationBus
	DefaultTTL time.Duration
}

// NewTieredCache constructs a new TieredCache and hooks up the invalidation bus subscriber.
func NewTieredCache(ctx context.Context, cfg TieredCacheConfig) *TieredCache {
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = 15 * time.Second
	}
	if cfg.L1 == nil {
		cfg.L1 = NewMemoryCache(cfg.DefaultTTL, 10000)
	}

	tc := &TieredCache{
		l1:         cfg.L1,
		l2:         cfg.L2,
		bus:        cfg.Bus,
		defaultTTL: cfg.DefaultTTL,
	}

	if tc.bus != nil {
		err := tc.bus.Subscribe(ctx, func(walletIDs []string) {
			if len(walletIDs) > 0 {
				log.Printf("[TIERED-CACHE] Invalidation bus event received: evicting %v from L1", walletIDs)
				_ = tc.l1.Invalidate(context.Background(), walletIDs...)
			}
		})
		if err != nil {
			log.Printf("[TIERED-CACHE] Warning: failed to subscribe to invalidation bus: %v", err)
		}
	}

	return tc
}

// Get queries L1 memory first, falling back to L2 Redis on miss and back-filling L1 on hit.
func (t *TieredCache) Get(ctx context.Context, walletID string) (*walletv1.GetBalanceResponse, bool, error) {
	if t == nil || walletID == "" {
		return nil, false, nil
	}

	// 1. Check ultra-fast L1 In-Memory Cache
	if resp, hit, err := t.l1.Get(ctx, walletID); err == nil && hit && resp != nil {
		return resp, true, nil
	}

	// 2. If L2 is configured, check centralized Redis/Dragonfly
	if t.l2 != nil {
		resp, hit, err := t.l2.Get(ctx, walletID)
		if err == nil && hit && resp != nil {
			// Backfill L1 so subsequent requests are served from local memory
			_ = t.l1.Set(ctx, walletID, resp, t.defaultTTL)
			return resp, true, nil
		}
	}

	return nil, false, nil
}

// Set populates L1 and L2 with the given TTL.
func (t *TieredCache) Set(ctx context.Context, walletID string, resp *walletv1.GetBalanceResponse, ttl time.Duration) error {
	if t == nil || walletID == "" || resp == nil {
		return nil
	}

	if ttl <= 0 {
		ttl = t.defaultTTL
	}

	_ = t.l1.Set(ctx, walletID, resp, ttl)

	if t.l2 != nil {
		if err := t.l2.Set(ctx, walletID, resp, ttl); err != nil {
			log.Printf("[TIERED-CACHE] Warning: failed to write to L2 cache for wallet %s: %v", walletID, err)
		}
	}
	return nil
}

// Invalidate clears the keys in L1, L2, and broadcasts an invalidation event across the AMQP bus.
func (t *TieredCache) Invalidate(ctx context.Context, walletIDs ...string) error {
	if t == nil || len(walletIDs) == 0 {
		return nil
	}

	_ = t.l1.Invalidate(ctx, walletIDs...)

	if t.l2 != nil {
		if err := t.l2.Invalidate(ctx, walletIDs...); err != nil {
			log.Printf("[TIERED-CACHE] Warning: failed to invalidate L2 cache for %v: %v", walletIDs, err)
		}
	}

	if t.bus != nil {
		if err := t.bus.PublishInvalidation(ctx, walletIDs...); err != nil {
			log.Printf("[TIERED-CACHE] Warning: failed to broadcast invalidation across AMQP bus: %v", err)
		}
	}
	return nil
}

// Close terminates L1, L2, and the AMQP invalidation bus.
func (t *TieredCache) Close() error {
	if t == nil {
		return nil
	}

	var err error
	if t.bus != nil {
		if bErr := t.bus.Close(); bErr != nil {
			err = bErr
		}
	}
	if t.l2 != nil {
		if l2Err := t.l2.Close(); l2Err != nil && err == nil {
			err = l2Err
		}
	}
	if t.l1 != nil {
		if l1Err := t.l1.Close(); l1Err != nil && err == nil {
			err = l1Err
		}
	}
	return err
}

// Stats returns the L1 cache statistics.
func (t *TieredCache) Stats() (hits, misses, invalids uint64, count int) {
	if t == nil || t.l1 == nil {
		return 0, 0, 0, 0
	}
	return t.l1.Stats()
}
