package master

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRateLimiter_Allows lets the configured RPS through and then trips
// once the bucket is dry. We pin "now" so the math is deterministic.
func TestRateLimiter_AllowsThenTrips(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := NewRateLimiter(RateLimitConfig{
		RPS: 3,
		Now: func() time.Time { return now },
	})
	for i := 0; i < 3; i++ {
		ok, _ := limiter.allowAccount("acct-A")
		if !ok {
			t.Fatalf("call %d: expected allow, got deny", i)
		}
	}
	// Bucket dry — next call must trip.
	ok, retry := limiter.allowAccount("acct-A")
	if ok {
		t.Fatalf("expected 4th call to trip the limiter")
	}
	if retry <= 0 {
		t.Fatalf("expected positive Retry-After, got %s", retry)
	}
}

// TestRateLimiter_PerAccount confirms the buckets are isolated: account A
// being throttled doesn't affect account B's quota.
func TestRateLimiter_PerAccount(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	limiter := NewRateLimiter(RateLimitConfig{
		RPS: 1,
		Now: func() time.Time { return now },
	})
	if ok, _ := limiter.allowAccount("acct-A"); !ok {
		t.Fatalf("first A call should pass")
	}
	if ok, _ := limiter.allowAccount("acct-A"); ok {
		t.Fatalf("second A call should trip")
	}
	if ok, _ := limiter.allowAccount("acct-B"); !ok {
		t.Fatalf("first B call should pass — buckets are per-account")
	}
}

// TestRateLimiter_RefillsOverTime advances the clock past the bucket
// refill window and checks that a previously-throttled caller is allowed
// through again.
func TestRateLimiter_RefillsOverTime(t *testing.T) {
	clock := time.Unix(1_700_000_200, 0)
	limiter := NewRateLimiter(RateLimitConfig{
		RPS: 2,
		Now: func() time.Time { return clock },
	})
	// Drain.
	for i := 0; i < 2; i++ {
		if ok, _ := limiter.allowAccount("acct-C"); !ok {
			t.Fatalf("call %d should pass before drain", i)
		}
	}
	if ok, _ := limiter.allowAccount("acct-C"); ok {
		t.Fatalf("expected drain to trip the limiter")
	}
	// Advance 1s — bucket should be back at full capacity (RPS tokens).
	clock = clock.Add(time.Second)
	for i := 0; i < 2; i++ {
		if ok, _ := limiter.allowAccount("acct-C"); !ok {
			t.Fatalf("post-refill call %d should pass", i)
		}
	}
}

// TestRateLimiter_Middleware_429 confirms the middleware short-circuits
// to 429 with a Retry-After header when the bucket is dry.
func TestRateLimiter_Middleware_429(t *testing.T) {
	now := time.Unix(1_700_000_300, 0)
	limiter := NewRateLimiter(RateLimitConfig{
		RPS: 1,
		Now: func() time.Time { return now },
	})
	called := 0
	wrapped := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))

	// Inject an account so the bucket key isn't "anonymous" (and so we
	// can confirm per-account semantics work end-to-end).
	withAccount := func(r *http.Request) *http.Request {
		ctx := WithAccountID(r.Context(), "acct-MW")
		return r.WithContext(ctx)
	}

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, withAccount(httptest.NewRequest(http.MethodGet, "/x", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	wrapped.ServeHTTP(rec, withAccount(httptest.NewRequest(http.MethodGet, "/x", nil)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second call: expected 429, got %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatalf("expected Retry-After header, got empty")
	}
	if called != 1 {
		t.Fatalf("inner handler should not run on a 429, called=%d", called)
	}
}

// TestRateLimiter_Anonymous_Bucket confirms unauthenticated requests
// share a single "anonymous" bucket — login/register endpoints are
// rate-limited before account context exists.
func TestRateLimiter_Anonymous_Bucket(t *testing.T) {
	now := time.Unix(1_700_000_400, 0)
	limiter := NewRateLimiter(RateLimitConfig{
		RPS: 1,
		Now: func() time.Time { return now },
	})
	ctx := context.Background()
	_ = ctx
	ok1, _ := limiter.allowAccount("")
	ok2, _ := limiter.allowAccount("")
	if !ok1 {
		t.Fatalf("first anonymous call should pass")
	}
	if ok2 {
		t.Fatalf("second anonymous call should trip the shared bucket")
	}
}
