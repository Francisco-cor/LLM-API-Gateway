package main

import (
	"log/slog"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
	"github.com/fcordero/llm-api-gateway/internal/redisstore"
)

// cacheConfigOf projects the YAML cache settings onto the reload-comparable
// cache.Config. Redis is part of the identity: moving between memory and Redis
// must rebuild even when the remaining settings match.
func cacheConfigOf(cfg *config.Config, redisActive bool) cache.Config {
	return cache.Config{
		Enabled:                cfg.Cache.Enabled,
		TTL:                    cfg.Cache.TTL,
		MaxSize:                cfg.Cache.MaxSize,
		MaxBytes:               cfg.Cache.MaxBytes,
		MaxTTL:                 cfg.Cache.MaxTTL,
		SemanticEnabled:        cfg.Cache.SemanticEnabled,
		SemanticThreshold:      cfg.Cache.SemanticThreshold,
		SemanticEmbeddingModel: cfg.Cache.SemanticEmbeddingModel,
		SemanticTopK:           cfg.Cache.SemanticTopK,
		SemanticMaxEntries:     cfg.Cache.SemanticMaxEntries,
		Redis:                  redisActive,
	}
}

// maxTTLCache caps the TTL requested by a client (X-Cache-TTL) before it
// reaches the underlying cache. This is the cmd/gateway-side wiring of
// cache.max_ttl; proxy.Handler does not need a new setter for it.
type maxTTLCache struct {
	cache.Cache
	max time.Duration
}

func (c *maxTTLCache) Set(key string, value []byte, ttl time.Duration) {
	c.Cache.Set(key, value, cache.ClampTTL(ttl, c.max))
}

// buildCacheForConfig constructs a cache from an already-computed cache.Config.
// It is the build callback used by cache.BuildIfChanged on reload.
func buildCacheForConfig(cc cache.Config, redisStore redisstore.Store, registry *proxy.Registry, log *slog.Logger) cache.Cache {
	if !cc.Enabled {
		return nil
	}
	var base cache.Cache = cache.NewMemoryWithBytes(cc.MaxSize, cc.MaxBytes)
	if cc.Redis {
		base = cache.NewRedisWithStore(redisStore)
		log.Info("cache redis enabled", "ttl", cc.TTL)
	}
	// Clamp X-Cache-TTL before both exact and semantic writes. Wrapping the
	// exact cache keeps the SemanticLookup interface intact because Semantic
	// forwards exact writes to this wrapper.
	if cc.MaxTTL > 0 {
		base = &maxTTLCache{Cache: base, max: cc.MaxTTL}
	}
	if cc.SemanticEnabled {
		embed := semanticEmbedder(registry, cc.SemanticEmbeddingModel)
		if embed == nil {
			log.Warn("semantic cache enabled without a usable embedding provider; exact cache remains active", "model", cc.SemanticEmbeddingModel)
		} else {
			base = cache.NewSemanticWithEmbedder(base, true, cc.SemanticThreshold, cc.SemanticTopK, cc.SemanticMaxEntries, embed)
			log.Info("semantic cache enabled", "model", cc.SemanticEmbeddingModel, "threshold", cc.SemanticThreshold, "top_k", cc.SemanticTopK)
		}
	}
	log.Info("cache enabled", "ttl", cc.TTL, "max_size", cc.MaxSize, "max_bytes", cc.MaxBytes, "max_ttl", cc.MaxTTL)
	return base
}
