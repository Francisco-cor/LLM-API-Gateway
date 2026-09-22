package cache

import (
	"context"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/redisstore"
	"github.com/redis/go-redis/v9"
)

// Redis is a Redis-backed cache (Fase 7). Fallback to memory if Redis down is handled by caller.
type Redis struct {
	store  redisstore.Store
	prefix string
	hits   int64 // not tracked precisely for Redis, approximate
	misses int64
}

func NewRedis(client *redis.Client) *Redis {
	return NewRedisWithStore(redisstore.NewStatic(client))
}

func NewRedisWithStore(store redisstore.Store) *Redis {
	return &Redis{store: store, prefix: "cache:"}
}

func (r *Redis) Get(key string) ([]byte, bool) {
	if r.store == nil {
		return nil, false
	}
	var val []byte
	err := r.store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var err error
		val, err = client.Get(ctx, r.prefix+key).Bytes()
		return err
	})
	if err != nil {
		return nil, false
	}
	return val, true
}

func (r *Redis) Set(key string, value []byte, ttl time.Duration) {
	if r.store == nil {
		return
	}
	_ = r.store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		return client.Set(ctx, r.prefix+key, value, ttl).Err()
	})
}

func (r *Redis) Delete(key string) {
	if r.store == nil {
		return
	}
	_ = r.store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		return client.Del(ctx, r.prefix+key).Err()
	})
}

func (r *Redis) Stats() Stats {
	return Stats{Hits: r.hits, Misses: r.misses, Size: -1}
}
