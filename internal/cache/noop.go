package cache

import (
	"context"
	"time"
)

// NoopCache is the zero-config Cache. Every Get reports a miss; every
// Set/Delete/Incr/Decr is a no-op. This is what we wire when REDIS_URL
// is empty — callers see ErrCacheMiss and fall through to Postgres,
// preserving the pre-Redis behaviour exactly.
type NoopCache struct{}

// NewNoopCache returns a NoopCache. Exists for symmetry with
// NewRedisCache — callers don't have to special-case the constructor.
func NewNoopCache() *NoopCache { return &NoopCache{} }

// Get always returns ErrCacheMiss.
func (n *NoopCache) Get(ctx context.Context, key string) (string, error) {
	return "", ErrCacheMiss
}

// Set is a no-op.
func (n *NoopCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return nil
}

// Delete is a no-op.
func (n *NoopCache) Delete(ctx context.Context, key string) error { return nil }

// Incr returns 0 — callers must not depend on the return value when
// running with NoopCache. Quota checks fall through to a Postgres
// COUNT(*) anyway.
func (n *NoopCache) Incr(ctx context.Context, key string) (int64, error) { return 0, nil }

// Decr returns 0. See Incr.
func (n *NoopCache) Decr(ctx context.Context, key string) (int64, error) { return 0, nil }

// Close is a no-op.
func (n *NoopCache) Close() error { return nil }
