package cache

import "time"

// ClampTTL bounds a caller-supplied cache TTL such as the X-Cache-TTL request
// header against a configured maximum (cache.max_ttl). The handler does not
// call it yet; wave 2 should apply it before passing the TTL to Set:
//
//	cacheTTL = cache.ClampTTL(cacheTTL, cfg.Cache.MaxTTL)
//
// Semantics: a non-positive requested TTL falls back to max; a non-positive max
// means "no cap". The result is never negative.
func ClampTTL(requested, max time.Duration) time.Duration {
	if requested <= 0 {
		if max > 0 {
			return max
		}
		return 0
	}
	if max > 0 && requested > max {
		return max
	}
	return requested
}
