package cache

import (
	"time"

	"golang.org/x/sync/singleflight"
)

// Group collapses concurrent cache misses for the same key into one provider
// call. The request handler does not use it yet; wave 2 should wrap the
// miss-then-fetch path:
//
//	value, hit, err := group.LoadOrStore(c, key, ttl, func() ([]byte, error) {
//	    return callProvider(ctx)
//	})
//
// A Group value must be shared by all handlers (one per cache instance), not
// created per request, otherwise it cannot deduplicate anything. Group is safe
// for concurrent use.
type Group struct {
	sf singleflight.Group
}

// Do runs fn once for concurrent callers with the same key and returns shared
// results to all of them.
func (g *Group) Do(key string, fn func() ([]byte, error)) (value []byte, err error, shared bool) {
	v, err, shared := g.sf.Do(key, func() (any, error) {
		return fn()
	})
	if err != nil {
		return nil, err, shared
	}
	value, _ = v.([]byte)
	return value, nil, shared
}

type loadResult struct {
	value []byte
	hit   bool
}

// LoadOrStore combines exact-cache lookup with single-flight execution. It
// checks the cache before entering the flight group and again inside it (a
// concurrent caller may have populated the cache while this caller waited).
// A non-positive TTL disables storing the freshly loaded value.
//
// hit reports whether the returned bytes came from the cache.
func (g *Group) LoadOrStore(c Cache, key string, ttl time.Duration, fn func() ([]byte, error)) (value []byte, hit bool, err error) {
	if c != nil && key != "" {
		if cached, ok := c.Get(key); ok {
			return cached, true, nil
		}
	}
	if key == "" {
		value, err = fn()
		return value, false, err
	}
	v, err, _ := g.sf.Do(key, func() (any, error) {
		if c != nil {
			if cached, ok := c.Get(key); ok {
				return loadResult{value: cached, hit: true}, nil
			}
		}
		loaded, err := fn()
		if err != nil {
			return nil, err
		}
		if c != nil && ttl > 0 {
			c.Set(key, loaded, ttl)
		}
		return loadResult{value: loaded}, nil
	})
	if err != nil {
		return nil, false, err
	}
	res, _ := v.(loadResult)
	return res.value, res.hit, nil
}
