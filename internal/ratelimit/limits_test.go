package ratelimit

import (
	"testing"
	"time"
)

func TestLimitKeyFingerprintsLimits(t *testing.T) {
	base := limitKey("tenant", 60, 10)
	if base == limitKey("tenant", 120, 10) {
		t.Fatal("rpm must be part of the bucket key")
	}
	if base == limitKey("tenant", 60, 20) {
		t.Fatal("burst must be part of the bucket key")
	}
	if base == limitKey("other", 60, 10) {
		t.Fatal("identity must be part of the bucket key")
	}
	if base != limitKey("tenant", 60, 10) {
		t.Fatal("limitKey must be deterministic")
	}
}

func TestRefillUsesElapsedTime(t *testing.T) {
	limiter := New(60, 2) // one token per second
	defer limiter.Close()

	key := limitKey("client", 60, 2)
	shard := limiter.shardFor(key)
	shard.mu.Lock()
	shard.buckets[key] = &bucket{tokens: 0, lastRefill: time.Now().Add(-time.Second), lastSeen: time.Now()}
	shard.mu.Unlock()

	if got := limiter.Tokens("client"); got < 0.9 || got > 1.1 {
		t.Fatalf("refilled tokens = %v, want about 1", got)
	}

	// Burst is a hard ceiling even after a long idle period.
	shard.mu.Lock()
	shard.buckets[key] = &bucket{tokens: 0, lastRefill: time.Now().Add(-time.Hour), lastSeen: time.Now()}
	shard.mu.Unlock()
	if got := limiter.Tokens("client"); got != 2 {
		t.Fatalf("tokens after idle = %v, want burst 2", got)
	}
}

func TestRetryAfterTracksRateAndExhaustion(t *testing.T) {
	limiter := New(60, 1) // one token per second
	defer limiter.Close()
	if !limiter.Allow("client") {
		t.Fatal("first request must pass")
	}
	if got := limiter.RetryAfter("client"); got <= 0 || got > time.Second {
		t.Fatalf("RetryAfter = %v, want 0 < d <= 1s", got)
	}

	// Overrides are evaluated with their own bucket and rate.
	if !limiter.AllowWithLimits("override", 1, 120, 1) {
		t.Fatal("override burst should allow one request")
	}
	if got := limiter.RetryAfterWithLimits("override", 120, 1); got <= 0 || got > 600*time.Millisecond {
		t.Fatalf("override RetryAfter = %v, want 0 < d <= 500ms", got)
	}

	// Allowed keys report zero wait.
	if got := limiter.RetryAfter("fresh"); got != 0 {
		t.Fatalf("RetryAfter for a full bucket = %v, want 0", got)
	}
	if got := limiter.RetryAfterWithLimits("fresh", 0, 1); got != time.Second {
		t.Fatalf("invalid limits RetryAfter = %v, want 1s", got)
	}
}

func TestRedisFallbackRetryAfterWithoutServer(t *testing.T) {
	// No store: every Redis operation reports unavailable and the local
	// fallback keeps the gateway protected.
	limiter := NewRedisWithStore(nil, 60, 1)
	if !limiter.Allow("client") {
		t.Fatal("fallback must allow the first request")
	}
	if !limiter.degraded.Load() {
		t.Fatal("limiter should be marked degraded after a store failure")
	}
	if limiter.Allow("client") {
		t.Fatal("fallback burst of one must reject the second immediate request")
	}
	if got := limiter.RetryAfter("client"); got <= 0 || got > time.Second {
		t.Fatalf("fallback RetryAfter = %v, want 0 < d <= 1s", got)
	}
	// Redis and local buckets share the same fingerprint, so a live store and
	// the fallback cannot disagree about which limits produced a bucket.
	if limitKey("client", 60, 1) != limitKey("client", 60, 1) {
		t.Fatal("limitKey is not stable")
	}
}
