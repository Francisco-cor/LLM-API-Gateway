package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLimiter implements distributed rate limiting via a Redis Lua token
// bucket. It falls back to an in-process limiter if Redis becomes unavailable.
type RedisLimiter struct {
	client   *redis.Client
	mu       sync.RWMutex
	rate     float64
	burst    float64
	script   *redis.Script
	fallback *Limiter
	degraded atomic.Bool
}

var luaScript = redis.NewScript(`
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])
local requested = tonumber(ARGV[5]) or 1

local bucket = redis.call("HMGET", key, "tokens", "last_refill")
local tokens = tonumber(bucket[1])
local last_refill = tonumber(bucket[2])

if tokens == nil then
  tokens = burst
  last_refill = now
else
  local elapsed = now - last_refill
  tokens = tokens + elapsed * rate
  if tokens > burst then tokens = burst end
end

local allowed = 0
if tokens >= requested then
  tokens = tokens - requested
  allowed = 1
end

redis.call("HMSET", key, "tokens", tokens, "last_refill", now)
redis.call("EXPIRE", key, ttl)

return {allowed, tokens}
`)

// NewRedis creates a RedisLimiter. redisURL e.g. "redis://localhost:6379/0".
func NewRedis(redisURL string, requestsPerMinute, burst int) (*RedisLimiter, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &RedisLimiter{
		client:   client,
		rate:     float64(requestsPerMinute) / 60.0,
		burst:    float64(burst),
		script:   luaScript,
		fallback: New(requestsPerMinute, burst),
	}, nil
}

// Allow reports whether key may proceed (atomic Lua).
func (r *RedisLimiter) Allow(key string) bool {
	return r.AllowN(key, 1)
}

func (r *RedisLimiter) AllowN(key string, n int) bool {
	r.mu.RLock()
	rpm := int(r.rate * 60)
	burst := int(r.burst)
	r.mu.RUnlock()
	return r.AllowWithLimits(key, n, rpm, burst)
}

// AllowWithLimits performs one atomic Redis operation for the requested token
// count, unlike the old sequential AllowN implementation.
func (r *RedisLimiter) AllowWithLimits(key string, n, requestsPerMinute, burst int) bool {
	if n <= 0 {
		n = 1
	}
	if requestsPerMinute <= 0 || burst <= 0 {
		return false
	}
	rate := float64(requestsPerMinute) / 60.0
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := r.script.Run(ctx, r.client, []string{"ratelimit:" + key}, rate, float64(burst), float64(time.Now().UnixNano())/1e9, 600, n).Result()
	if err != nil {
		// Preserve availability while retaining local protection during a Redis
		// outage. The next successful Redis operation leaves degraded mode.
		r.degraded.Store(true)
		return r.fallback.AllowWithLimits(key, n, requestsPerMinute, burst)
	}
	r.degraded.Store(false)
	if arr, ok := res.([]interface{}); ok && len(arr) >= 1 {
		if v, ok := arr[0].(int64); ok {
			return v == 1
		}
	}
	r.degraded.Store(true)
	return r.fallback.AllowWithLimits(key, n, requestsPerMinute, burst)
}

// RetryAfter approximates; Redis Lua returns tokens, but we estimate 1/rate.
func (r *RedisLimiter) RetryAfter(key string) time.Duration {
	r.mu.RLock()
	rpm := int(r.rate * 60)
	burst := int(r.burst)
	r.mu.RUnlock()
	return r.RetryAfterWithLimits(key, rpm, burst)
}

func (r *RedisLimiter) RetryAfterWithLimits(key string, requestsPerMinute, burst int) time.Duration {
	if requestsPerMinute <= 0 {
		return time.Second
	}
	if r.degraded.Load() {
		return r.fallback.RetryAfterWithLimits(key, requestsPerMinute, burst)
	}
	return time.Duration((60 / float64(requestsPerMinute)) * float64(time.Second))
}

// UpdateLimits changes the default Redis bucket settings for future requests.
func (r *RedisLimiter) UpdateLimits(requestsPerMinute, burst int) {
	if requestsPerMinute <= 0 || burst <= 0 {
		return
	}
	r.mu.Lock()
	r.rate = float64(requestsPerMinute) / 60.0
	r.burst = float64(burst)
	r.mu.Unlock()
	r.fallback.UpdateLimits(requestsPerMinute, burst)
}

// Close releases redis client.
func (r *RedisLimiter) Close() error {
	if r.fallback != nil {
		_ = r.fallback.Close()
	}
	return r.client.Close()
}
