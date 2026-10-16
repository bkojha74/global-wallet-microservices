package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	walletv1 "wallet-system/proto/wallet"
)

const (
	DefaultRedisKeyPrefix = "wallet:balance:"
)

// RedisCacheConfig holds connection and behavior settings for RedisCache.
type RedisCacheConfig struct {
	Addr       string
	Password   string
	DB         int
	KeyPrefix  string
	DefaultTTL time.Duration
}

// RedisCache implements DistributedCache backed by Redis or Dragonfly.
type RedisCache struct {
	client     *redis.Client
	keyPrefix  string
	defaultTTL time.Duration
}

// NewRedisCache creates a new RedisCache instance.
func NewRedisCache(client *redis.Client, keyPrefix string, defaultTTL time.Duration) *RedisCache {
	if keyPrefix == "" {
		keyPrefix = DefaultRedisKeyPrefix
	}
	if defaultTTL <= 0 {
		defaultTTL = 15 * time.Second
	}
	return &RedisCache{
		client:     client,
		keyPrefix:  keyPrefix,
		defaultTTL: defaultTTL,
	}
}

// NewRedisCacheFromConfig initializes a go-redis client and creates a RedisCache.
func NewRedisCacheFromConfig(ctx context.Context, cfg RedisCacheConfig) (*RedisCache, error) {
	if cfg.Addr == "" {
		return nil, errors.New("redis address is required")
	}

	opts := &redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	}

	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("failed to ping redis at %s: %w", cfg.Addr, err)
	}

	return NewRedisCache(client, cfg.KeyPrefix, cfg.DefaultTTL), nil
}

// Get retrieves and unmarshals a cached balance from Redis.
func (r *RedisCache) Get(ctx context.Context, walletID string) (*walletv1.GetBalanceResponse, bool, error) {
	if r == nil || r.client == nil || walletID == "" {
		return nil, false, nil
	}

	key := r.formatKey(walletID)
	data, err := r.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("redis get error: %w", err)
	}

	resp := &walletv1.GetBalanceResponse{}
	if err := proto.Unmarshal(data, resp); err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal cached balance proto: %w", err)
	}

	return resp, true, nil
}

// Set marshals and stores a balance response in Redis with the specified TTL.
func (r *RedisCache) Set(ctx context.Context, walletID string, resp *walletv1.GetBalanceResponse, ttl time.Duration) error {
	if r == nil || r.client == nil || walletID == "" || resp == nil {
		return nil
	}

	if ttl <= 0 {
		ttl = r.defaultTTL
	}

	data, err := proto.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal balance proto: %w", err)
	}

	key := r.formatKey(walletID)
	if err := r.client.Set(ctx, key, data, ttl).Err(); err != nil {
		return fmt.Errorf("redis set error: %w", err)
	}
	return nil
}

// Invalidate deletes one or more wallet balance keys from Redis using a pipeline.
func (r *RedisCache) Invalidate(ctx context.Context, walletIDs ...string) error {
	if r == nil || r.client == nil || len(walletIDs) == 0 {
		return nil
	}

	keys := make([]string, 0, len(walletIDs))
	for _, id := range walletIDs {
		if id != "" {
			keys = append(keys, r.formatKey(id))
		}
	}

	if len(keys) == 0 {
		return nil
	}

	if err := r.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("redis del error: %w", err)
	}
	return nil
}

// Close gracefully closes the Redis client connection.
func (r *RedisCache) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Close()
}

func (r *RedisCache) formatKey(walletID string) string {
	return r.keyPrefix + walletID
}
