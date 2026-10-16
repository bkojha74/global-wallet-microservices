package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wallet-system/pkg/cache"
	"wallet-system/pkg/observability"
	walletv1 "wallet-system/proto/wallet"
)

// cachedBalanceEntry stores a cloned balance response with an absolute expiration time.
type cachedBalanceEntry struct {
	response  *walletv1.GetBalanceResponse
	expiresAt time.Time
}

// BalanceCacheOption configures an optional extension for BalanceCache.
type BalanceCacheOption func(*BalanceCache)

// WithL2Cache configures a centralized L2 distributed cache backend (e.g. Redis).
func WithL2Cache(l2 cache.DistributedCache) BalanceCacheOption {
	return func(c *BalanceCache) {
		c.l2 = l2
	}
}

// WithInvalidationBus configures an AMQP invalidation bus for multi-pod cache coherence.
func WithInvalidationBus(bus cache.InvalidationBus) BalanceCacheOption {
	return func(c *BalanceCache) {
		c.bus = bus
		if bus != nil {
			err := bus.Subscribe(context.Background(), func(walletIDs []string) {
				c.InvalidateLocal(walletIDs...)
			})
			if err != nil {
				log.Printf("[WALLET-CACHE] Warning: failed to subscribe to AMQP invalidation bus: %v", err)
			}
		}
	}
}

// BalanceCache provides high-speed, thread-safe caching for wallet balance queries.
// It supports local L1 in-memory caching, pluggable L2 Redis caching, and real-time
// multi-pod invalidation broadcasting via RabbitMQ.
type BalanceCache struct {
	mu          sync.RWMutex
	entries     map[string]cachedBalanceEntry
	ttl         time.Duration
	maxEntries  int
	serviceName string
	stopCleanup chan struct{}
	closeOnce   sync.Once
	hits        uint64
	misses      uint64
	invalids    uint64

	l2  cache.DistributedCache
	bus cache.InvalidationBus
}

// NewBalanceCache initializes a BalanceCache and starts a background eviction worker.
func NewBalanceCache(ttl time.Duration, maxEntries int, serviceName string, opts ...BalanceCacheOption) *BalanceCache {
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

	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
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

	var opts []BalanceCacheOption

	backend := strings.ToLower(strings.TrimSpace(os.Getenv("WALLET_CACHE_BACKEND")))
	if backend == "" {
		backend = "hybrid"
	}

	// Check for Redis L2 configuration (only if backend is not purely memory)
	if backend != "memory" {
		if redisAddr := os.Getenv("REDIS_ADDR"); redisAddr != "" {
			redisDB := 0
			if dbVal := os.Getenv("REDIS_DB"); dbVal != "" {
				if parsedDB, err := strconv.Atoi(dbVal); err == nil {
					redisDB = parsedDB
				}
			}
			redisCache, err := cache.NewRedisCacheFromConfig(context.Background(), cache.RedisCacheConfig{
				Addr:       redisAddr,
				Password:   os.Getenv("REDIS_PASSWORD"),
				DB:         redisDB,
				DefaultTTL: ttl,
			})
			if err == nil && redisCache != nil {
				log.Printf("[WALLET-CACHE] L2 Redis cache connected at %s (backend: %s)", redisAddr, backend)
				opts = append(opts, WithL2Cache(redisCache))
			} else {
				log.Printf("[WALLET-CACHE] Warning: failed to connect to Redis at %s (%v), operating in L1-only mode", redisAddr, err)
			}
		}
	}

	// Check for AMQP Distributed Invalidation Bus configuration (fallback to LOGGING_RABBITMQ_URL)
	rabbitURL := strings.TrimSpace(os.Getenv("RABBITMQ_URL"))
	if rabbitURL == "" {
		rabbitURL = strings.TrimSpace(os.Getenv("LOGGING_RABBITMQ_URL"))
	}
	if rabbitURL != "" {
		exchange := os.Getenv("WALLET_CACHE_EXCHANGE")
		if exchange == "" {
			exchange = cache.DefaultCacheExchange
		}
		bus := cache.NewAMQPInvalidationBus(cache.InvalidationBusConfig{
			URL:        rabbitURL,
			Exchange:   exchange,
			InstanceID: observability.ResolveInstanceID(),
		})
		if err := bus.Connect(); err == nil {
			log.Printf("[WALLET-CACHE] AMQP Distributed Invalidation Bus connected on exchange '%s'", exchange)
			opts = append(opts, WithInvalidationBus(bus))
		} else {
			log.Printf("[WALLET-CACHE] Warning: failed to connect AMQP Invalidation Bus (%v), operating in local-invalidation mode", err)
		}
	}

	return NewBalanceCache(ttl, maxEntries, serviceName, opts...)
}

// Get looks up a wallet ID in the cache. Checks L1 memory first, falling back to L2 Redis on miss.
func (c *BalanceCache) Get(walletID string) (*walletv1.GetBalanceResponse, bool) {
	if c == nil || walletID == "" {
		return nil, false
	}

	// 1. Check L1 in-memory cache
	c.mu.RLock()
	entry, found := c.entries[walletID]
	if found {
		now := time.Now()
		if !now.After(entry.expiresAt) {
			cloned := cloneGetBalanceResponse(entry.response)
			c.mu.RUnlock()
			atomic.AddUint64(&c.hits, 1)
			observability.DefaultMetrics.IncCacheHits(c.serviceName, "wallet_balance")
			return cloned, true
		}
		c.mu.RUnlock()
		// Expired: prune lazily
		c.mu.Lock()
		if e, exists := c.entries[walletID]; exists && time.Now().After(e.expiresAt) {
			delete(c.entries, walletID)
		}
		c.mu.Unlock()
	} else {
		c.mu.RUnlock()
	}

	// 2. Check L2 Redis cache if configured
	if c.l2 != nil {
		if resp, hit, err := c.l2.Get(context.Background(), walletID); err == nil && hit && resp != nil {
			// Backfill L1
			c.setLocal(walletID, resp)
			atomic.AddUint64(&c.hits, 1)
			observability.DefaultMetrics.IncCacheHits(c.serviceName, "wallet_balance")
			return resp, true
		}
	}

	atomic.AddUint64(&c.misses, 1)
	observability.DefaultMetrics.IncCacheMisses(c.serviceName, "wallet_balance")
	return nil, false
}

// Set stores a balance response in the local cache and L2 Redis (if configured).
func (c *BalanceCache) Set(walletID string, resp *walletv1.GetBalanceResponse) {
	if c == nil || walletID == "" || resp == nil {
		return
	}

	c.setLocal(walletID, resp)

	if c.l2 != nil {
		if err := c.l2.Set(context.Background(), walletID, resp, c.ttl); err != nil {
			log.Printf("[WALLET-CACHE] Warning: failed to write to L2 cache for wallet %s: %v", walletID, err)
		}
	}
}

func (c *BalanceCache) setLocal(walletID string, resp *walletv1.GetBalanceResponse) {
	cloned := cloneGetBalanceResponse(resp)
	expiresAt := time.Now().Add(c.ttl)

	c.mu.Lock()
	defer c.mu.Unlock()

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

// Invalidate removes wallet IDs from local L1, L2 Redis, and broadcasts across the AMQP invalidation bus.
func (c *BalanceCache) Invalidate(walletIDs ...string) {
	if c == nil || len(walletIDs) == 0 {
		return
	}

	c.InvalidateLocal(walletIDs...)

	if c.l2 != nil {
		if err := c.l2.Invalidate(context.Background(), walletIDs...); err != nil {
			log.Printf("[WALLET-CACHE] Warning: failed to invalidate L2 cache for %v: %v", walletIDs, err)
		}
	}

	if c.bus != nil {
		if err := c.bus.PublishInvalidation(context.Background(), walletIDs...); err != nil {
			log.Printf("[WALLET-CACHE] Warning: failed to broadcast cache invalidation event: %v", err)
		}
	}
}

// InvalidateLocal removes wallet IDs from the local in-memory cache only (used by invalidation subscriber).
func (c *BalanceCache) InvalidateLocal(walletIDs ...string) {
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
	c.mu.Unlock()

	if evictedCount > 0 {
		atomic.AddUint64(&c.invalids, evictedCount)
		observability.DefaultMetrics.IncCacheInvalidations(c.serviceName, "wallet_balance")
	}
}

// Clear flushes all entries from the local cache.
func (c *BalanceCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cachedBalanceEntry)
}

// Close stops the background eviction goroutine and closes L2 and bus connections.
func (c *BalanceCache) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		close(c.stopCleanup)
		if c.bus != nil {
			_ = c.bus.Close()
		}
		if c.l2 != nil {
			_ = c.l2.Close()
		}
	})
}

// Stats returns cumulative hits, misses, invalidations, and current entry count.
func (c *BalanceCache) Stats() (hits, misses, invalids uint64, count int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.RLock()
	count = len(c.entries)
	c.mu.RUnlock()
	return atomic.LoadUint64(&c.hits), atomic.LoadUint64(&c.misses), atomic.LoadUint64(&c.invalids), count
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

func cloneGetBalanceResponse(in *walletv1.GetBalanceResponse) *walletv1.GetBalanceResponse {
	return cache.CloneGetBalanceResponse(in)
}
