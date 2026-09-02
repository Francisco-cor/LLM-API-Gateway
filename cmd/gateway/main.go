package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/admin"
	"github.com/fcordero/llm-api-gateway/internal/auth"
	"github.com/fcordero/llm-api-gateway/internal/budget"
	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/logger"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
	"github.com/fcordero/llm-api-gateway/internal/resilience"
	"github.com/fcordero/llm-api-gateway/internal/tracing"
	"github.com/redis/go-redis/v9"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	log := logger.New(cfg.Logging.Level, cfg.Logging.Format)

	// tracing init (Fase 5) — noop if OTEL env not set
	shutdownTracing, err := tracing.Init("llm-api-gateway")
	if err != nil {
		log.Warn("tracing init failed", "error", err)
	} else {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownTracing(ctx)
		}()
	}

	providers, err := buildProviders(cfg, log)
	if err != nil {
		log.Error("provider setup failed", "error", err)
		os.Exit(1)
	}

	// auto-discovery (Fase 8): if provider models empty, fetch upstream
	providers = discoverModels(providers, log)

	// build weighted routing map for Fase 8
	weighted := buildWeighted(cfg, providers)
	var registry *proxy.Registry
	if len(weighted) > 0 {
		registry = proxy.NewRegistryWithWeighted(providers, cfg.ModelAliases, weighted)
	} else {
		registry = proxy.NewRegistryWithAliases(providers, cfg.ModelAliases)
	}
	registry.SetRandSeed(time.Now().UnixNano())
	limiter := ratelimit.New(cfg.RateLimit.RequestsPerMinute, cfg.RateLimit.Burst)
	// Redis distributed limiter (optional, Fase 6)
	var redisClient *redis.Client
	if cfg.RateLimit.RedisURL != "" && cfg.RateLimit.RedisURL != "${REDIS_URL}" {
		if rl, err := ratelimit.NewRedis(cfg.RateLimit.RedisURL, cfg.RateLimit.RequestsPerMinute, cfg.RateLimit.Burst); err == nil {
			log.Info("redis limiter enabled", "url", cfg.RateLimit.RedisURL)
			_ = rl
			opts, _ := redis.ParseURL(cfg.RateLimit.RedisURL)
			redisClient = redis.NewClient(opts)
		} else {
			log.Warn("redis limiter failed, using memory", "error", err)
		}
	}
	authStore := auth.New(cfg.Auth.Keys)

	// Budget manager (Fase 6)
	var budgetMgr *budget.Manager
	if cfg.RateLimit.Budget != nil && cfg.RateLimit.Budget.Enabled {
		budgetMgr = budget.New(cfg.RateLimit.Budget.MonthlyTokens, cfg.RateLimit.Budget.MonthlyUSD, redisClient)
		log.Info("budget enabled", "tokens", cfg.RateLimit.Budget.MonthlyTokens, "usd", cfg.RateLimit.Budget.MonthlyUSD)
	}
	overrideStore := ratelimit.NewOverrideStore(cfg.RateLimit)

	// Cache (Fase 7)
	var cacheInst cache.Cache
	if cfg.Cache.Enabled {
		var baseCache cache.Cache = cache.NewMemory(cfg.Cache.MaxSize)
		if redisClient != nil {
			baseCache = cache.NewRedis(redisClient)
			log.Info("cache redis enabled", "ttl", cfg.Cache.TTL)
		}
		if cfg.Cache.SemanticEnabled {
			baseCache = cache.NewSemantic(baseCache, true, cfg.Cache.SemanticThreshold)
			log.Info("semantic cache enabled", "threshold", cfg.Cache.SemanticThreshold)
		}
		cacheInst = baseCache
		log.Info("cache enabled", "ttl", cfg.Cache.TTL, "max_size", cfg.Cache.MaxSize)
	}

	// Build resilience configs from YAML
	retryCfg := resilience.RetryConfig{
		MaxAttempts: cfg.Resilience.Retry.MaxAttempts,
		BaseDelay:   cfg.Resilience.Retry.BaseDelay,
		MaxDelay:    cfg.Resilience.Retry.MaxDelay,
		Jitter:      true,
	}
	circuitCfg := resilience.CircuitConfig{
		FailureThreshold: cfg.Resilience.Circuit.FailureThreshold,
		OpenTimeout:      cfg.Resilience.Circuit.OpenTimeout,
		HalfOpenMax:      1,
	}
	hedgeCfg := struct {
		Enabled bool
		Delay   time.Duration
	}{Enabled: cfg.Resilience.Hedge.Enabled, Delay: cfg.Resilience.Hedge.Delay}

	health := proxy.NewHealthHandler()
	mux := http.NewServeMux()
	handlerOpts := proxy.NewHandlerWithCache(registry, cfg.FallbackChain, log, retryCfg, circuitCfg, hedgeCfg, limiter, overrideStore, budgetMgr, cfg.RateLimit.TokenAware, cacheInst, cfg.Cache.TTL)
	embedHandler := proxy.NewEmbeddingsHandler(registry, cfg.FallbackChain, log)
	mux.Handle("POST /v1/chat/completions", handlerOpts)
	mux.Handle("POST /v1/embeddings", embedHandler)
	mux.Handle("GET /v1/models", proxy.NewModelsHandler(registry))
	mux.Handle("GET /health", health)
	mux.Handle("GET /health/providers", proxy.NewHealthProvidersHandler(registry))
	mux.Handle("GET /livez", proxy.NewLivenessHandler(health))
	mux.Handle("GET /readyz", proxy.NewReadinessHandler(registry))
	mux.Handle("GET /metrics", proxy.NewMetricsHandler())

	var handler http.Handler = mux
	if cfg.Auth.Enabled {
		handler = auth.Middleware(authStore, handler)
		log.Info("auth enabled", "keys", len(cfg.Auth.Keys))
	}
	if cfg.RateLimit.Enabled {
		handler = proxy.RateLimit(limiter, handler)
	}
	if len(cfg.CORS.AllowedOrigins) > 0 {
		handler = proxy.CORS(cfg.CORS.AllowedOrigins)(handler)
	}
	handler = proxy.Metrics(handler)
	handler = proxy.Tracing("llm-api-gateway")(handler)
	handler = proxy.SecurityHeaders(handler)
	handler = proxy.Logging(log, handler)
	handler = proxy.RequestID(handler)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           handler,
		ReadTimeout:       cfg.Server.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       120 * time.Second,
	}

	// --- Fase 9: Control Plane & hot-reload ---
	var (
		currentCfg = cfg
		cfgMu      sync.RWMutex
		applyMu    sync.Mutex
		adminSrv   *admin.Server
		adminHTTP  *http.Server
	)

	// applyConfig is the central hot-reload entry: validates, rebuilds, and swaps live state.
	// It is used by admin API, SIGHUP, and file watcher. It holds applyMu to serialize.
	applyConfig := func(newCfg *config.Config) error {
		applyMu.Lock()
		defer applyMu.Unlock()

		if err := newCfg.Validate(); err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}
		// rebuild providers
		newProviders, err := buildProviders(newCfg, log)
		if err != nil {
			return fmt.Errorf("provider build failed: %w", err)
		}
		newProviders = discoverModels(newProviders, log)
		newWeighted := buildWeighted(newCfg, newProviders)

		// check weighted references are valid (already validated)
		// atomically update registry in place
		registry.Reload(newProviders, newCfg.ModelAliases, newWeighted)

		// update limiter and overrides
		limiter.UpdateLimits(newCfg.RateLimit.RequestsPerMinute, newCfg.RateLimit.Burst)
		overrideStore.Reload(newCfg.RateLimit)

		// auth
		authStore.Reload(newCfg.Auth.Keys)

		// cache: handle enable/disable and TTL (recreate if enabled)
		var newCache cache.Cache
		newCacheTTL := newCfg.Cache.TTL
		if newCfg.Cache.Enabled {
			var base cache.Cache = cache.NewMemory(newCfg.Cache.MaxSize)
			if redisClient != nil {
				base = cache.NewRedis(redisClient)
			}
			if newCfg.Cache.SemanticEnabled {
				base = cache.NewSemantic(base, true, newCfg.Cache.SemanticThreshold)
			}
			newCache = base
		} else {
			newCache = nil
		}
		handlerOpts.SetCache(newCache, newCacheTTL)
		cacheInst = newCache

		// resilience
		newRetryCfg := resilience.RetryConfig{
			MaxAttempts: newCfg.Resilience.Retry.MaxAttempts,
			BaseDelay:   newCfg.Resilience.Retry.BaseDelay,
			MaxDelay:    newCfg.Resilience.Retry.MaxDelay,
			Jitter:      true,
		}
		newCircuitCfg := resilience.CircuitConfig{
			FailureThreshold: newCfg.Resilience.Circuit.FailureThreshold,
			OpenTimeout:      newCfg.Resilience.Circuit.OpenTimeout,
			HalfOpenMax:      1,
		}
		newHedgeCfg := struct {
			Enabled bool
			Delay   time.Duration
		}{Enabled: newCfg.Resilience.Hedge.Enabled, Delay: newCfg.Resilience.Hedge.Delay}
		handlerOpts.SetRetryConfig(newRetryCfg)
		handlerOpts.SetCircuitConfig(newCircuitCfg)
		handlerOpts.SetHedgeConfig(newHedgeCfg)
		handlerOpts.SetFallbackChain(newCfg.FallbackChain)
		embedHandler.SetFallbackChain(newCfg.FallbackChain)

		// update currentCfg
		cfgMu.Lock()
		currentCfg = newCfg
		cfgMu.Unlock()

		// sync admin server
		if adminSrv != nil {
			adminSrv.SetConfig(newCfg.Clone())
		}

		log.Info("config reloaded", "providers", len(newProviders), "fallback", newCfg.FallbackChain)
		return nil
	}

	// admin server (Fase 9) on :8081
	adminAddr := fmt.Sprintf(":%d", cfg.Admin.Port)
	adminAPIKey := cfg.Admin.APIKey
	if adminAPIKey == "" {
		adminAPIKey = os.Getenv("ADMIN_API_KEY")
	}
	adminSrv = admin.New(log, adminAPIKey, *configPath, currentCfg, registry, applyConfig, nil)
	adminHTTP = &http.Server{
		Addr:    adminAddr,
		Handler: adminSrv.Handler(),
	}
	go func() {
		log.Info("admin listening", "addr", adminHTTP.Addr)
		if err := adminHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("admin server error", "error", err)
		}
	}()

	// file watcher (polling)
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go func() {
		if err := config.Watch(watchCtx, *configPath, time.Second, func(newCfg *config.Config) error {
			log.Info("file watcher detected change, reloading")
			if err := applyConfig(newCfg); err != nil {
				log.Error("watch reload failed, keeping old config", "error", err)
				return err
			}
			return nil
		}); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("watch error", "error", err)
		}
	}()

	// SIGHUP handler
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	go func() {
		for range hupCh {
			log.Info("SIGHUP received, reloading config")
			newCfg, err := config.Load(*configPath)
			if err != nil {
				log.Error("SIGHUP reload failed", "error", err)
				continue
			}
			if err := applyConfig(newCfg); err != nil {
				log.Error("SIGHUP apply failed", "error", err)
				continue
			}
			log.Info("SIGHUP reload succeeded")
		}
	}()

	serverErr := make(chan error, 1)
	go func() {
		log.Info("gateway listening", "addr", srv.Addr)
		serverErr <- srv.ListenAndServe()
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	case <-quit:
		log.Info("shutting down")
		// shutdown admin first
		ctxA, cancelA := context.WithTimeout(context.Background(), 5*time.Second)
		_ = adminHTTP.Shutdown(ctxA)
		cancelA()
		watchCancel()
		signal.Stop(hupCh)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Error("graceful shutdown failed", "error", err)
		}
	}
}

func buildWeighted(cfg *config.Config, providers []provider.Provider) map[string][]proxy.WeightedConfig {
	// helper to filter weighted entries to only known providers
	known := make(map[string]bool, len(providers))
	for _, p := range providers {
		known[p.Name()] = true
	}
	weighted := make(map[string][]proxy.WeightedConfig)
	for model, entries := range cfg.Routing.Weighted {
		var we []proxy.WeightedConfig
		for _, e := range entries {
			name := e.ProviderName()
			if !known[name] {
				continue
			}
			we = append(we, proxy.WeightedConfig{
				Provider: name,
				Weight:   e.Weight,
			})
		}
		if len(we) > 0 {
			weighted[model] = we
		}
	}
	return weighted
}

// buildProviders constructs a Provider for each configured backend that has
// a non-empty API key.
func buildProviders(cfg *config.Config, log *slog.Logger) ([]provider.Provider, error) {
	isDisabled := func(key string) bool {
		return key == "" || (len(key) > 3 && key[:2] == "${")
	}
	var providers []provider.Provider

	if pc, ok := cfg.Providers["openai"]; ok && !isDisabled(pc.APIKey) {
		providers = append(providers, provider.NewOpenAI(pc.APIKey, pc.BaseURL, pc.Timeout, pc.Models))
		log.Info("provider configured", "name", "openai", "models", pc.Models)
	}
	if pc, ok := cfg.Providers["anthropic"]; ok && !isDisabled(pc.APIKey) {
		providers = append(providers, provider.NewAnthropic(pc.APIKey, pc.BaseURL, pc.Timeout, pc.Models))
		log.Info("provider configured", "name", "anthropic", "models", pc.Models)
	}
	if pc, ok := cfg.Providers["gemini"]; ok && !isDisabled(pc.APIKey) {
		providers = append(providers, provider.NewGemini(pc.APIKey, pc.BaseURL, pc.Timeout, pc.Models))
		log.Info("provider configured", "name", "gemini", "models", pc.Models)
	}

	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers configured: set at least one of OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY")
	}
	return providers, nil
}

type modelDiscoverer interface {
	DiscoverModels(ctx context.Context) ([]string, error)
	SetModels(models []string)
}

// discoverModels attempts to auto-discover models for providers with empty model list (Fase 8).
func discoverModels(providers []provider.Provider, log *slog.Logger) []provider.Provider {
	for i, p := range providers {
		if len(p.Models()) > 0 {
			continue
		}
		// try to discover
		if d, ok := p.(modelDiscoverer); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			models, err := d.DiscoverModels(ctx)
			cancel()
			if err != nil {
				log.Warn("auto-discovery failed, provider has no models", "provider", p.Name(), "error", err)
				continue
			}
			if len(models) == 0 {
				log.Warn("auto-discovery returned no models", "provider", p.Name())
				continue
			}
			log.Info("auto-discovered models", "provider", p.Name(), "models", models)
			d.SetModels(models)
			providers[i] = p
		}
	}
	return providers
}
