package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCache is the Redis-backed Cache used in production. The client is
// goroutine-safe (go-redis maintains its own pool) so a single instance
// is shared across the entire master process.
type RedisCache struct {
	client *redis.Client
}

// NewRedisCache parses the redisURL (redis://[:password@]host:port/db),
// builds a pool, and pings the server to fail fast on misconfiguration.
// Returns the RedisCache ready to use; callers must Close() at shutdown.
func NewRedisCache(redisURL string) (*RedisCache, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	if opts.PoolSize == 0 {
		opts.PoolSize = 20
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 3 * time.Second
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 2 * time.Second
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = 2 * time.Second
	}
	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &RedisCache{client: client}, nil
}

// Get returns the value at key, or ErrCacheMiss if absent.
func (r *RedisCache) Get(ctx context.Context, key string) (string, error) {
	v, err := r.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrCacheMiss
	}
	if err != nil {
		return "", fmt.Errorf("redis get: %w", err)
	}
	return v, nil
}

// Set writes value at key with ttl. A zero ttl means no expiry; in
// practice every caller sets a TTL so the cache cannot grow unbounded.
func (r *RedisCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if err := r.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("redis set: %w", err)
	}
	return nil
}

// Delete removes key. Missing keys are not an error.
func (r *RedisCache) Delete(ctx context.Context, key string) error {
	if err := r.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("redis del: %w", err)
	}
	return nil
}

// Incr atomically increments key, creating it at 1 when absent.
func (r *RedisCache) Incr(ctx context.Context, key string) (int64, error) {
	v, err := r.client.Incr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("redis incr: %w", err)
	}
	return v, nil
}

// Decr atomically decrements key. Redis allows negative values; callers
// that care about non-negative counters must clamp themselves.
func (r *RedisCache) Decr(ctx context.Context, key string) (int64, error) {
	v, err := r.client.Decr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("redis decr: %w", err)
	}
	return v, nil
}

// Close shuts the connection pool down.
func (r *RedisCache) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Close()
}
