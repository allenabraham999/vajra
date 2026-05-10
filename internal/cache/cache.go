// Package cache is the hot-path key/value layer between master and the
// authoritative Postgres store. It exists so we don't hit the database
// on every read of fast-changing values (sandbox state, node usage,
// account quotas). Implementations are picked at startup via the
// REDIS_URL env var: set → RedisCache, unset → NoopCache. The Noop
// implementation always reports a miss, so callers fall straight
// through to Postgres and we keep full backward compatibility.
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrCacheMiss is returned by Get when the key is absent. Callers MUST
// treat this as a non-error signal and fall through to the source of
// truth (Postgres).
var ErrCacheMiss = errors.New("cache: miss")

// Cache is the narrow interface every consumer depends on. The shape is
// deliberately small — anything richer (JSON marshalling, struct
// helpers) is built on top in helpers.go.
type Cache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	Incr(ctx context.Context, key string) (int64, error)
	Decr(ctx context.Context, key string) (int64, error)
	Close() error
}
