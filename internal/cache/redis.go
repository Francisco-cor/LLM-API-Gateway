package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is a Redis-backed cache (Fase 7). Fallback to memory if Redis down is handled by caller.
type Redis struct {
	client *redis.Client
	prefix string
	hits   int64 // not tracked precisely for Redis, approximate
	misses int64
}

func NewRedis(client *redis.Client) *Redis {
	return &Redis{client: client, prefix: "cache:"}
}

func (r *Redis) Get(key string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	val, err := r.client.Get(ctx, r.prefix+key).Bytes()
	if err != nil {
		return nil, false
	}
	return val, true
}

func (r *Redis) Set(key string, value []byte, ttl time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = r.client.Set(ctx, r.prefix+key, value, ttl).Err()
}

func (r *Redis) Delete(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = r.client.Del(ctx, r.prefix+key).Err()
}

func (r *Redis) Stats() Stats {
	return Stats{Hits: r.hits, Misses: r.misses, Size: -1}
}
