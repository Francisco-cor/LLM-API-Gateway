package ratelimit

import (
	"hash/fnv"
	"sync"
	"time"
)

// Limiter is a per-key token bucket rate limiter with sharded mutexes (16 shards)
// and TTL expiration (10m) to avoid unbounded memory and contention.
type Limiter struct {
	shards [16]*shard
	mu     sync.RWMutex
	rate   float64
	burst  float64
	ttl    time.Duration
}

type shard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
}

// New creates a Limiter allowing requestsPerMinute sustained requests per
// key, with up to burst requests allowed instantaneously.
func New(requestsPerMinute, burst int) *Limiter {
	l := &Limiter{
		rate:  float64(requestsPerMinute) / 60.0,
		burst: float64(burst),
		ttl:   10 * time.Minute,
	}
	for i := range l.shards {
		l.shards[i] = &shard{buckets: make(map[string]*bucket)}
	}
	go l.cleanupLoop()
	return l
}

func (l *Limiter) shardFor(key string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return l.shards[h.Sum32()%16]
}

// Allow reports whether a request for key may proceed, consuming one token
// if so.
func (l *Limiter) Allow(key string) bool {
	return l.AllowN(key, 1)
}

// AllowN consumes n tokens if available (token-aware, Fase 6).
func (l *Limiter) AllowN(key string, n int) bool {
	if n <= 0 {
		n = 1
	}
	sh := l.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	b := l.refillLocked(sh, key)
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}

// UpdateLimits atomically updates rate and burst (for hot-reload via admin).
func (l *Limiter) UpdateLimits(requestsPerMinute, burst int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if requestsPerMinute > 0 {
		l.rate = float64(requestsPerMinute) / 60.0
	}
	if burst > 0 {
		l.burst = float64(burst)
	}
}

// GetLimits returns current RPM and burst.
func (l *Limiter) GetLimits() (int, int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return int(l.rate * 60), int(l.burst)
}

// SetBurstForTesting allows tests to set burst directly.
func (l *Limiter) SetBurstForTesting(burst int) {
	l.mu.Lock()
	l.burst = float64(burst)
	l.mu.Unlock()
}

// RetryAfter returns how long the caller should wait before key's bucket has
// at least one token available again.
func (l *Limiter) RetryAfter(key string) time.Duration {
	sh := l.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	b := l.refillLocked(sh, key)
	if b.tokens >= 1 {
		return 0
	}
	l.mu.RLock()
	rate := l.rate
	l.mu.RUnlock()
	seconds := (1 - b.tokens) / rate
	return time.Duration(seconds * float64(time.Second))
}

// Tokens returns current available tokens (for testing/metrics).
func (l *Limiter) Tokens(key string) float64 {
	sh := l.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	b := l.refillLocked(sh, key)
	return b.tokens
}

// refillLocked applies elapsed-time refill to key's bucket. Callers must hold shard mu.
func (l *Limiter) refillLocked(sh *shard, key string) *bucket {
	l.mu.RLock()
	rate := l.rate
	burst := l.burst
	l.mu.RUnlock()
	now := time.Now()
	b, ok := sh.buckets[key]
	if !ok {
		b = &bucket{tokens: burst, lastRefill: now, lastSeen: now}
		sh.buckets[key] = b
		return b
	}

	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.lastRefill = now
	b.lastSeen = now
	return b
}

func (l *Limiter) cleanupLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		l.cleanup()
	}
}

func (l *Limiter) cleanup() {
	cutoff := time.Now().Add(-l.ttl)
	for _, sh := range l.shards {
		sh.mu.Lock()
		for k, b := range sh.buckets {
			if b.lastSeen.Before(cutoff) {
				delete(sh.buckets, k)
			}
		}
		sh.mu.Unlock()
	}
}

// EstimateTokens estimates tokens for a ChatRequest (4 chars ~ 1 token).
func EstimateTokens(messagesChars int) int {
	if messagesChars <= 0 {
		return 1
	}
	tokens := messagesChars / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}
