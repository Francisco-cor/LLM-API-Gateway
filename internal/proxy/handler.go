package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/budget"
	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/metrics"
	"github.com/fcordero/llm-api-gateway/internal/pricing"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
	"github.com/fcordero/llm-api-gateway/internal/resilience"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	// maxBodySize accommodates vision/audio payloads. Requests above it are
	// rejected before any provider sees them.
	maxBodySize = 8 << 20 // 8 MiB

	defaultCacheTTL    = 5 * time.Minute
	defaultCacheMaxTTL = 24 * time.Hour

	// streamWriteTimeout is refreshed before every SSE chunk. A healthy stream
	// keeps extending it; a client that stops reading trips it instead of
	// holding a provider connection (and the goroutine) forever.
	streamWriteTimeout = 60 * time.Second
)

// Handler serves POST /v1/chat/completions, routing each request to the
// provider that owns the requested model and falling back through
// fallbackChain on retryable errors. It integrates retry, circuit breaker
// and hedge per Fase 4, and token-aware rate limit + budget per Fase 6,
// and response caching per Fase 7.
// It supports hot-reload via Update* methods protected by mu.
type Handler struct {
	registry *Registry
	log      *slog.Logger

	mu            sync.RWMutex
	fallbackChain []string
	retryCfg      resilience.RetryConfig
	circuitCfg    resilience.CircuitConfig
	circuits      map[string]*resilience.Breaker
	circuitsMu    sync.RWMutex
	hedgeCfg      hedgeConfig
	limiter       ratelimit.Backend
	overrideStore *ratelimit.OverrideStore
	budgetMgr     *budget.Manager
	tokenAware    bool
	tokenRPM      int
	tokenBurst    int
	cache         cache.Cache
	cacheTTL      time.Duration
	cacheMaxTTL   time.Duration

	// flight coalesces concurrent cache misses for the same key. It must live
	// for the handler's lifetime, not per request.
	flight cache.Group
}

type hedgeConfig struct {
	Enabled bool
	Delay   time.Duration
}

// errHedgeUnavailable signals that hedging did not run (fewer than two healthy
// candidates). It is not a provider failure and must not replace lastErr.
var errHedgeUnavailable = errors.New("hedge unavailable: fewer than two healthy fallback candidates")

// NewHandler creates a chat completions Handler with default resilience.
func NewHandler(registry *Registry, fallbackChain []string, log *slog.Logger) *Handler {
	return NewHandlerWithResilience(registry, fallbackChain, log, resilience.DefaultRetryConfig(), resilience.DefaultCircuitConfig(), hedgeConfig{Enabled: false, Delay: 300 * time.Millisecond})
}

// NewHandlerWithResilience allows custom resilience config (from config.yaml).
func NewHandlerWithResilience(registry *Registry, fallbackChain []string, log *slog.Logger, retryCfg resilience.RetryConfig, circuitCfg resilience.CircuitConfig, hedge hedgeConfig) *Handler {
	return NewHandlerWithResilienceAndBudget(registry, fallbackChain, log, retryCfg, circuitCfg, hedge, nil, nil, nil, false)
}

// NewHandlerWithBudget creates a handler with default resilience and a budget
// manager. It keeps budget integration usable without exposing the internal
// hedge configuration type to external packages.
func NewHandlerWithBudget(registry *Registry, fallbackChain []string, log *slog.Logger, budgetMgr *budget.Manager) *Handler {
	return NewHandlerWithResilienceAndBudget(registry, fallbackChain, log, resilience.DefaultRetryConfig(), resilience.DefaultCircuitConfig(), hedgeConfig{}, nil, nil, budgetMgr, false)
}

// NewHandlerWithResilienceAndBudget extends NewHandlerWithResilience with Fase 6 budget and token-aware rate limit.
func NewHandlerWithResilienceAndBudget(registry *Registry, fallbackChain []string, log *slog.Logger, retryCfg resilience.RetryConfig, circuitCfg resilience.CircuitConfig, hedge hedgeConfig, limiter ratelimit.Backend, overrides *ratelimit.OverrideStore, budgetMgr *budget.Manager, tokenAware bool) *Handler {
	return NewHandlerWithCache(registry, fallbackChain, log, retryCfg, circuitCfg, hedge, limiter, overrides, budgetMgr, tokenAware, nil, 0)
}

// NewHandlerWithRateLimit creates a handler with a rate-limit backend and
// token-aware budgeting. It keeps the internal hedge type private while
// giving tests and embedders a way to exercise token limiting.
func NewHandlerWithRateLimit(registry *Registry, fallbackChain []string, log *slog.Logger, limiter ratelimit.Backend, overrides *ratelimit.OverrideStore, budgetMgr *budget.Manager, tokenAware bool) *Handler {
	return NewHandlerWithResilienceAndBudget(registry, fallbackChain, log, resilience.DefaultRetryConfig(), resilience.DefaultCircuitConfig(), hedgeConfig{}, limiter, overrides, budgetMgr, tokenAware)
}

// NewHandlerWithCache extends with Fase 7 cache support.
func NewHandlerWithCache(registry *Registry, fallbackChain []string, log *slog.Logger, retryCfg resilience.RetryConfig, circuitCfg resilience.CircuitConfig, hedge hedgeConfig, limiter ratelimit.Backend, overrides *ratelimit.OverrideStore, budgetMgr *budget.Manager, tokenAware bool, c cache.Cache, ttl time.Duration) *Handler {
	h := &Handler{
		registry:      registry,
		fallbackChain: fallbackChain,
		log:           log,
		retryCfg:      retryCfg,
		circuitCfg:    circuitCfg,
		circuits:      make(map[string]*resilience.Breaker),
		hedgeCfg:      hedge,
		limiter:       limiter,
		overrideStore: overrides,
		budgetMgr:     budgetMgr,
		tokenAware:    tokenAware,
		cache:         c,
		cacheTTL:      ttl,
		cacheMaxTTL:   defaultCacheMaxTTL,
	}
	// pre-create breakers for known providers
	for _, p := range registry.All() {
		h.circuits[p.Name()] = resilience.NewBreaker(circuitCfg)
	}
	return h
}

// Hot-reload helpers (Fase 9) — all protected by mu.

// SetFallbackChain updates fallback chain atomically.
func (h *Handler) SetFallbackChain(chain []string) {
	h.mu.Lock()
	h.fallbackChain = append([]string(nil), chain...)
	h.mu.Unlock()
}

// SetTokenLimits updates the token-per-minute bucket used by token-aware
// limiting. Request RPM and token budgets are different units and must not
// share the request burst capacity.
func (h *Handler) SetTokenLimits(tokensPerMinute, tokenBurst int) {
	h.mu.Lock()
	h.tokenRPM = tokensPerMinute
	h.tokenBurst = tokenBurst
	h.mu.Unlock()
}

// SetRetryConfig updates retry config.
func (h *Handler) SetRetryConfig(cfg resilience.RetryConfig) {
	h.mu.Lock()
	h.retryCfg = cfg
	h.mu.Unlock()
}

// SetCircuitConfig updates circuit breaker thresholds for all breakers and future ones.
func (h *Handler) SetCircuitConfig(cfg resilience.CircuitConfig) {
	h.mu.Lock()
	h.circuitCfg = cfg
	h.mu.Unlock()
	h.circuitsMu.RLock()
	for _, b := range h.circuits {
		b.UpdateConfig(cfg)
	}
	h.circuitsMu.RUnlock()
}

// SetHedgeConfig updates hedge config.
func (h *Handler) SetHedgeConfig(cfg hedgeConfig) {
	h.mu.Lock()
	h.hedgeCfg = cfg
	h.mu.Unlock()
}

// SetHedge updates hedge behaviour without exposing the internal hedgeConfig
// type. main can call it during startup and hot reload; tests use it to
// exercise hedged dispatch from outside this package.
func (h *Handler) SetHedge(enabled bool, delay time.Duration) {
	if delay <= 0 {
		delay = 300 * time.Millisecond
	}
	h.SetHedgeConfig(hedgeConfig{Enabled: enabled, Delay: delay})
}

// SetTokenAware enables or disables the per-request token bucket during a
// configuration reload without rebuilding the HTTP handler chain.
func (h *Handler) SetTokenAware(enabled bool) {
	h.mu.Lock()
	h.tokenAware = enabled
	h.mu.Unlock()
}

func (h *Handler) tokenAwareEnabled() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tokenAware
}

// SetCache updates cache instance and TTL atomically.
func (h *Handler) SetCache(c cache.Cache, ttl time.Duration) {
	h.mu.Lock()
	h.cache = c
	h.cacheTTL = ttl
	h.mu.Unlock()
}

// SetCacheTTL updates only TTL.
func (h *Handler) SetCacheTTL(ttl time.Duration) {
	h.mu.Lock()
	h.cacheTTL = ttl
	h.mu.Unlock()
}

// SetCacheMaxTTL bounds any caller-supplied X-Cache-TTL. main should wire this
// to cache.max_ttl during startup and hot reload. A non-positive value disables
// the cap.
func (h *Handler) SetCacheMaxTTL(max time.Duration) {
	h.mu.Lock()
	h.cacheMaxTTL = max
	h.mu.Unlock()
}

// getFallbackChain returns a copy under RLock.
func (h *Handler) tokenLimits() (int, int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tokenRPM, h.tokenBurst
}

func (h *Handler) getFallbackChain() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]string(nil), h.fallbackChain...)
}

func (h *Handler) getRetryConfig() resilience.RetryConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.retryCfg
}

func (h *Handler) getHedgeConfig() hedgeConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.hedgeCfg
}

func (h *Handler) getCache() (cache.Cache, time.Duration, time.Duration) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	max := h.cacheMaxTTL
	if max == 0 {
		max = defaultCacheMaxTTL
	}
	return h.cache, h.cacheTTL, max
}

func (h *Handler) getCircuitConfig() resilience.CircuitConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.circuitCfg
}

func (h *Handler) breakerFor(name string) *resilience.Breaker {
	h.circuitsMu.RLock()
	b, ok := h.circuits[name]
	h.circuitsMu.RUnlock()
	if ok {
		return b
	}
	h.circuitsMu.Lock()
	defer h.circuitsMu.Unlock()
	if b, ok := h.circuits[name]; ok {
		return b
	}
	cfg := h.getCircuitConfig()
	if cfg.FailureThreshold == 0 {
		cfg = resilience.DefaultCircuitConfig()
	}
	b = resilience.NewBreaker(cfg)
	h.circuits[name] = b
	return b
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large (max 8MiB)")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}

	req, err := decodeChatRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", `"model" field is required`)
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", `"messages" must be a non-empty array`)
		return
	}

	requestID := r.Header.Get("X-Request-ID")
	tenant := r.Header.Get("X-Tenant-ID")
	ctx := r.Context()

	// Token budget: tokens per minute, with a bucket sized in tokens. This is
	// deliberately independent from the request bucket (whose capacity is a
	// number of requests, not tokens).
	if h.limiter != nil && h.tokenAwareEnabled() {
		tokenRPM, tokenBurst := h.tokenLimits()
		estTokens := chatPromptTokens(req)
		if tokenRPM > 0 && tokenBurst > 0 {
			key := clientRateLimitKey(r) + ":tokens"
			if !h.limiter.AllowWithLimits(key, estTokens, tokenRPM, tokenBurst) {
				retryAfter := h.limiter.RetryAfterWithLimits(key, tokenRPM, tokenBurst)
				seconds := int64(retryAfter / time.Second)
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
				writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "token budget exceeded, retry later")
				return
			}
		}
	}

	// Fase 7: cache lookup (only non-streaming 200 responses are cached)
	var cacheKey string
	var cacheTTL time.Duration
	var semanticVector []float32
	var semanticNamespace string
	cached, ttl, maxTTL := h.getCache()
	useCache := cached != nil && !req.Stream && r.Header.Get("X-Cache-Skip") != "true"
	if useCache {
		cacheTTL = h.resolveCacheTTL(r, ttl, maxTTL)
		cacheKey = cache.BuildKeyForIdentity(req, r.Header.Get("Authorization"))
		if data, ok := cached.Get(cacheKey); ok {
			h.serveCacheHit(w, req, requestID, data)
			return
		}
		if semantic, ok := cached.(semanticVectorCache); ok {
			semanticNamespace = cache.BuildSemanticNamespace(req, r.Header.Get("Authorization"))
			data, vector, hit := semantic.LookupWithVector(ctx, semanticNamespace, cache.SemanticQuery(req))
			semanticVector = vector
			if hit {
				h.serveCacheHit(w, req, requestID, data)
				return
			}
		}
		metrics.CacheHits.WithLabelValues("miss").Inc()
	}

	budgetReservation, estimatedTokens, estimatedUSD, err := h.reserveChatBudget(tenant, req)
	if err != nil {
		writeBudgetError(w, err)
		return
	}

	// OTEL span covers both branches: creating it after the streaming return
	// meant streamed requests were invisible to tracing.
	tracer := otel.Tracer("gateway.handler")
	ctx, span := tracer.Start(ctx, "chat.completions")
	span.SetAttributes(
		attribute.String("model", req.Model),
		attribute.String("request_id", requestID),
		attribute.Bool("stream", req.Stream),
	)
	defer span.End()

	if req.Stream {
		started, usage, effectiveModel := h.handleStream(w, r, req, requestID, ctx)
		span.SetAttributes(
			attribute.String("model.effective", effectiveModel),
			attribute.Int("tokens.prompt", usage.PromptTokens),
			attribute.Int("tokens.completion", usage.CompletionTokens),
		)
		h.finishStreamBudget(tenant, budgetReservation, started, usage, effectiveModel, estimatedTokens, estimatedUSD)
		return
	}
	budgetCommitted := false
	defer func() {
		if budgetReservation != nil && !budgetCommitted {
			budgetReservation.Cancel()
		}
	}()

	fetch, err := h.fetchChat(ctx, req, requestID, cached, cacheKey, cacheTTL, semanticNamespace, semanticVector, useCache)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		if errors.Is(err, provider.ErrNoProvider) {
			h.log.Warn("unknown model",
				"model", req.Model,
				"request_id", requestID,
				"error", err,
			)
		} else {
			h.log.Error("dispatch failed",
				"model", req.Model,
				"request_id", requestID,
				"error", err,
			)
		}
		writeDispatchError(w, err)
		return
	}

	if !fetch.executed {
		// This request joined an in-flight miss. The leader already billed and
		// recorded metrics for the provider call; charging the reservation here
		// would double-count the tenant and the cost counters.
		if budgetReservation != nil {
			budgetReservation.Cancel()
			budgetCommitted = true
		}
		h.serveCacheHit(w, req, requestID, fetch.payload)
		return
	}

	resp := fetch.response
	providerName := fetch.provider
	span.SetAttributes(
		attribute.String("provider", providerName),
		attribute.Int("tokens.prompt", resp.Usage.PromptTokens),
		attribute.Int("tokens.completion", resp.Usage.CompletionTokens),
	)

	h.log.Info("chat completion",
		"model", req.Model,
		"provider", providerName,
		"request_id", requestID,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
	)

	// metrics + cache store
	metrics.ObserveTokens(providerName, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	metrics.CircuitState.WithLabelValues(providerName).Set(float64(h.breakerFor(providerName).StateReadOnly()))
	if h.budgetMgr != nil {
		actualUSD, pricingKnown := h.chatCostUSD(providerName, req.Model, resp.Usage)
		metrics.ObserveCost(providerName, req.Model, actualUSD, pricingKnown)
		if budgetReservation != nil {
			if err := budgetReservation.CommitWithRetry(resp.Usage.TotalTokens, actualUSD, 3); err != nil {
				h.log.Warn("budget adjustment exceeded estimate", "tenant", tenant, "error", err)
			}
			budgetCommitted = true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Provider", providerName)
	if fetch.payload != nil {
		if useCache {
			w.Header().Set("X-Cache", "MISS")
			metrics.CacheSize.Set(float64(cached.Stats().Size))
		}
		_, _ = w.Write(fetch.payload)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// resolveCacheTTL picks header override > configured TTL > default, then clamps
// against the configured maximum. Without the cap a client could pin an entry
// for a year with X-Cache-TTL=8760h.
func (h *Handler) resolveCacheTTL(r *http.Request, configured, max time.Duration) time.Duration {
	ttl := configured
	if ttlStr := r.Header.Get("X-Cache-TTL"); ttlStr != "" {
		if d, err := time.ParseDuration(ttlStr); err == nil && d > 0 {
			ttl = d
		}
	}
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	return cache.ClampTTL(ttl, max)
}

// semanticVectorCache is the richer semantic cache API. LookupWithVector and
// SetSemanticVector share one embedding between miss and store; the narrow
// cache.SemanticLookup interface predates that optimization.
type semanticVectorCache interface {
	LookupWithVector(context.Context, string, string) ([]byte, []float32, bool)
	SetSemanticVector(context.Context, string, string, []float32, []byte, time.Duration)
}

// chatFetch is the result of a non-streaming dispatch, possibly shared through
// single-flight. Only the leader (executed=true) performs billing and metrics.
type chatFetch struct {
	response provider.ChatResponse
	provider string
	payload  []byte
	executed bool
}

// fetchChat performs (or joins) the non-streaming provider call. Concurrent
// misses for the same cache key run the provider once; cacheable errors are
// never stored because the single-flight function returns an error instead of
// bytes and LoadOrStore propagates it.
func (h *Handler) fetchChat(ctx context.Context, req provider.ChatRequest, requestID string, c cache.Cache, key string, ttl time.Duration, semanticNamespace string, vector []float32, useCache bool) (chatFetch, error) {
	if c == nil || key == "" || !useCache {
		resp, name, err := h.dispatch(ctx, req, requestID)
		if err != nil {
			return chatFetch{}, err
		}
		return chatFetch{response: resp, provider: name, executed: true}, nil
	}

	var fetch chatFetch
	payload, _, err := h.flight.LoadOrStore(c, key, ttl, func() ([]byte, error) {
		resp, name, dispatchErr := h.dispatch(ctx, req, requestID)
		if dispatchErr != nil {
			return nil, dispatchErr
		}
		data, marshalErr := json.Marshal(resp)
		if marshalErr != nil {
			return nil, marshalErr
		}
		fetch.executed = true
		fetch.response = resp
		fetch.provider = name
		fetch.payload = data
		if semantic, ok := c.(semanticVectorCache); ok && len(vector) > 0 {
			semantic.SetSemanticVector(ctx, semanticNamespace, key, vector, data, ttl)
		}
		return data, nil
	})
	if err != nil {
		return chatFetch{}, err
	}
	fetch.payload = payload
	if fetch.executed {
		return fetch, nil
	}
	fetch.provider = "cache"
	_ = json.Unmarshal(payload, &fetch.response)
	return fetch, nil
}

func (h *Handler) serveCacheHit(w http.ResponseWriter, req provider.ChatRequest, requestID string, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "HIT")
	w.Header().Set("X-Gateway-Provider", "cache")
	metrics.CacheHits.WithLabelValues("hit").Inc()
	// RequestsTotal is owned by the Metrics middleware; incrementing it here
	// counted every cache hit twice.
	key := cache.BuildKey(req)
	if len(key) > 8 {
		key = key[:8]
	}
	h.log.Info("cache hit", "model", req.Model, "request_id", requestID, "cache_key", key)
	_, _ = w.Write(data)
}

func (h *Handler) reserveChatBudget(tenant string, req provider.ChatRequest) (*budget.Reservation, int, float64, error) {
	if h.budgetMgr == nil {
		return nil, 0, 0, nil
	}
	tokens := chatPromptTokens(req)
	completionTokens := 0
	if max := req.EffectiveMaxTokens(); max != nil && *max > 0 {
		completionTokens = *max
	}
	tokens += completionTokens
	estimate, err := h.budgetMgr.EstimateChat(req.Model, tokens-completionTokens, completionTokens)
	if err != nil {
		return nil, 0, 0, err
	}
	usd := estimate.USD
	reservation, err := h.budgetMgr.Reserve(tenant, tokens, usd)
	return reservation, tokens, usd, err
}

// finishStreamBudget settles the streaming reservation with provider-reported
// usage. When the provider never sent a usage block we commit the original
// estimate: the stream may have produced billable tokens, so cancelling would
// under-charge, and we have no better number to use. The warning is deliberate
// so this path is visible in production.
func (h *Handler) finishStreamBudget(tenant string, reservation *budget.Reservation, started bool, usage provider.Usage, effectiveModel string, estimatedTokens int, estimatedUSD float64) {
	if reservation == nil {
		return
	}
	if !started {
		reservation.Cancel()
		return
	}
	tokens := usage.TotalTokens
	usd := estimatedUSD
	if tokens > 0 {
		usd = 0
		if h.budgetMgr != nil {
			usd, _ = h.chatCostUSD("", effectiveModel, usage)
		}
	} else {
		tokens = estimatedTokens
		h.log.Warn("stream ended without provider usage; committing reservation estimate",
			"tenant", tenant,
			"model", effectiveModel,
			"estimated_tokens", estimatedTokens,
		)
	}
	if err := reservation.CommitWithRetry(tokens, usd, 3); err != nil {
		h.log.Warn("stream budget adjustment exceeded estimate", "tenant", tenant, "error", err)
	}
}

// chatCostUSD prices chat usage. Cached prompt tokens are split out so a
// provider cache hit is not billed as fresh input. budget.Manager exposes
// CostForChat but not a cache-aware variant, so the cached slice is priced at
// the rule's input rate: that over-reserves rather than under-bills, and it
// avoids silently losing money when no cache rate exists.
func (h *Handler) chatCostUSD(providerName, model string, usage provider.Usage) (float64, bool) {
	if h.budgetMgr == nil {
		return 0, false
	}
	prompt := usage.PromptTokens
	if prompt == 0 && usage.TotalTokens > 0 {
		prompt = usage.TotalTokens - usage.CompletionTokens
		if prompt < 0 {
			prompt = 0
		}
	}
	cached := 0
	if usage.PromptTokensDetails != nil {
		cached = usage.PromptTokensDetails.CachedTokens
	}
	if cached > prompt {
		cached = prompt
	}
	if cached > 0 {
		base, baseErr := h.budgetMgr.CostForChat(providerName, model, prompt-cached, usage.CompletionTokens)
		cachedEst, cachedErr := h.budgetMgr.CostForChat(providerName, model, cached, 0)
		if baseErr == nil && cachedErr == nil {
			return base.USD + cachedEst.USD, base.Known && cachedEst.Known
		}
	}
	estimate, err := h.budgetMgr.CostForChat(providerName, model, prompt, usage.CompletionTokens)
	if err != nil {
		h.log.Warn("provider/model price unavailable; using fallback estimate", "provider", providerName, "model", model, "error", err)
		return h.budgetMgr.CostForTokens(usage.TotalTokens), false
	}
	return estimate.USD, estimate.Known
}

func chatPromptTokens(req provider.ChatRequest) int {
	chars := 0
	for _, message := range req.Messages {
		chars += len(message.Text())
	}
	if len(req.Tools) > 0 {
		if data, err := json.Marshal(req.Tools); err == nil {
			chars += len(data)
		}
	}
	return ratelimit.EstimateTokens(chars)
}

func writeBudgetError(w http.ResponseWriter, err error) {
	if errors.Is(err, budget.ErrUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "budget_unavailable", "budget service temporarily unavailable")
		return
	}
	if errors.Is(err, pricing.ErrUnknownPrice) {
		writeError(w, http.StatusServiceUnavailable, "pricing_unavailable", "provider/model price is not configured")
		return
	}
	writeError(w, http.StatusTooManyRequests, "insufficient_quota", err.Error())
}

// handleStream proxies SSE and reports whether the response had started, the
// last provider-reported usage, and the model actually sent upstream (after
// fallback remapping). Billing must use that model, not the one the client
// asked for.
func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request, req provider.ChatRequest, requestID string, ctx context.Context) (started bool, usage provider.Usage, effectiveModel string) {
	primary, err := h.registry.Resolve(req.Model)
	if err != nil {
		writeDispatchError(w, err)
		return false, usage, req.Model
	}
	effectiveModel = req.Model

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return false, usage, effectiveModel
	}

	session, err := h.openStream(ctx, req, primary, requestID)
	if err != nil {
		metrics.ProviderErrors.WithLabelValues(primary.Name(), providerErrorCode(err)).Inc()
		propagateRetryAfter(w, err)
		writeDispatchError(w, err)
		return false, usage, effectiveModel
	}
	defer session.cancel()
	effectiveModel = session.model

	streamStart := time.Now()
	defer func() {
		metrics.StreamDuration.WithLabelValues(session.provider.Name()).Observe(time.Since(streamStart).Seconds())
		if usage.TotalTokens > 0 || usage.PromptTokens > 0 {
			metrics.ObserveTokens(session.provider.Name(), usage.PromptTokens, usage.CompletionTokens)
			if h.budgetMgr != nil {
				usd, known := h.chatCostUSD(session.provider.Name(), effectiveModel, usage)
				metrics.ObserveCost(session.provider.Name(), effectiveModel, usd, known)
			}
		}
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Gateway-Provider", session.provider.Name())

	rc := http.NewResponseController(w)
	// http.Server.WriteTimeout caps ordinary responses. A rolling deadline lets
	// a live stream last as long as it keeps producing chunks, while a client
	// that stops reading is disconnected instead of pinning the provider.
	extendDeadline := func() error {
		err := rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		if errors.Is(err, http.ErrNotSupported) {
			return nil
		}
		return err
	}
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	if err := extendDeadline(); err != nil {
		return false, usage, effectiveModel
	}

	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var buf bytes.Buffer
	writeEvent := func(event string, payload []byte) error {
		buf.Reset()
		if event != "" {
			buf.WriteString("event: ")
			buf.WriteString(event)
			buf.WriteByte('\n')
		}
		buf.WriteString("data: ")
		buf.Write(payload)
		buf.WriteString("\n\n")
		// One Write per event: three writes per chunk could interleave with a
		// concurrent writer and produce malformed SSE.
		if err := extendDeadline(); err != nil {
			return err
		}
		if _, err := w.Write(buf.Bytes()); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	writeChunk := func(chunk provider.StreamChunk) error {
		data, marshalErr := json.Marshal(chunk)
		if marshalErr != nil {
			return marshalErr
		}
		return writeEvent("", data)
	}

	terminal := false
	ttftRecorded := false
	recordTTFT := func() {
		if ttftRecorded {
			return
		}
		ttftRecorded = true
		metrics.StreamTTFT.WithLabelValues(session.provider.Name()).Observe(time.Since(streamStart).Seconds())
	}
	observeChunk := func(chunk provider.StreamChunk) {
		if chunk.Usage != nil {
			usage = *chunk.Usage
			terminal = true
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				terminal = true
			}
		}
	}

	if session.first != nil {
		observeChunk(*session.first)
		if err := writeChunk(*session.first); err != nil {
			return true, usage, effectiveModel
		}
		recordTTFT()
	}

	for session.ch != nil || session.errCh != nil {
		select {
		case chunk, ok := <-session.ch:
			if !ok {
				session.ch = nil
				continue
			}
			observeChunk(chunk)
			if err := writeChunk(chunk); err != nil {
				return true, usage, effectiveModel
			}
			recordTTFT()
		case streamErr, ok := <-session.errCh:
			if !ok {
				session.errCh = nil
				continue
			}
			if streamErr == nil {
				continue
			}
			if errors.Is(streamErr, context.Canceled) || ctx.Err() != nil {
				return true, usage, effectiveModel
			}
			metrics.ProviderErrors.WithLabelValues(session.provider.Name(), providerErrorCode(streamErr)).Inc()
			h.log.Error("stream provider error", "provider", session.provider.Name(), "request_id", requestID, "error", streamErr)
			// Emit the OpenAI error event but never [DONE]: [DONE] means the
			// completion finished, and a client that sees it after a truncation
			// records a successful, incomplete answer.
			payload, _ := json.Marshal(errorEnvelope{Error: errorBody{
				Message: sanitizeUpstreamMessage(streamErr.Error()),
				Type:    "provider_error",
				Code:    providerErrorCode(streamErr),
			}})
			_ = writeEvent("error", payload)
			session.cancel()
			return true, usage, effectiveModel
		case <-ctx.Done():
			session.cancel()
			return true, usage, effectiveModel
		}
	}

	if !terminal {
		// Upstream closed without finish_reason or usage: this is a truncated
		// stream, not a successful empty completion.
		err := &provider.ProviderError{
			ProviderName: session.provider.Name(),
			Message:      provider.ErrStreamTruncated.Error(),
			Retryable:    false,
		}
		metrics.ProviderErrors.WithLabelValues(session.provider.Name(), providerErrorCode(err)).Inc()
		h.log.Error("stream truncated before terminal event", "provider", session.provider.Name(), "request_id", requestID)
		payload, _ := json.Marshal(errorEnvelope{Error: errorBody{
			Message: err.Message,
			Type:    "provider_error",
			Code:    "stream_truncated",
		}})
		_ = writeEvent("error", payload)
		return true, usage, effectiveModel
	}

	_ = writeEvent("", []byte("[DONE]"))
	h.log.Info("stream completed", "model", req.Model, "provider", session.provider.Name(), "request_id", requestID)
	return true, usage, effectiveModel
}

// providerErrorCode derives a stable metric/error code from a provider error.
func providerErrorCode(err error) string {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		if pe.StatusCode > 0 {
			return strconv.Itoa(pe.StatusCode)
		}
		if errors.Is(err, provider.ErrStreamTruncated) || pe.Message == provider.ErrStreamTruncated.Error() {
			return "stream_truncated"
		}
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "provider_error"
}

type streamSession struct {
	provider provider.Provider
	model    string
	ch       <-chan provider.StreamChunk
	errCh    <-chan error
	first    *provider.StreamChunk
	cancel   context.CancelFunc
}

// openStream waits for the first upstream event before committing the HTTP
// response. That makes retryable early stream failures eligible for the same
// fallback chain as non-streaming requests; once bytes are sent, a stream
// cannot be transparently moved to another provider.
func (h *Handler) openStream(ctx context.Context, req provider.ChatRequest, primary provider.Provider, requestID string) (*streamSession, error) {
	providers := []provider.Provider{primary}
	for _, name := range h.getFallbackChain() {
		if name == primary.Name() {
			continue
		}
		if p, ok := h.registry.Get(name); ok {
			providers = append(providers, p)
		}
	}

	var lastErr error
	for i, p := range providers {
		b := h.breakerFor(p.Name())
		if !b.Allow() {
			h.log.Warn("stream circuit open, skipping provider", "provider", p.Name(), "request_id", requestID)
			continue
		}
		mapped := req
		if i > 0 {
			mapped = h.registry.RemapForFallback(req, p)
		}
		streamCtx, cancel := context.WithCancel(ctx)
		ch, errCh := p.SendStream(streamCtx, mapped)
		first, err := firstStreamChunk(streamCtx, p.Name(), ch, errCh)
		if err != nil {
			cancel()
			lastErr = err
			h.settleStreamFailure(b, err)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			if !provider.IsRetryable(err) {
				return nil, err
			}
			h.log.Warn("stream provider failed before first chunk, trying fallback", "provider", p.Name(), "request_id", requestID, "error", err)
			continue
		}
		b.RecordSuccess()
		return &streamSession{provider: p, model: mapped.Model, ch: ch, errCh: errCh, first: first, cancel: cancel}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("all providers failed for streaming model %q", req.Model)
}

// settleStreamFailure only feeds the breaker for failures that say something
// about provider health: retryable upstream errors. A client cancellation or a
// 4xx is not the provider's fault and must not open its circuit.
func (h *Handler) settleStreamFailure(b *resilience.Breaker, err error) {
	if b == nil || err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		b.RecordCancel()
		return
	}
	if provider.IsRetryable(err) {
		b.RecordFailure()
		return
	}
	b.Release()
}

// firstStreamChunk waits for either the first chunk or an early error. Closing
// both channels without producing a single chunk is a truncated stream, not a
// successful empty completion.
func firstStreamChunk(ctx context.Context, providerName string, ch <-chan provider.StreamChunk, errCh <-chan error) (*provider.StreamChunk, error) {
	for ch != nil || errCh != nil {
		select {
		case chunk, ok := <-ch:
			if !ok {
				ch = nil
				continue
			}
			return &chunk, nil
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			if err != nil {
				return nil, err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, &provider.ProviderError{
		ProviderName: providerName,
		Message:      provider.ErrStreamTruncated.Error(),
		Retryable:    false,
	}
}

// dispatch sends req to the provider that owns req.Model with retry, circuit breaker and hedge.
// If primary returns retryable error, it tries each fallback in chain, remapping model via aliases.
func (h *Handler) dispatch(ctx context.Context, req provider.ChatRequest, requestID string) (provider.ChatResponse, string, error) {
	var lastErr error
	primary, err := h.registry.Resolve(req.Model)
	if err != nil {
		return provider.ChatResponse{}, "", err
	}

	attempted := map[string]bool{primary.Name(): true}

	// circuit check
	if b := h.breakerFor(primary.Name()); !b.Allow() {
		h.log.Warn("circuit open, skipping primary", "provider", primary.Name(), "request_id", requestID, "state", b.StateReadOnly().String())
	} else {
		resp, err := h.sendWithRetry(ctx, primary, req)
		if err == nil {
			h.breakerFor(primary.Name()).RecordSuccess()
			return resp, primary.Name(), nil
		}
		lastErr = err
		settleBreakerFailure(h.breakerFor(primary.Name()), err)
		if !provider.IsRetryable(err) {
			return provider.ChatResponse{}, "", err
		}
		h.log.Warn("primary provider failed, attempting fallback",
			"provider", primary.Name(),
			"model", req.Model,
			"request_id", requestID,
			"error", err,
		)
	}

	// hedge: if enabled and at least 2 fallbacks, race first fallback after delay
	hedgeCfg := h.getHedgeConfig()
	fallbackChain := h.getFallbackChain()
	if hedgeCfg.Enabled && len(fallbackChain) > 1 {
		resp, name, hedgeAttempted, hedgeErr := h.dispatchHedge(ctx, req, requestID, primary.Name())
		for providerName := range hedgeAttempted {
			attempted[providerName] = true
		}
		if hedgeErr == nil {
			return resp, name, nil
		}
		if !errors.Is(hedgeErr, errHedgeUnavailable) {
			// Keep the real hedge failure instead of masking it with the
			// generic "all providers failed" error below.
			lastErr = hedgeErr
		}
	}

	for _, name := range fallbackChain {
		if attempted[name] {
			continue
		}
		if name == primary.Name() {
			continue
		}
		fallback, ok := h.registry.Get(name)
		if !ok {
			continue
		}
		attempted[name] = true
		if b := h.breakerFor(fallback.Name()); !b.Allow() {
			h.log.Warn("circuit open, skipping fallback", "provider", fallback.Name(), "request_id", requestID, "state", b.StateReadOnly().String())
			continue
		}
		mappedReq := h.registry.RemapForFallback(req, fallback)
		resp, err := h.sendWithRetry(ctx, fallback, mappedReq)
		if err == nil {
			h.breakerFor(fallback.Name()).RecordSuccess()
			h.log.Info("fallback succeeded",
				"provider", fallback.Name(),
				"model", mappedReq.Model,
				"request_id", requestID,
			)
			return resp, fallback.Name(), nil
		}
		lastErr = err
		settleBreakerFailure(h.breakerFor(fallback.Name()), err)
		h.log.Warn("fallback provider failed",
			"provider", fallback.Name(),
			"request_id", requestID,
			"error", err,
		)
		if !provider.IsRetryable(err) {
			// non-retryable, but continue to next fallback (may still succeed with different model)
			continue
		}
	}

	if lastErr != nil {
		return provider.ChatResponse{}, "", lastErr
	}
	return provider.ChatResponse{}, "", fmt.Errorf("all providers failed for model %q", req.Model)
}

// settleBreakerFailure settles a breaker permission using provider-health semantics.
func settleBreakerFailure(b *resilience.Breaker, err error) {
	if b == nil || err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		b.RecordCancel()
		return
	}
	if provider.IsRetryable(err) {
		b.RecordFailure()
		return
	}
	// Client errors (400/404/422) and non-retryable provider rejections do not
	// indicate an unhealthy provider.
	b.Release()
}

func (h *Handler) sendWithRetry(ctx context.Context, p provider.Provider, req provider.ChatRequest) (provider.ChatResponse, error) {
	var resp provider.ChatResponse
	var lastErr error
	retryCfg := h.getRetryConfig()
	err := resilience.Do(ctx, retryCfg, provider.IsRetryable, func() error {
		var err error
		resp, err = p.Send(ctx, req)
		lastErr = err
		return err
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return provider.ChatResponse{}, ctxErr
		}
		if lastErr != nil {
			return provider.ChatResponse{}, lastErr
		}
		return provider.ChatResponse{}, err
	}
	return resp, nil
}

// dispatchHedge races the first two fallback candidates that are not already
// open. Candidates are selected by a read-only state check; Allow() is only
// called by the branch that actually runs, so mere participation in the
// selection cannot consume half-open probe permissions.
func (h *Handler) dispatchHedge(ctx context.Context, req provider.ChatRequest, requestID, primaryName string) (provider.ChatResponse, string, map[string]bool, error) {
	fallbackChain := h.getFallbackChain()
	hedgeCfg := h.getHedgeConfig()
	retryCfg := h.getRetryConfig()
	var candidates []provider.Provider
	for _, name := range fallbackChain {
		if name == primaryName {
			continue
		}
		if p, ok := h.registry.Get(name); ok {
			if h.breakerFor(p.Name()).StateReadOnly() != resilience.StateOpen {
				candidates = append(candidates, p)
			}
		}
		if len(candidates) == 2 {
			break
		}
	}
	if len(candidates) < 2 {
		return provider.ChatResponse{}, "", nil, errHedgeUnavailable
	}
	h.log.Info("hedge enabled, racing fallbacks", "request_id", requestID, "p1", candidates[0].Name(), "p2", candidates[1].Name())

	ctx, cancel := context.WithTimeout(ctx, retryCfg.MaxDelay*2+5*time.Second)
	defer cancel()

	type hedgeResult struct {
		response provider.ChatResponse
		provider string
	}

	var ranMu sync.Mutex
	attempted := make(map[string]bool, 2)
	markRan := func(name string) {
		ranMu.Lock()
		attempted[name] = true
		ranMu.Unlock()
	}

	run := func(branchCtx context.Context, candidate provider.Provider) (any, error) {
		markRan(candidate.Name())
		breaker := h.breakerFor(candidate.Name())
		if !breaker.Allow() {
			return nil, &provider.ProviderError{
				ProviderName: candidate.Name(),
				Message:      "circuit open",
				Retryable:    true,
			}
		}
		mapped := h.registry.RemapForFallback(req, candidate)
		resp, err := h.sendWithRetry(branchCtx, candidate, mapped)
		if err == nil {
			breaker.RecordSuccess()
			return hedgeResult{response: resp, provider: candidate.Name()}, nil
		}
		settleBreakerFailure(breaker, err)
		return nil, err
	}

	val, err := resilience.DoHedgeContext(ctx, hedgeCfg.Delay,
		func(branchCtx context.Context) (any, error) { return run(branchCtx, candidates[0]) },
		func(branchCtx context.Context) (any, error) { return run(branchCtx, candidates[1]) },
	)
	// Snapshot under the lock: a losing branch may still mark itself after the
	// winner returns, and returning the live map would race with that write.
	snapshot := func() map[string]bool {
		ranMu.Lock()
		defer ranMu.Unlock()
		out := make(map[string]bool, len(attempted))
		for name := range attempted {
			out[name] = true
		}
		return out
	}
	if err != nil {
		return provider.ChatResponse{}, "", snapshot(), err
	}
	if result, ok := val.(hedgeResult); ok {
		return result.response, result.provider, snapshot(), nil
	}
	return provider.ChatResponse{}, "", snapshot(), errors.New("hedge returned no result")
}
