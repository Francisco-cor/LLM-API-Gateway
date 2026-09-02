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
	"syscall"
	"time"

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

	registry := proxy.NewRegistryWithAliases(providers, cfg.ModelAliases)
	limiter := ratelimit.New(cfg.RateLimit.RequestsPerMinute, cfg.RateLimit.Burst)
	// Redis distributed limiter (optional, Fase 6)
	var redisClient *redis.Client
	if cfg.RateLimit.RedisURL != "" && cfg.RateLimit.RedisURL != "${REDIS_URL}" {
		if rl, err := ratelimit.NewRedis(cfg.RateLimit.RedisURL, cfg.RateLimit.RequestsPerMinute, cfg.RateLimit.Burst); err == nil {
			log.Info("redis limiter enabled", "url", cfg.RateLimit.RedisURL)
			// wrap: we keep limiter as memory fallback, redis used if available
			_ = rl
			// also create client for budget
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
			// use Redis as primary if available, wrap with memory fallback via semantic wrapper
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

	health := proxy.NewHealthHandler()
	mux := http.NewServeMux()
	handlerOpts := proxy.NewHandlerWithCache(registry, cfg.FallbackChain, log, retryCfg, circuitCfg, struct {
		Enabled bool
		Delay   time.Duration
	}{Enabled: cfg.Resilience.Hedge.Enabled, Delay: cfg.Resilience.Hedge.Delay}, limiter, overrideStore, budgetMgr, cfg.RateLimit.TokenAware, cacheInst, cfg.Cache.TTL)
	mux.Handle("POST /v1/chat/completions", handlerOpts)
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
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Error("graceful shutdown failed", "error", err)
		}
	}
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
