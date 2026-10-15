package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	walletv1 "wallet-system/proto/wallet"
)

// DistributedCache defines the contract for pluggable L1/L2 wallet balance caching.
type DistributedCache interface {
	Get(ctx context.Context, walletID string) (*walletv1.GetBalanceResponse, bool, error)
	Set(ctx context.Context, walletID string, resp *walletv1.GetBalanceResponse, ttl time.Duration) error
	Invalidate(ctx context.Context, walletIDs ...string) error
	Close() error
}

// memoryCacheEntry stores a cloned balance response with an absolute expiration time.
type memoryCacheEntry struct {
	response  *walletv1.GetBalanceResponse
	expiresAt time.Time
}

// MemoryCache provides a fast, thread-safe in-memory cache implementation of DistributedCache.
type MemoryCache struct {
	mu          sync.RWMutex
	entries     map[string]memoryCacheEntry
	defaultTTL  time.Duration
	maxEntries  int
	stopCleanup chan struct{}
	closeOnce   sync.Once
	hits        uint64
	misses      uint64
	invalids    uint64
}

// NewMemoryCache initializes an in-memory cache with background eviction.
func NewMemoryCache(defaultTTL time.Duration, maxEntries int) *MemoryCache {
	if defaultTTL <= 0 {
		defaultTTL = 15 * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = 10000
	}

	c := &MemoryCache{
		entries:     make(map[string]memoryCacheEntry),
		defaultTTL:  defaultTTL,
		maxEntries:  maxEntries,
		stopCleanup: make(chan struct{}),
	}

	go c.runBackgroundCleanup(30 * time.Second)
	return c
}

// Get retrieves a wallet balance from in-memory cache.
func (c *MemoryCache) Get(_ context.Context, walletID string) (*walletv1.GetBalanceResponse, bool, error) {
	if c == nil || walletID == "" {
		return nil, false, nil
	}

	c.mu.RLock()
	entry, found := c.entries[walletID]
	if !found {
		c.mu.RUnlock()
		atomic.AddUint64(&c.misses, 1)
		return nil, false, nil
	}

	now := time.Now()
	if now.After(entry.expiresAt) {
		c.mu.RUnlock()
		c.mu.Lock()
		if e, exists := c.entries[walletID]; exists && time.Now().After(e.expiresAt) {
			delete(c.entries, walletID)
		}
		c.mu.Unlock()
		atomic.AddUint64(&c.misses, 1)
		return nil, false, nil
	}

	cloned := CloneGetBalanceResponse(entry.response)
	c.mu.RUnlock()

	atomic.AddUint64(&c.hits, 1)
	return cloned, true, nil
}

// Set stores a balance response in the in-memory cache.
func (c *MemoryCache) Set(_ context.Context, walletID string, resp *walletv1.GetBalanceResponse, ttl time.Duration) error {
	if c == nil || walletID == "" || resp == nil {
		return nil
	}

	if ttl <= 0 {
		ttl = c.defaultTTL
	}

	cloned := CloneGetBalanceResponse(resp)
	expiresAt := time.Now().Add(ttl)

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= c.maxEntries {
		if _, exists := c.entries[walletID]; !exists {
			c.evictOneLocked()
		}
	}

	c.entries[walletID] = memoryCacheEntry{
		response:  cloned,
		expiresAt: expiresAt,
	}
	return nil
}

// Invalidate removes one or more wallet IDs from the in-memory cache.
func (c *MemoryCache) Invalidate(_ context.Context, walletIDs ...string) error {
	if c == nil || len(walletIDs) == 0 {
		return nil
	}

	c.mu.Lock()
	var count uint64
	for _, id := range walletIDs {
		if id == "" {
			continue
		}
		if _, exists := c.entries[id]; exists {
			delete(c.entries, id)
			count++
		}
	}
	c.mu.Unlock()

	if count > 0 {
		atomic.AddUint64(&c.invalids, count)
	}
	return nil
}

// Close stops the background cleanup worker.
func (c *MemoryCache) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		close(c.stopCleanup)
	})
	return nil
}

// Stats returns hit, miss, and invalidation counters along with total count.
func (c *MemoryCache) Stats() (hits, misses, invalids uint64, count int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.RLock()
	count = len(c.entries)
	c.mu.RUnlock()
	return atomic.LoadUint64(&c.hits), atomic.LoadUint64(&c.misses), atomic.LoadUint64(&c.invalids), count
}

func (c *MemoryCache) runBackgroundCleanup(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCleanup:
			return
		case <-ticker.C:
			c.mu.Lock()
			now := time.Now()
			for k, v := range c.entries {
				if now.After(v.expiresAt) {
					delete(c.entries, k)
				}
			}
			c.mu.Unlock()
		}
	}
}

func (c *MemoryCache) evictOneLocked() {
	now := time.Now()
	for k, v := range c.entries {
		if now.After(v.expiresAt) {
			delete(c.entries, k)
			return
		}
	}
	for k := range c.entries {
		delete(c.entries, k)
		return
	}
}

// CloneGetBalanceResponse performs a deep copy of a protobuf GetBalanceResponse.
func CloneGetBalanceResponse(in *walletv1.GetBalanceResponse) *walletv1.GetBalanceResponse {
	if in == nil {
		return nil
	}
	out := &walletv1.GetBalanceResponse{
		WalletId:        in.WalletId,
		HandledByRegion: in.HandledByRegion,
		Status:          in.Status,
		Balances:        make([]*walletv1.Money, len(in.Balances)),
	}
	for i, m := range in.Balances {
		if m != nil {
			out.Balances[i] = &walletv1.Money{
				Currency: m.Currency,
				Units:    m.Units,
			}
		}
	}
	return out
}
