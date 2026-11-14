package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/admin"
	"github.com/fcordero/llm-api-gateway/internal/auth"
	"github.com/fcordero/llm-api-gateway/internal/budget"
	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/capability"
	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/logger"
	"github.com/fcordero/llm-api-gateway/internal/pricing"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
	"github.com/fcordero/llm-api-gateway/internal/redisstore"
	"github.com/fcordero/llm-api-gateway/internal/resilience"
	"github.com/fcordero/llm-api-gateway/internal/tracing"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	strictFlag := flag.Bool("strict", false, "apply strict production validation (also enabled by GATEWAY_ENV=prod)")
	flag.Parse()

	strictMode := *strictFlag || strings.EqualFold(os.Getenv("GATEWAY_ENV"), "prod")

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if strictMode {
		if err := cfg.ValidateStrict(); err != nil {
			fmt.Fprintf(os.Stderr, "strict config validation: %v\n", err)
			os.Exit(1)
		}
	}

	log, logCtl := logger.NewControlled(cfg.Logging.Level, cfg.Logging.Format)

	// tracing init (Fase 5) — noop if OTEL env not set
	shutdownTracing, tracingErr := tracing.Init("llm-api-gateway")
	if tracingErr != nil {
		log.Warn("tracing init failed; continuing with local tracing", "error", tracingErr)
	}
	if shutdownTracing != nil {
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
	capabilities := buildCapabilityTables(cfg)
	var registry *proxy.Registry
	if len(weighted) > 0 {
		registry = proxy.NewRegistryWithCapabilities(providers, cfg.ModelAliases, weighted, capabilities)
	} else {
		registry = proxy.NewRegistryWithCapabilities(providers, cfg.ModelAliases, nil, capabilities)
	}
	registry.SetRandSeed(time.Now().UnixNano())
	// Redis is shared by rate-limit, budget, and cache. Its manager can swap
	// connections during a config reload while waiting for active operations.
	redisStore := redisstore.New()
	if err := redisStore.ReplaceURL(cfg.RateLimit.RedisURL); err != nil {
		log.Warn("redis backend unavailable, using local fallbacks", "error", err)
	} else if redisStore.URL() != "" {
		log.Info("redis backend enabled")
	}
	limiter := ratelimit.NewRedisWithStore(redisStore, cfg.RateLimit.RequestsPerMinute, cfg.RateLimit.Burst)
	defer func() { _ = limiter.Close() }()
	defer func() { _ = redisStore.Close() }()
	authStore := auth.NewWithPepper(cfg.Auth.Keys, cfg.Auth.KeyPepper)

	// Budget manager (Fase 6): keep the manager installed while disabled so a
	// config reload can enable limits without replacing request handlers.
	pricingCatalog, err := buildPricingCatalog(cfg)
	if err != nil {
		log.Error("pricing setup failed", "error", err)
		os.Exit(1)
	}
	budgetCfg := cfg.RateLimit.Budget
	budgetOpts := budget.Options{Store: redisStore}
	budgetEnabled := false
	budgetTokens, budgetUSD, budgetCostPerToken := 0, 0.0, 0.0
	if budgetCfg != nil {
		budgetEnabled = budgetCfg.Enabled
		budgetTokens = budgetCfg.MonthlyTokens
		budgetUSD = budgetCfg.MonthlyUSD
		budgetCostPerToken = budgetCfg.CostPerTokenUSD
		budgetOpts.RequireSharedStore = budgetCfg.RequireSharedStore
		budgetOpts.ReservationTTL = budgetCfg.ReservationTTL
	}
	budgetMgr := budget.NewConfiguredWithStoreOptions(budgetEnabled, budgetTokens, budgetUSD, budgetCostPerToken, pricingCatalog, budgetOpts)
	if budgetCfg != nil && budgetCfg.Enabled {
		log.Info("budget enabled", "tokens", budgetCfg.MonthlyTokens, "usd", budgetCfg.MonthlyUSD, "pricing_rules", len(budgetCfg.Pricing.Catalog), "require_shared_store", budgetCfg.RequireSharedStore)
	}
	warnBudgetReplicas(cfg, log)
	overrideStore := ratelimit.NewOverrideStore(cfg.RateLimit)

	// Cache (Fase 7). The cache.Config snapshot lets a reload keep the warm
	// cache when nothing relevant changed instead of wiping it on every SIGHUP.
	redisActive := redisStore.Active()
	cacheCfg := cacheConfigOf(cfg, redisActive)
	cacheInst := buildCacheForConfig(cacheCfg, redisStore, registry, log)

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
	healthOptions := buildHealthOptions(cfg)
	readiness := proxy.NewReadinessHandlerWithOptions(registry, health, healthOptions)
	healthProviders := proxy.NewHealthProvidersHandlerWithOptions(registry, healthOptions)
	mux := http.NewServeMux()
	rateLimitEnabled := &atomic.Bool{}
	rateLimitEnabled.Store(cfg.RateLimit.Enabled)
	authEnabled := &atomic.Bool{}
	authEnabled.Store(cfg.Auth.Enabled)
	cors := newDynamicCORS(cfg.CORS.AllowedOrigins)
	handlerOpts := proxy.NewHandlerWithCache(registry, cfg.FallbackChain, log, retryCfg, circuitCfg, hedgeCfg, limiter, overrideStore, budgetMgr, cfg.RateLimit.Enabled && cfg.RateLimit.TokenAware, cacheInst, cfg.Cache.TTL)
	embedHandler := proxy.NewEmbeddingsHandlerWithCache(registry, cfg.FallbackChain, log, limiter, overrideStore, cfg.RateLimit.Enabled && cfg.RateLimit.TokenAware, retryCfg, circuitCfg, budgetMgr, cacheInst, cfg.Cache.TTL)
	handlerOpts.SetTokenLimits(cfg.RateLimit.TokensPerMinute, cfg.RateLimit.TokenBurst)
	embedHandler.SetTokenLimits(cfg.RateLimit.TokensPerMinute, cfg.RateLimit.TokenBurst)
	handlerOpts.SetCacheMaxTTL(cfg.Cache.MaxTTL)
	embedHandler.SetCacheMaxTTL(cfg.Cache.MaxTTL)
	mux.Handle("POST /v1/chat/completions", handlerOpts)
	mux.Handle("POST /v1/embeddings", embedHandler)
	mux.Handle("GET /v1/models", proxy.NewModelsHandler(registry))
	mux.Handle("GET /health", health)
	mux.Handle("GET /health/providers", healthProviders)
	mux.Handle("GET /livez", proxy.NewLivenessHandler(health))
	mux.Handle("GET /readyz", readiness)
	mux.Handle("GET /metrics", proxy.NewMetricsHandler())

	var handler http.Handler = mux
	// Keep this wrapper installed so admin/SIGHUP hot-reload can toggle the
	// feature without rebuilding the HTTP server.
	handler = proxy.RateLimitWithEnabled(limiter, overrideStore, rateLimitEnabled, handler)
	// Auth and CORS wrappers are always installed and read their flags/policy
	// atomically. Enabling them via SIGHUP/admin now takes effect immediately.
	handler = auth.MiddlewareWithEnabled(authEnabled, authStore, handler)
	handler = cors.Wrap(handler)
	if cfg.Auth.Enabled {
		log.Info("auth enabled", "keys", len(cfg.Auth.Keys), "pepper", cfg.Auth.KeyPepper != "")
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
	// BaseContext lets shutdown cancel long-lived handlers (SSE) once the
	// drain grace period expires, so they do not hold the process open.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv.BaseContext = func(net.Listener) context.Context { return baseCtx }

	// --- Fase 9: Control Plane & hot-reload ---
	var (
		currentCfg = cfg
		cfgMu      sync.RWMutex
		applyMu    sync.Mutex
		adminSrv   *admin.Server
		adminHTTP  *http.Server
	)
	// Tenants with active auth keys. The budget reconciler walks this list so
	// expired reservations are reclaimed even for tenants with no traffic.
	budgetTenants := &atomic.Value{}
	budgetTenants.Store(tenantsOf(cfg))

	// applyConfig is the central hot-reload entry: validates, rebuilds, and swaps live state.
	// It is used by admin API, SIGHUP, and file watcher. It holds applyMu to serialize.
	applyConfig := func(newCfg *config.Config) error {
		applyMu.Lock()
		defer applyMu.Unlock()

		if err := newCfg.Validate(); err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}
		if strictMode {
			if err := newCfg.ValidateStrict(); err != nil {
				return fmt.Errorf("strict validation failed: %w", err)
			}
		}
		cfgMu.RLock()
		oldCfg := currentCfg
		cfgMu.RUnlock()
		if err := newCfg.ValidateReload(oldCfg); err != nil {
			return err
		}
		// rebuild providers
		newProviders, err := buildProviders(newCfg, log)
		if err != nil {
			return fmt.Errorf("provider build failed: %w", err)
		}
		newProviders = discoverModels(newProviders, log)
		newWeighted := buildWeighted(newCfg, newProviders)
		newPricingCatalog, err := buildPricingCatalog(newCfg)
		if err != nil {
			return fmt.Errorf("pricing setup failed: %w", err)
		}

		// Redis: only touch the connection when the URL actually changed (or
		// when an earlier attempt left it inactive). A transient Redis outage
		// must not block key rotation or any other reload step; keep the
		// previous backend and let the budget fail closed if configured to.
		if newCfg.RateLimit.RedisURL != oldCfg.RateLimit.RedisURL || (newCfg.RateLimit.RedisURL != "" && !redisStore.Active()) {
			if err := redisStore.ReplaceURL(newCfg.RateLimit.RedisURL); err != nil {
				log.Warn("redis backend update failed; keeping previous backend", "error", err)
			}
		}

		// atomically update registry in place
		registry.ReloadWithCapabilities(newProviders, newCfg.ModelAliases, newWeighted, buildCapabilityTables(newCfg))

		// update limiter and overrides
		limiter.UpdateLimits(newCfg.RateLimit.RequestsPerMinute, newCfg.RateLimit.Burst)
		overrideStore.Reload(newCfg.RateLimit)
		rateLimitEnabled.Store(newCfg.RateLimit.Enabled)
		newBudgetCfg := newCfg.RateLimit.Budget
		if newBudgetCfg == nil {
			budgetMgr.UpdateConfig(false, 0, 0, 0, newPricingCatalog)
			budgetMgr.SetRequireSharedStore(false)
			budgetMgr.SetReservationTTL(0)
		} else {
			budgetMgr.UpdateConfig(newBudgetCfg.Enabled, newBudgetCfg.MonthlyTokens, newBudgetCfg.MonthlyUSD, newBudgetCfg.CostPerTokenUSD, newPricingCatalog)
			budgetMgr.SetRequireSharedStore(newBudgetCfg.RequireSharedStore)
			budgetMgr.SetReservationTTL(newBudgetCfg.ReservationTTL)
		}
		budgetTenants.Store(tenantsOf(newCfg))
		warnBudgetReplicas(newCfg, log)

		// auth: swap keys+pepper and the enabled flag. The middleware is
		// already installed, so enabling auth now takes effect immediately.
		authStore.ReloadWithPepper(newCfg.Auth.Keys, newCfg.Auth.KeyPepper)
		authWasEnabled := authEnabled.Swap(newCfg.Auth.Enabled)
		if authWasEnabled != newCfg.Auth.Enabled {
			log.Info("auth state changed", "enabled", newCfg.Auth.Enabled, "keys", len(newCfg.Auth.Keys))
		}

		// CORS policy
		cors.Set(newCfg.CORS.AllowedOrigins)

		// Logging: level is reloadable, format is not. Rejecting the format
		// change with a warning keeps the current handler and the reload.
		if err := logCtl.SetLevel(newCfg.Logging.Level); err != nil {
			return fmt.Errorf("logging.level: %w", err)
		}
		if err := logCtl.SetFormat(newCfg.Logging.Format); err != nil {
			log.Warn("logging.format change ignored; restart required", "error", err, "current", oldCfg.Logging.Format, "requested", newCfg.Logging.Format)
		}

		healthOptions = buildHealthOptions(newCfg)
		readiness.SetOptions(healthOptions)
		healthProviders.SetOptions(healthOptions)

		// cache: rebuild only when a relevant setting changed. A TTL or limit
		// that did not change keeps the warm cache (and its Redis contents).
		newRedisActive := redisStore.Active()
		newCacheCfg := cacheConfigOf(newCfg, newRedisActive)
		newCache := cache.BuildIfChanged(cacheInst, cacheCfg, newCacheCfg, func(cc cache.Config) cache.Cache {
			return buildCacheForConfig(cc, redisStore, registry, log)
		})
		handlerOpts.SetCache(newCache, newCfg.Cache.TTL)
		embedHandler.SetCache(newCache, newCfg.Cache.TTL)
		handlerOpts.SetCacheMaxTTL(newCfg.Cache.MaxTTL)
		embedHandler.SetCacheMaxTTL(newCfg.Cache.MaxTTL)
		cacheInst = newCache
		cacheCfg = newCacheCfg

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
		handlerOpts.SetTokenAware(newCfg.RateLimit.Enabled && newCfg.RateLimit.TokenAware)
		embedHandler.SetTokenAware(newCfg.RateLimit.Enabled && newCfg.RateLimit.TokenAware)
		handlerOpts.SetTokenLimits(newCfg.RateLimit.TokensPerMinute, newCfg.RateLimit.TokenBurst)
		embedHandler.SetTokenLimits(newCfg.RateLimit.TokensPerMinute, newCfg.RateLimit.TokenBurst)
		embedHandler.SetResilience(newRetryCfg, newCircuitCfg)
		handlerOpts.SetFallbackChain(newCfg.FallbackChain)
		embedHandler.SetFallbackChain(newCfg.FallbackChain)

		// update currentCfg
		cfgMu.Lock()
		currentCfg = newCfg
		cfgMu.Unlock()

		// sync admin server: the key rotates live; bind/port/pprof were
		// rejected by ValidateReload above.
		if adminSrv != nil {
			adminSrv.SetAPIKey(adminAPIKey(newCfg))
			adminSrv.SetRequireAuth(!newCfg.Admin.AllowInsecure)
			adminSrv.SetConfig(newCfg.Clone())
		}

		log.Info("config reloaded", "providers", len(newProviders), "fallback", newCfg.FallbackChain, "auth", newCfg.Auth.Enabled)
		return nil
	}

	// Budget reservation hygiene: reclaim leases that were never committed.
	// One minute is frequent enough to keep quota accurate without hammering
	// Redis (the operation is a no-op for tenants without stale leases).
	reconcileCtx, reconcileCancel := context.WithCancel(context.Background())
	defer reconcileCancel()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-reconcileCtx.Done():
				return
			case <-ticker.C:
				tenants, _ := budgetTenants.Load().([]string)
				for _, tenant := range tenants {
					if _, err := budgetMgr.Reconcile(tenant); err != nil {
						log.Warn("budget reconcile failed", "tenant", tenant, "error", err)
					}
				}
			}
		}
	}()

	// admin server (Fase 9) on the configured admin bind address (loopback by default)
	if cfg.Admin.AllowInsecure {
		log.Warn("admin API running without authentication (admin.allow_insecure=true); bind is restricted to localhost", "addr", cfg.Admin.BindAddress)
	}
	adminSrv = admin.New(log, adminAPIKey(cfg), *configPath, currentCfg, registry, applyConfig, nil)
	adminSrv.SetPprofEnabled(cfg.Admin.PprofEnabled)
	adminSrv.SetRequireAuth(!cfg.Admin.AllowInsecure)
	adminHTTP = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Admin.BindAddress, cfg.Admin.Port),
		Handler:           adminSrv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Info("admin listening", "addr", adminHTTP.Addr, "pprof", cfg.Admin.PprofEnabled)
		if err := adminHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("admin server error", "error", err)
		}
	}()

	// file watcher (polling). Errors are retried with backoff by config.Watch;
	// WatchWithErrorHandler surfaces them for logging.
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go func() {
		if err := config.WatchWithErrorHandler(watchCtx, *configPath, time.Second, func(newCfg *config.Config) error {
			log.Info("file watcher detected change, reloading")
			if err := applyConfig(newCfg); err != nil {
				log.Error("watch reload failed, keeping old config", "error", err)
				return err
			}
			return nil
		}, func(err error) {
			log.Warn("config watcher retrying", "error", err)
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
		log.Info("shutting down, draining")
		// Fase 10: mark as not ready so /readyz fails and K8s stops routing before drain
		health.SetReady(false)
		// Give kube-proxy / load balancer time to remove the endpoint (5s).
		// This is a pre-drain wait, not a second drain sleep.
		time.Sleep(5 * time.Second)
		// shutdown admin and pprof first
		ctxA, cancelA := context.WithTimeout(context.Background(), 5*time.Second)
		_ = adminHTTP.Shutdown(ctxA)
		cancelA()
		watchCancel()
		reconcileCancel()
		signal.Stop(hupCh)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- srv.Shutdown(ctx) }()
		select {
		case err := <-done:
			cancelBase()
			if err != nil {
				log.Error("graceful shutdown failed", "error", err)
			}
		case <-time.After(5 * time.Second):
			// SSE and long-poll handlers only observe cancellation through the
			// request context. Cancel BaseContext after the drain grace so they
			// can emit a terminal error and return instead of holding shutdown.
			log.Warn("drain grace elapsed; cancelling in-flight requests")
			cancelBase()
			if err := <-done; err != nil && !errors.Is(err, context.DeadlineExceeded) {
				log.Error("graceful shutdown failed", "error", err)
			}
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

func buildPricingCatalog(cfg *config.Config) (*pricing.Catalog, error) {
	if cfg.RateLimit.Budget == nil {
		// cost_per_token_usd is expressed per token (0.00001 = 10 USD per 1M).
		return pricing.NewCatalogWithFallbackPerToken(nil, 0.00001, pricing.DefaultCurrency, "", pricing.UnknownUseFallback)
	}
	p := cfg.RateLimit.Budget.Pricing
	rules := make([]pricing.Rule, 0, len(p.Catalog))
	for _, rule := range p.Catalog {
		rules = append(rules, pricing.Rule{
			Provider:                      rule.Provider,
			Model:                         rule.Model,
			InputPerMillionTokensUSD:      rule.InputPerMillionTokensUSD,
			OutputPerMillionTokensUSD:     rule.OutputPerMillionTokensUSD,
			EmbeddingPerMillionTokensUSD:  rule.EmbeddingPerMillionTokensUSD,
			CacheReadPerMillionTokensUSD:  rule.CacheReadPerMillionTokensUSD,
			CacheWritePerMillionTokensUSD: rule.CacheWritePerMillionTokensUSD,
		})
	}
	// The fallback is stored as USD per token; the per-million constructor
	// avoids under-pricing unknown models by a factor of 1,000,000.
	return pricing.NewCatalogWithFallbackPerToken(rules, cfg.RateLimit.Budget.CostPerTokenUSD, p.Currency, p.Version, p.UnknownModelPolicy)
}

func buildHealthOptions(cfg *config.Config) proxy.HealthOptions {
	return proxy.HealthOptions{
		ReadinessCacheTTL:   cfg.Health.ReadinessCacheTTL,
		CheckTimeout:        cfg.Health.CheckTimeout,
		ProviderTimeout:     cfg.Health.ProviderTimeout,
		SkipExpensiveChecks: cfg.Health.SkipExpensiveChecks,
	}
}

func adminAPIKey(cfg *config.Config) string {
	if cfg.Admin.APIKey != "" {
		return cfg.Admin.APIKey
	}
	return os.Getenv("ADMIN_API_KEY")
}

// tenantsOf returns the distinct non-empty tenants with configured auth keys.
func tenantsOf(cfg *config.Config) []string {
	seen := make(map[string]bool, len(cfg.Auth.Keys))
	var out []string
	for _, k := range cfg.Auth.Keys {
		if k.Tenant == "" || seen[k.Tenant] {
			continue
		}
		seen[k.Tenant] = true
		out = append(out, k.Tenant)
	}
	return out
}

// replicaCount reads GATEWAY_REPLICAS (set by the deployment manifest). It is
// best effort: a missing value assumes a single replica.
func replicaCount() int {
	n, err := strconv.Atoi(os.Getenv("GATEWAY_REPLICAS"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// warnBudgetReplicas makes the dangerous multi-replica budget mode visible.
// Per-process accounting multiplies a monthly quota by the replica count, so
// operators must either enable require_shared_store or accept the risk.
func warnBudgetReplicas(cfg *config.Config, log *slog.Logger) {
	b := cfg.RateLimit.Budget
	if b == nil || !b.Enabled || b.RequireSharedStore {
		return
	}
	if replicas := replicaCount(); replicas > 1 {
		log.Warn("budget enabled without require_shared_store in a multi-replica deployment; each replica enforces its own quota",
			"replicas", replicas)
	}
}

func semanticEmbedder(registry *proxy.Registry, model string) cache.EmbeddingFunc {
	if registry == nil || model == "" {
		return nil
	}
	return func(ctx context.Context, text string) ([]float32, error) {
		p, err := registry.Resolve(model)
		if err != nil {
			return nil, err
		}
		embedder, ok := p.(provider.Embedder)
		if !ok {
			return nil, fmt.Errorf("provider %s does not support embeddings", p.Name())
		}
		resp, err := embedder.Embed(ctx, provider.EmbeddingRequest{Model: model, Input: text})
		if err != nil {
			return nil, err
		}
		if len(resp.Data) == 0 || len(resp.Data[0].Embedding) == 0 {
			return nil, fmt.Errorf("provider returned an empty embedding")
		}
		return resp.Data[0].Embedding, nil
	}
}

// buildCapabilityTables converts providers[].capabilities into per-provider
// tables. A table with only a kind still enables provider-kind defaults.
func buildCapabilityTables(cfg *config.Config) map[string]capability.Table {
	if len(cfg.Providers) == 0 {
		return nil
	}
	tables := make(map[string]capability.Table, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		tables[name] = capability.NewTable(name, pc.Capabilities)
	}
	return tables
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
