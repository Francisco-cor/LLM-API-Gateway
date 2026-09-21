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
	stop   chan struct{}
	done   chan struct{}
	close  sync.Once
}

// Backend is the rate-limit contract used by HTTP middleware and handlers.
// Both the in-process and Redis implementations satisfy it, which keeps the
// storage choice from being silently ignored by the gateway wiring.
type Backend interface {
	Allow(key string) bool
	AllowN(key string, n int) bool
	AllowWithLimits(key string, n, requestsPerMinute, burst int) bool
	RetryAfter(key string) time.Duration
	RetryAfterWithLimits(key string, requestsPerMinute, burst int) time.Duration
	UpdateLimits(requestsPerMinute, burst int)
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
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
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
	rpm, burst := l.GetLimits()
	return l.AllowWithLimits(key, n, rpm, burst)
}

// AllowWithLimits consumes n tokens using a per-key override. The override is
// evaluated at request time so hot-reloaded tenant/model limits take effect
// without creating a second limiter per tenant.
func (l *Limiter) AllowWithLimits(key string, n, requestsPerMinute, burst int) bool {
	if n <= 0 {
		n = 1
	}
	if requestsPerMinute <= 0 || burst <= 0 {
		return false
	}
	rate := float64(requestsPerMinute) / 60.0
	maxTokens := float64(burst)
	sh := l.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	b := l.refillLocked(sh, key, rate, maxTokens)
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
	rpm, burst := l.GetLimits()
	return l.RetryAfterWithLimits(key, rpm, burst)
}

// RetryAfterWithLimits returns the wait time for one token using an override.
func (l *Limiter) RetryAfterWithLimits(key string, requestsPerMinute, burst int) time.Duration {
	if requestsPerMinute <= 0 || burst <= 0 {
		return time.Second
	}
	sh := l.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	rate := float64(requestsPerMinute) / 60.0
	b := l.refillLocked(sh, key, rate, float64(burst))
	if b.tokens >= 1 {
		return 0
	}
	seconds := (1 - b.tokens) / rate
	return time.Duration(seconds * float64(time.Second))
}

// Tokens returns current available tokens (for testing/metrics).
func (l *Limiter) Tokens(key string) float64 {
	sh := l.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	l.mu.RLock()
	rate := l.rate
	burst := l.burst
	l.mu.RUnlock()
	b := l.refillLocked(sh, key, rate, burst)
	return b.tokens
}

// refillLocked applies elapsed-time refill to key's bucket. Callers must hold shard mu.
func (l *Limiter) refillLocked(sh *shard, key string, rate, burst float64) *bucket {
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
	defer close(l.done)
	for {
		select {
		case <-ticker.C:
			l.cleanup()
		case <-l.stop:
			return
		}
	}
}

// Close stops the background cleanup goroutine. It is safe to call multiple
// times and prevents limiter goroutines from surviving server shutdown.
func (l *Limiter) Close() error {
	l.close.Do(func() { close(l.stop) })
	<-l.done
	return nil
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
