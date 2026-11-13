package cache

import "time"

// Config mirrors the gateway cache settings that determine whether a cache
// instance must be rebuilt. Keeping it in this package lets the reload path
// avoid throwing away a warm cache (and its Redis/memory contents) on every
// configuration reload.
//
// cmd/gateway should adopt:
//
//	oldCfg := cache.Config{Enabled: old.Cache.Enabled, TTL: old.Cache.TTL, ...}
//	newCfg := cache.Config{Enabled: new.Cache.Enabled, TTL: new.Cache.TTL, ...}
//	cacheInst = cache.BuildIfChanged(cacheInst, oldCfg, newCfg, build)
//
// The Redis field must be set when the underlying redisstore is active; moving
// between memory and Redis is a rebuild even if the remaining settings match.
type Config struct {
	Enabled                bool
	TTL                    time.Duration
	MaxSize                int
	MaxBytes               int64
	MaxTTL                 time.Duration
	SemanticEnabled        bool
	SemanticThreshold      float64
	SemanticEmbeddingModel string
	SemanticTopK           int
	SemanticMaxEntries     int
	Redis                  bool
}

// ConfigEquals reports whether two cache configurations are equivalent.
func ConfigEquals(a, b Config) bool {
	return a == b
}

// BuildIfChanged returns current when next is equivalent to previous. Otherwise
// it calls build(next) to construct a replacement cache. A nil current cache is
// always rebuilt so enabling the cache after startup works.
func BuildIfChanged(current Cache, previous, next Config, build func(Config) Cache) Cache {
	if current != nil && ConfigEquals(previous, next) {
		return current
	}
	return build(next)
}
