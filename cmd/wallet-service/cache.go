package main

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"wallet-system/pkg/observability"
	walletv1 "wallet-system/proto/wallet"
)

// cachedBalanceEntry stores a cloned balance response with an absolute expiration time.
type cachedBalanceEntry struct {
	response  *walletv1.GetBalanceResponse
	expiresAt time.Time
}

// BalanceCache provides high-speed, thread-safe in-memory caching for wallet balance queries.
// It follows the Cache-Aside pattern with strict immediate write-invalidation during
// transfers and status changes, ensuring sub-millisecond reads while preventing stale balances.
type BalanceCache struct {
	mu          sync.RWMutex
	entries     map[string]cachedBalanceEntry
	ttl         time.Duration
	maxEntries  int
	serviceName string
	stopCleanup chan struct{}
	hits        uint64
	misses      uint64
	invalids    uint64
}

// NewBalanceCache initializes an in-memory BalanceCache and starts a background eviction worker.
func NewBalanceCache(ttl time.Duration, maxEntries int, serviceName string) *BalanceCache {
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	if serviceName == "" {
		serviceName = "wallet-service"
	}

	c := &BalanceCache{
		entries:     make(map[string]cachedBalanceEntry),
		ttl:         ttl,
		maxEntries:  maxEntries,
		serviceName: serviceName,
		stopCleanup: make(chan struct{}),
	}

	// Start background cleanup ticker to evict expired entries periodically
	go c.runBackgroundCleanup(30 * time.Second)

	return c
}

// NewBalanceCacheFromEnv constructs a BalanceCache reading configuration from environment variables.
func NewBalanceCacheFromEnv(serviceName string) *BalanceCache {
	if strings.ToLower(os.Getenv("WALLET_CACHE_ENABLED")) == "false" {
		return nil
	}

	ttl := 15 * time.Second
	if val := os.Getenv("WALLET_CACHE_TTL_SECONDS"); val != "" {
		if sec, err := strconv.Atoi(val); err == nil && sec > 0 {
			ttl = time.Duration(sec) * time.Second
		}
	}

	maxEntries := 10000
	if val := os.Getenv("WALLET_CACHE_MAX_ENTRIES"); val != "" {
		if m, err := strconv.Atoi(val); err == nil && m > 0 {
			maxEntries = m
		}
	}

	return NewBalanceCache(ttl, maxEntries, serviceName)
}

// Get looks up a wallet ID in the cache. Returns a clone of the cached response and true on hit.
func (c *BalanceCache) Get(walletID string) (*walletv1.GetBalanceResponse, bool) {
	if c == nil || walletID == "" {
		return nil, false
	}

	c.mu.RLock()
	entry, found := c.entries[walletID]
	if !found {
		c.mu.RUnlock()
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		observability.DefaultMetrics.IncCacheMisses(c.serviceName, "wallet_balance")
		return nil, false
	}

	now := time.Now()
	if now.After(entry.expiresAt) {
		c.mu.RUnlock()
		// Expired: prune lazily
		c.mu.Lock()
		delete(c.entries, walletID)
		c.misses++
		c.mu.Unlock()
		observability.DefaultMetrics.IncCacheMisses(c.serviceName, "wallet_balance")
		return nil, false
	}

	cloned := cloneGetBalanceResponse(entry.response)
	c.hits++
	c.mu.RUnlock()

	observability.DefaultMetrics.IncCacheHits(c.serviceName, "wallet_balance")
	return cloned, true
}

// Set stores a balance response in the cache with the configured TTL.
func (c *BalanceCache) Set(walletID string, resp *walletv1.GetBalanceResponse) {
	if c == nil || walletID == "" || resp == nil {
		return
	}

	cloned := cloneGetBalanceResponse(resp)
	expiresAt := time.Now().Add(c.ttl)

	c.mu.Lock()
	defer c.mu.Unlock()

	// If at capacity and key is new, evict expired or an arbitrary entry
	if len(c.entries) >= c.maxEntries {
		if _, exists := c.entries[walletID]; !exists {
			c.evictOneLocked()
		}
	}

	c.entries[walletID] = cachedBalanceEntry{
		response:  cloned,
		expiresAt: expiresAt,
	}
}

// Invalidate removes one or more wallet IDs from the cache immediately.
func (c *BalanceCache) Invalidate(walletIDs ...string) {
	if c == nil || len(walletIDs) == 0 {
		return
	}

	c.mu.Lock()
	var evictedCount uint64
	for _, id := range walletIDs {
		if id == "" {
			continue
		}
		if _, exists := c.entries[id]; exists {
			delete(c.entries, id)
			evictedCount++
		}
	}
	c.invalids += evictedCount
	c.mu.Unlock()

	if evictedCount > 0 {
		observability.DefaultMetrics.IncCacheInvalidations(c.serviceName, "wallet_balance")
	}
}

// Clear flushes all entries from the cache.
func (c *BalanceCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cachedBalanceEntry)
}

// Close stops the background eviction goroutine.
func (c *BalanceCache) Close() {
	if c == nil {
		return
	}
	select {
	case <-c.stopCleanup:
		// already closed
	default:
		close(c.stopCleanup)
	}
}

// Stats returns cumulative hits, misses, invalidations, and current entry count.
func (c *BalanceCache) Stats() (hits, misses, invalids uint64, count int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hits, c.misses, c.invalids, len(c.entries)
}

func (c *BalanceCache) runBackgroundCleanup(interval time.Duration) {
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

func (c *BalanceCache) evictOneLocked() {
	now := time.Now()
	// Priority 1: Evict an already expired entry
	for k, v := range c.entries {
		if now.After(v.expiresAt) {
			delete(c.entries, k)
			return
		}
	}
	// Priority 2: Evict first entry encountered
	for k := range c.entries {
		delete(c.entries, k)
		return
	}
}

func cloneGetBalanceResponse(in *walletv1.GetBalanceResponse) *walletv1.GetBalanceResponse {
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
