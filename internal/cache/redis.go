package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/redisstore"
	"github.com/redis/go-redis/v9"
)

const (
	redisOpTimeout  = 100 * time.Millisecond
	redisScanLimit  = 10_000
	statsSizeTTL    = 30 * time.Second
	redisScanWindow = 500 * time.Millisecond
)

// Redis is a Redis-backed cache (Fase 7). Fallback to memory if Redis down is
// handled by caller.
//
// Stats counters are exact for this process. Size is approximate: it is
// obtained with a capped SCAN and cached for 30 seconds so metrics scrapes do
// not turn into a full keyspace walk. Size never reports -1; a failed scan
// reports 0.
type Redis struct {
	store  redisstore.Store
	prefix string
	hits   atomic.Int64
	misses atomic.Int64

	sizeMu  sync.Mutex
	sizeVal int
	sizeAt  time.Time
}

func NewRedis(client *redis.Client) *Redis {
	return NewRedisWithStore(redisstore.NewStatic(client))
}

func NewRedisWithStore(store redisstore.Store) *Redis {
	return &Redis{store: store, prefix: "cache:"}
}

func (r *Redis) Get(key string) ([]byte, bool) {
	if r.store == nil {
		r.misses.Add(1)
		return nil, false
	}
	var val []byte
	err := r.store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
		defer cancel()
		var err error
		val, err = client.Get(ctx, r.prefix+key).Bytes()
		return err
	})
	if err != nil {
		r.misses.Add(1)
		return nil, false
	}
	r.hits.Add(1)
	return val, true
}

func (r *Redis) Set(key string, value []byte, ttl time.Duration) {
	if r.store == nil {
		return
	}
	_ = r.store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
		defer cancel()
		return client.Set(ctx, r.prefix+key, value, ttl).Err()
	})
}

func (r *Redis) Delete(key string) {
	if r.store == nil {
		return
	}
	_ = r.store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
		defer cancel()
		return client.Del(ctx, r.prefix+key).Err()
	})
}

func (r *Redis) Stats() Stats {
	return Stats{
		Hits:   r.hits.Load(),
		Misses: r.misses.Load(),
		Size:   r.size(),
	}
}

// size returns a capped, 30s-cached key count. A full SCAN on every metrics
// scrape would be unbounded work against the shared Redis instance.
func (r *Redis) size() int {
	now := time.Now()
	r.sizeMu.Lock()
	if !r.sizeAt.IsZero() && now.Sub(r.sizeAt) < statsSizeTTL {
		value := r.sizeVal
		r.sizeMu.Unlock()
		return value
	}
	r.sizeMu.Unlock()

	count := 0
	if r.store != nil {
		err := r.store.WithClient(func(client *redis.Client) error {
			ctx, cancel := context.WithTimeout(context.Background(), redisScanWindow)
			defer cancel()
			var cursor uint64
			for {
				keys, next, err := client.Scan(ctx, cursor, r.prefix+"*", 500).Result()
				if err != nil {
					return err
				}
				count += len(keys)
				cursor = next
				if cursor == 0 || count >= redisScanLimit {
					break
				}
			}
			return nil
		})
		if err != nil {
			count = 0
		}
	}
	if count > redisScanLimit {
		count = redisScanLimit
	}

	r.sizeMu.Lock()
	r.sizeVal = count
	r.sizeAt = time.Now()
	r.sizeMu.Unlock()
	return count
}
