package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestNoopCacheGetMiss confirms Get always returns ErrCacheMiss — the
// contract callers depend on for fall-through to Postgres.
func TestNoopCacheGetMiss(t *testing.T) {
	c := NewNoopCache()
	_, err := c.Get(context.Background(), "anything")
	if !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("want ErrCacheMiss, got %v", err)
	}
}

// TestNoopCacheWritesNoOp confirms Set/Delete/Incr/Decr never error
// and never affect a subsequent Get.
func TestNoopCacheWritesNoOp(t *testing.T) {
	c := NewNoopCache()
	ctx := context.Background()
	if err := c.Set(ctx, "k", "v", time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := c.Incr(ctx, "k"); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if _, err := c.Decr(ctx, "k"); err != nil {
		t.Fatalf("Decr: %v", err)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("want miss after writes, got %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
