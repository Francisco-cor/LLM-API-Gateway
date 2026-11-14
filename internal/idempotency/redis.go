package idempotency

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisDataPrefix = "gw:idempotency:data:"
	redisLockPrefix = "gw:idempotency:lock:"
	redisTimeout    = 300 * time.Millisecond
)

func (m *Manager) redisGet(ctx context.Context, key string) ([]byte, error) {
	if m.redis == nil {
		return nil, errRedisUnavailable
	}
	var value []byte
	err := m.redis.WithClient(func(client *redis.Client) error {
		opCtx, cancel := context.WithTimeout(ctx, redisTimeout)
		defer cancel()
		var err error
		value, err = client.Get(opCtx, redisDataPrefix+key).Bytes()
		return err
	})
	return value, err
}

func (m *Manager) redisSet(ctx context.Context, key string, value []byte, ttl time.Duration) {
	if m.redis == nil {
		return
	}
	_ = m.redis.WithClient(func(client *redis.Client) error {
		opCtx, cancel := context.WithTimeout(ctx, redisTimeout)
		defer cancel()
		return client.Set(opCtx, redisDataPrefix+key, value, ttl).Err()
	})
}

// acquireRedisLock uses SET NX so exactly one replica executes the request.
func (m *Manager) acquireRedisLock(ctx context.Context, key, token string) (bool, error) {
	if m.redis == nil {
		return false, errRedisUnavailable
	}
	var acquired bool
	err := m.redis.WithClient(func(client *redis.Client) error {
		opCtx, cancel := context.WithTimeout(ctx, redisTimeout)
		defer cancel()
		ok, err := client.SetNX(opCtx, redisLockPrefix+key, token, m.Config().LockTTL).Result()
		acquired = ok
		return err
	})
	return acquired, err
}

// releaseRedisLock only deletes the lock if it still belongs to token, so a
// leader whose lock expired cannot release a successor's lock.
func (m *Manager) releaseRedisLock(ctx context.Context, key, token string) {
	if m.redis == nil {
		return
	}
	const script = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`
	_ = m.redis.WithClient(func(client *redis.Client) error {
		opCtx, cancel := context.WithTimeout(ctx, redisTimeout)
		defer cancel()
		return client.Eval(opCtx, script, []string{redisLockPrefix + key}, token).Err()
	})
}
