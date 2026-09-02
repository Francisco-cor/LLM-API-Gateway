package cache

import "time"

// Cache is the interface for response caching (exact-match and semantic).
type Cache interface {
	Get(key string) ([]byte, bool)
	Set(key string, value []byte, ttl time.Duration)
	Delete(key string)
	Stats() Stats
}

type Stats struct {
	Hits   int64
	Misses int64
	Size   int
}
