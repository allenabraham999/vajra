//go:build integration

package cache

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// integrationRedisURL returns the URL the integration tests should hit.
// We default to the docker-compose exposed instance; an override via
// REDIS_TEST_URL lets CI point elsewhere.
func integrationRedisURL() string {
	if v := os.Getenv("REDIS_TEST_URL"); v != "" {
		return v
	}
	return "redis://localhost:6379/15"
}

// TestRedisRoundTrip exercises the four hot-path methods against a real
// Redis. Build-tag-gated because the unit-test pipeline doesn't always
// have the dependency available; run via:
//
//	go test -tags=integration ./internal/cache/...
func TestRedisRoundTrip(t *testing.T) {
	rc, err := NewRedisCache(integrationRedisURL())
	if err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	defer rc.Close()
	ctx := context.Background()
	key := "vajra-test:roundtrip:" + time.Now().Format(time.RFC3339Nano)

	if _, err := rc.Get(ctx, key); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected miss, got %v", err)
	}
	if err := rc.Set(ctx, key, "hello", time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := rc.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if v != "hello" {
		t.Fatalf("Get = %q, want %q", v, "hello")
	}
	if err := rc.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := rc.Get(ctx, key); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected miss after delete, got %v", err)
	}

	counter := key + ":counter"
	defer func() { _ = rc.Delete(ctx, counter) }()
	v1, err := rc.Incr(ctx, counter)
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if v1 != 1 {
		t.Fatalf("Incr = %d, want 1", v1)
	}
	v2, err := rc.Incr(ctx, counter)
	if err != nil {
		t.Fatalf("Incr#2: %v", err)
	}
	if v2 != 2 {
		t.Fatalf("Incr#2 = %d, want 2", v2)
	}
	v3, err := rc.Decr(ctx, counter)
	if err != nil {
		t.Fatalf("Decr: %v", err)
	}
	if v3 != 1 {
		t.Fatalf("Decr = %d, want 1", v3)
	}
}
