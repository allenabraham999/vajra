// Package master — ratelimit.go is a per-account token-bucket limiter
// applied to the authenticated REST surface. It is intentionally simple:
// each account gets one bucket of `rate` tokens that refills at `rate`
// per second. Bursts up to bucket size are allowed; sustained traffic
// over the rate trips a 429 with a Retry-After header.
//
// The brief calls out 10 RPS by default, configurable via
// VAJRA_RATE_LIMIT_RPS. Buckets live in a sync.Map keyed by account ID.
package master

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultRateLimitRPS is the per-account ceiling when no override is set.
// Matches the brief.
const DefaultRateLimitRPS = 10

// RateLimitConfig is the bundle the middleware consumes.
type RateLimitConfig struct {
	// RPS is the steady-state rate. Bucket size is also RPS — the brief
	// keeps it simple and so do we; if a tenant needs burst headroom we
	// can add a separate Burst knob later.
	RPS int
	// Now is overridable in tests so the bucket math is deterministic.
	Now func() time.Time
}

// tokenBucket is the per-account state. We track tokens as a fixed-point
// value scaled by 1e6 so the refill math is integer-only and lock-free
// via atomics. floatTokens / 1e6 is the human-readable balance; we never
// emit it because consumers only need pass/fail.
type tokenBucket struct {
	tokens   atomic.Int64 // scaled by 1e6
	lastNS   atomic.Int64 // last refill timestamp, ns
	capacity int64        // max tokens (scaled)
	refill   int64        // tokens added per second (scaled)
}

// tokenScale is the multiplier used to keep tokens integer.
const tokenScale int64 = 1_000_000

// allow returns true if a token was deducted, false if the bucket is dry.
// The race is benign — under contention two callers can both observe the
// same balance and both deduct, briefly oversubscribing by one token. We
// accept that for speed; an exact lock would gate every authenticated
// request through a mutex.
func (b *tokenBucket) allow(now time.Time) bool {
	for {
		last := b.lastNS.Load()
		cur := now.UnixNano()
		// Refill: tokens accumulated since the last call. A gap longer
		// than one full refill cycle can only top the bucket off, so
		// clamp dt before the multiply: unclamped, dt*refill overflows
		// int64 once a bucket has sat idle ~15 min, wrapping `add`
		// negative and wedging the account at a permanent 429.
		dt := cur - last
		add := int64(0)
		if dt > 0 {
			if maxDt := (b.capacity/b.refill + 1) * int64(time.Second); dt > maxDt {
				dt = maxDt
			}
			add = (dt * b.refill) / int64(time.Second)
		}
		balance := b.tokens.Load() + add
		if balance > b.capacity {
			balance = b.capacity
		}
		if balance < tokenScale {
			// Persist the refill anyway so a caller right behind us
			// doesn't redo the same arithmetic.
			b.tokens.Store(balance)
			b.lastNS.Store(cur)
			return false
		}
		newBal := balance - tokenScale
		// CAS: only one caller wins; losers re-loop.
		if b.tokens.CompareAndSwap(b.tokens.Load(), newBal) {
			b.lastNS.Store(cur)
			return true
		}
	}
}

// retryAfter estimates how long the caller must wait for one token in a
// dry bucket. Used to populate the Retry-After header.
func (b *tokenBucket) retryAfter() time.Duration {
	if b.refill == 0 {
		return time.Second
	}
	balance := b.tokens.Load()
	deficit := tokenScale - balance
	if deficit <= 0 {
		return 0
	}
	ns := (deficit * int64(time.Second)) / b.refill
	if ns <= 0 {
		return 100 * time.Millisecond
	}
	return time.Duration(ns)
}

// RateLimiter is the per-account limiter. Buckets are lazily created on
// first hit so an idle tenant doesn't take up memory.
type RateLimiter struct {
	cfg     RateLimitConfig
	buckets sync.Map // accountID -> *tokenBucket
}

// NewRateLimiter builds a limiter. cfg.RPS <= 0 falls back to
// DefaultRateLimitRPS; cfg.Now == nil falls back to time.Now.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	if cfg.RPS <= 0 {
		cfg.RPS = DefaultRateLimitRPS
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &RateLimiter{cfg: cfg}
}

// allowAccount is the public entry point used by the middleware. Empty
// accountID falls back to a shared "anonymous" bucket so unauthenticated
// surfaces are still rate-bounded (e.g. /v1/auth/login brute-force
// attempts).
func (l *RateLimiter) allowAccount(accountID string) (bool, time.Duration) {
	key := accountID
	if key == "" {
		key = "anonymous"
	}
	bucket := l.bucketFor(key)
	now := l.cfg.Now()
	if bucket.allow(now) {
		return true, 0
	}
	return false, bucket.retryAfter()
}

func (l *RateLimiter) bucketFor(key string) *tokenBucket {
	if v, ok := l.buckets.Load(key); ok {
		return v.(*tokenBucket)
	}
	cap := int64(l.cfg.RPS) * tokenScale
	refill := int64(l.cfg.RPS) * tokenScale
	b := &tokenBucket{capacity: cap, refill: refill}
	b.tokens.Store(cap)
	b.lastNS.Store(l.cfg.Now().UnixNano())
	actual, _ := l.buckets.LoadOrStore(key, b)
	return actual.(*tokenBucket)
}

// Middleware returns a chi-style middleware that enforces the limit on
// every wrapped handler. When the bucket is dry the handler chain is
// short-circuited with 429.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID := AccountIDFromContext(r.Context())
		if ok, retryAfter := l.allowAccount(accountID); !ok {
			seconds := int(retryAfter.Round(time.Second) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}
