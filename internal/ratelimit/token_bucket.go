package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a per-key token bucket rate limiter. Each key (typically the
// client's API key) gets its own bucket that refills continuously at rate
// tokens/second up to a maximum of burst tokens.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
}

// New creates a Limiter allowing requestsPerMinute sustained requests per
// key, with up to burst requests allowed instantaneously.
func New(requestsPerMinute, burst int) *Limiter {
	return &Limiter{
		buckets: make(map[string]*bucket),
		rate:    float64(requestsPerMinute) / 60.0,
		burst:   float64(burst),
	}
}

// Allow reports whether a request for key may proceed, consuming one token
// if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refill(key)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// RetryAfter returns how long the caller should wait before key's bucket has
// at least one token available again.
func (l *Limiter) RetryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refill(key)
	if b.tokens >= 1 {
		return 0
	}
	seconds := (1 - b.tokens) / l.rate
	return time.Duration(seconds * float64(time.Second))
}

// refill applies elapsed-time refill to key's bucket and returns it. Callers
// must hold l.mu.
func (l *Limiter) refill(key string) *bucket {
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, lastRefill: now}
		l.buckets[key] = b
		return b
	}

	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.lastRefill = now
	return b
}
