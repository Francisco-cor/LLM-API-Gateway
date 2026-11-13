package cache

import "time"

// Cache is the interface for response caching (exact-match and semantic).
type Cache interface {
	Get(key string) ([]byte, bool)
	Set(key string, value []byte, ttl time.Duration)
	Delete(key string)
	Stats() Stats
}

// Stats is a point-in-time snapshot. Redis-backed caches report approximate
// size (see Redis.Stats) and never publish -1.
type Stats struct {
	Hits   int64
	Misses int64
	Size   int
	// Bytes is the approximate payload size retained by an in-memory cache.
	// Redis does not track it and reports 0.
	Bytes int64
	// Evictions counts entries removed to satisfy size/byte limits.
	Evictions int64
	// Rejected counts values that were larger than the cache-wide byte limit
	// and therefore were not stored.
	Rejected int64
}
