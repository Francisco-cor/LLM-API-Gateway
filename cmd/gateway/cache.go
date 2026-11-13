package main

import (
	"log/slog"

	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
	"github.com/fcordero/llm-api-gateway/internal/redisstore"
)

// cacheConfigOf projects the YAML cache settings onto the reload-comparable
// cache.Config. Redis is part of the identity: moving between memory and Redis
// must rebuild even when the remaining settings match. MaxTTL is intentionally
// not part of the identity: the proxy handlers clamp X-Cache-TTL themselves, so
// changing cache.max_ttl must not throw away a warm cache.
func cacheConfigOf(cfg *config.Config, redisActive bool) cache.Config {
	return cache.Config{
		Enabled:                cfg.Cache.Enabled,
		TTL:                    cfg.Cache.TTL,
		MaxSize:                cfg.Cache.MaxSize,
		MaxBytes:               cfg.Cache.MaxBytes,
		SemanticEnabled:        cfg.Cache.SemanticEnabled,
		SemanticThreshold:      cfg.Cache.SemanticThreshold,
		SemanticEmbeddingModel: cfg.Cache.SemanticEmbeddingModel,
		SemanticTopK:           cfg.Cache.SemanticTopK,
		SemanticMaxEntries:     cfg.Cache.SemanticMaxEntries,
		Redis:                  redisActive,
	}
}

// buildCacheForConfig constructs a cache from an already-computed cache.Config.
// It is the build callback used by cache.BuildIfChanged on reload.
//
// The X-Cache-TTL cap (cache.max_ttl) is not implemented here: the proxy
// handlers clamp it themselves via Handler.SetCacheMaxTTL and
// EmbeddingsHandler.SetCacheMaxTTL, which main wires at startup and on reload.
func buildCacheForConfig(cc cache.Config, redisStore redisstore.Store, registry *proxy.Registry, log *slog.Logger) cache.Cache {
	if !cc.Enabled {
		return nil
	}
	var base cache.Cache = cache.NewMemoryWithBytes(cc.MaxSize, cc.MaxBytes)
	if cc.Redis {
		base = cache.NewRedisWithStore(redisStore)
		log.Info("cache redis enabled", "ttl", cc.TTL)
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
	log.Info("cache enabled", "ttl", cc.TTL, "max_size", cc.MaxSize, "max_bytes", cc.MaxBytes)
	return base
}
