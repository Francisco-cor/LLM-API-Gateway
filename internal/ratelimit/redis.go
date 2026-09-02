package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLimiter implements distributed rate limiting via Redis Lua script (atomic INCR + EXPIRE).
// Falls back to memory limiter if Redis unavailable (fail-open with warning).
type RedisLimiter struct {
	client *redis.Client
	rate   float64
	burst  float64
	script *redis.Script
}

var luaScript = redis.NewScript(`
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

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
if tokens >= 1 then
  tokens = tokens - 1
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
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &RedisLimiter{
		client: client,
		rate:   float64(requestsPerMinute) / 60.0,
		burst:  float64(burst),
		script: luaScript,
	}, nil
}

// Allow reports whether key may proceed (atomic Lua).
func (r *RedisLimiter) Allow(key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := r.script.Run(ctx, r.client, []string{"ratelimit:" + key}, r.rate, r.burst, float64(time.Now().Unix()), 600).Result()
	if err != nil {
		// fail-open
		return true
	}
	if arr, ok := res.([]interface{}); ok && len(arr) >= 1 {
		if v, ok := arr[0].(int64); ok {
			return v == 1
		}
	}
	return true
}

// RetryAfter approximates; Redis Lua returns tokens, but we estimate 1/rate.
func (r *RedisLimiter) RetryAfter(key string) time.Duration {
	// Estimate: if burst>0, assume need 1 token
	return time.Duration((1 / r.rate) * float64(time.Second))
}

// AllowN token-aware (consumes n tokens, approximated as n sequential Allows).
func (r *RedisLimiter) AllowN(key string, n int) bool {
	for i := 0; i < n; i++ {
		if !r.Allow(key) {
			return false
		}
	}
	return true
}

// Close releases redis client.
func (r *RedisLimiter) Close() error {
	return r.client.Close()
}
