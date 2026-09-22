package proxy

import (
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
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
	"github.com/fcordero/llm-api-gateway/internal/resilience"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// EmbeddingsHandler serves POST /v1/embeddings (OpenAI-compatible).
// It routes to the provider that owns the requested embedding model and falls
// back on retryable errors, translating Gemini embedContent format.
// Supports hot-reload via mutex-protected fallbackChain.
type EmbeddingsHandler struct {
	registry *Registry
	log      *slog.Logger

	mu            sync.RWMutex
	fallbackChain []string
	limiter       ratelimit.Backend
	overrides     *ratelimit.OverrideStore
	tokenAware    bool
	retryCfg      resilience.RetryConfig
	circuitCfg    resilience.CircuitConfig
	circuits      map[string]*resilience.Breaker
	circuitsMu    sync.RWMutex
	budgetMgr     *budget.Manager
	cache         cache.Cache
	cacheTTL      time.Duration
}

func NewEmbeddingsHandler(registry *Registry, fallbackChain []string, log *slog.Logger) *EmbeddingsHandler {
	return NewEmbeddingsHandlerWithRateLimit(registry, fallbackChain, log, nil, nil, false)
}

func NewEmbeddingsHandlerWithRateLimit(registry *Registry, fallbackChain []string, log *slog.Logger, limiter ratelimit.Backend, overrides *ratelimit.OverrideStore, tokenAware bool) *EmbeddingsHandler {
	return NewEmbeddingsHandlerWithResilienceAndBudget(registry, fallbackChain, log, limiter, overrides, tokenAware, resilience.DefaultRetryConfig(), resilience.DefaultCircuitConfig(), nil)
}

func NewEmbeddingsHandlerWithResilienceAndBudget(registry *Registry, fallbackChain []string, log *slog.Logger, limiter ratelimit.Backend, overrides *ratelimit.OverrideStore, tokenAware bool, retryCfg resilience.RetryConfig, circuitCfg resilience.CircuitConfig, budgetMgr *budget.Manager) *EmbeddingsHandler {
	return NewEmbeddingsHandlerWithCache(registry, fallbackChain, log, limiter, overrides, tokenAware, retryCfg, circuitCfg, budgetMgr, nil, 0)
}

func NewEmbeddingsHandlerWithCache(registry *Registry, fallbackChain []string, log *slog.Logger, limiter ratelimit.Backend, overrides *ratelimit.OverrideStore, tokenAware bool, retryCfg resilience.RetryConfig, circuitCfg resilience.CircuitConfig, budgetMgr *budget.Manager, c cache.Cache, ttl time.Duration) *EmbeddingsHandler {
	circuits := make(map[string]*resilience.Breaker)
	for _, p := range registry.All() {
		circuits[p.Name()] = resilience.NewBreaker(circuitCfg)
	}
	return &EmbeddingsHandler{
		registry:      registry,
		fallbackChain: append([]string(nil), fallbackChain...),
		log:           log,
		limiter:       limiter,
		overrides:     overrides,
		tokenAware:    tokenAware,
		retryCfg:      retryCfg,
		circuitCfg:    circuitCfg,
		circuits:      circuits,
		budgetMgr:     budgetMgr,
		cache:         c,
		cacheTTL:      ttl,
	}
}

func (h *EmbeddingsHandler) SetCache(c cache.Cache, ttl time.Duration) {
	h.mu.Lock()
	h.cache = c
	h.cacheTTL = ttl
	h.mu.Unlock()
}

func (h *EmbeddingsHandler) getCache() (cache.Cache, time.Duration) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cache, h.cacheTTL
}

func (h *EmbeddingsHandler) SetFallbackChain(chain []string) {
	h.mu.Lock()
	h.fallbackChain = append([]string(nil), chain...)
	h.mu.Unlock()
}

func (h *EmbeddingsHandler) getFallbackChain() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]string(nil), h.fallbackChain...)
}

func (h *EmbeddingsHandler) SetTokenAware(enabled bool) {
	h.mu.Lock()
	h.tokenAware = enabled
	h.mu.Unlock()
}

func (h *EmbeddingsHandler) tokenAwareEnabled() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tokenAware
}

func (h *EmbeddingsHandler) SetResilience(retryCfg resilience.RetryConfig, circuitCfg resilience.CircuitConfig) {
	h.mu.Lock()
	h.retryCfg = retryCfg
	h.circuitCfg = circuitCfg
	h.mu.Unlock()
	h.circuitsMu.Lock()
	defer h.circuitsMu.Unlock()
	for name, breaker := range h.circuits {
		breaker.UpdateConfig(circuitCfg)
		if _, ok := h.registry.Get(name); !ok {
			delete(h.circuits, name)
		}
	}
	for _, p := range h.registry.All() {
		if _, ok := h.circuits[p.Name()]; !ok {
			h.circuits[p.Name()] = resilience.NewBreaker(circuitCfg)
		}
	}
}

func (h *EmbeddingsHandler) getRetryConfig() resilience.RetryConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.retryCfg
}

func (h *EmbeddingsHandler) breakerFor(name string) *resilience.Breaker {
	h.circuitsMu.RLock()
	b := h.circuits[name]
	h.circuitsMu.RUnlock()
	if b != nil {
		return b
	}
	h.circuitsMu.Lock()
	defer h.circuitsMu.Unlock()
	if b = h.circuits[name]; b == nil {
		h.mu.RLock()
		circuitCfg := h.circuitCfg
		h.mu.RUnlock()
		b = resilience.NewBreaker(circuitCfg)
		h.circuits[name] = b
	}
	return b
}

func (h *EmbeddingsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large (max 1MiB)")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}
	var req provider.EmbeddingRequest
	if err := decodeSingleJSON(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", `"model" field is required`)
		return
	}
	if req.Input == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", `"input" field is required`)
		return
	}
	// normalize input check: empty string or empty array
	switch v := req.Input.(type) {
	case string:
		if v == "" {
			writeError(w, http.StatusBadRequest, "invalid_request_error", `"input" must be non-empty`)
			return
		}
	case []any:
		if len(v) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", `"input" must be non-empty`)
			return
		}
	case []string:
		if len(v) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", `"input" must be non-empty`)
			return
		}
	}
	var cacheKey string
	var cacheTTL time.Duration
	cached, ttl := h.getCache()
	if cached != nil && r.Header.Get("X-Cache-Skip") != "true" {
		if ttlHeader := r.Header.Get("X-Cache-TTL"); ttlHeader != "" {
			if d, parseErr := time.ParseDuration(ttlHeader); parseErr == nil && d > 0 {
				cacheTTL = d
			}
		}
		if cacheTTL == 0 {
			cacheTTL = ttl
			if cacheTTL == 0 {
				cacheTTL = 5 * time.Minute
			}
		}
		cacheKey = cache.BuildEmbeddingKeyForIdentity(req, r.Header.Get("Authorization"))
		if data, ok := cached.Get(cacheKey); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("X-Gateway-Provider", "cache")
			metrics.CacheHits.WithLabelValues("hit").Inc()
			metrics.CacheSize.Set(float64(cached.Stats().Size))
			_, _ = w.Write(data)
			return
		}
		metrics.CacheHits.WithLabelValues("miss").Inc()
	}
	if h.limiter != nil && h.overrides != nil && h.tokenAwareEnabled() {
		tenant := r.Header.Get("X-Tenant-ID")
		rpm, burst := h.overrides.Resolve(tenant, req.Model)
		estTokens := ratelimit.EstimateTokens(embeddingInputChars(req.Input))
		key := clientRateLimitKey(r) + ":tokens"
		if !h.limiter.AllowWithLimits(key, estTokens, rpm, burst) {
			retryAfter := h.limiter.RetryAfterWithLimits(key, rpm, burst)
			seconds := int64(retryAfter / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
			writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "token budget exceeded, retry later")
			return
		}
	}

	estimatedTokens := ratelimit.EstimateTokens(embeddingInputChars(req.Input))
	var budgetReservation *budget.Reservation
	if h.budgetMgr != nil {
		var err error
		providerName := ""
		if primary, resolveErr := h.registry.Resolve(req.Model); resolveErr == nil {
			providerName = primary.Name()
		}
		estimate, estimateErr := h.budgetMgr.CostForEmbedding(providerName, req.Model, estimatedTokens)
		if estimateErr != nil {
			writeBudgetError(w, estimateErr)
			return
		}
		budgetReservation, err = h.budgetMgr.Reserve(r.Header.Get("X-Tenant-ID"), estimatedTokens, estimate.USD)
		if err != nil {
			writeBudgetError(w, err)
			return
		}
	}
	budgetCommitted := false
	defer func() {
		if budgetReservation != nil && !budgetCommitted {
			budgetReservation.Cancel()
		}
	}()

	requestID := r.Header.Get("X-Request-ID")
	ctx := r.Context()

	tracer := otel.Tracer("gateway.embeddings")
	ctx, span := tracer.Start(ctx, "embeddings")
	span.SetAttributes(attribute.String("model", req.Model), attribute.String("request_id", requestID))
	defer span.End()

	resp, provName, err := h.dispatch(ctx, req, requestID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		if errors.Is(err, provider.ErrNoProvider) {
			writeError(w, http.StatusNotFound, "model_not_found", err.Error())
			return
		}
		h.log.Error("embeddings dispatch failed", "model", req.Model, "request_id", requestID, "error", err)
		propagateRetryAfter(w, err)
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	if h.budgetMgr != nil {
		estimate, estimateErr := h.budgetMgr.CostForEmbedding(provName, req.Model, resp.Usage.TotalTokens)
		if estimateErr != nil {
			h.log.Warn("embedding price unavailable after dispatch; using fallback estimate", "provider", provName, "model", req.Model, "error", estimateErr)
			estimate.USD = h.budgetMgr.CostForTokens(resp.Usage.TotalTokens)
		}
		metrics.ObserveTokens(provName, resp.Usage.TotalTokens, 0)
		metrics.ObserveCost(provName, req.Model, estimate.USD, estimate.Known)
		if budgetReservation != nil {
			if err := budgetReservation.Commit(resp.Usage.TotalTokens, estimate.USD); err != nil {
				h.log.Warn("embedding budget adjustment exceeded estimate", "tenant", r.Header.Get("X-Tenant-ID"), "error", err)
			}
			budgetCommitted = true
		}
	}
	span.SetAttributes(attribute.String("provider", provName))
	h.log.Info("embeddings", "model", req.Model, "provider", provName, "request_id", requestID, "input_count", len(resp.Data))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Provider", provName)
	if cached != nil && cacheKey != "" {
		data, marshalErr := json.Marshal(resp)
		if marshalErr == nil {
			cached.Set(cacheKey, data, cacheTTL)
			w.Header().Set("X-Cache", "MISS")
			metrics.CacheSize.Set(float64(cached.Stats().Size))
			_, _ = w.Write(data)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func embeddingInputChars(input any) int {
	switch v := input.(type) {
	case string:
		return len(v)
	case []string:
		chars := 0
		for _, s := range v {
			chars += len(s)
		}
		return chars
	case []any:
		chars := 0
		for _, item := range v {
			if s, ok := item.(string); ok {
				chars += len(s)
			}
		}
		return chars
	default:
		return 0
	}
}

func (h *EmbeddingsHandler) dispatch(ctx context.Context, req provider.EmbeddingRequest, requestID string) (provider.EmbeddingResponse, string, error) {
	var lastErr error
	// try primary
	primary, err := h.registry.Resolve(req.Model)
	if err != nil {
		return provider.EmbeddingResponse{}, "", err
	}
	if embedder, ok := primary.(provider.Embedder); ok && h.breakerFor(primary.Name()).Allow() {
		resp, err := h.embedWithRetry(ctx, embedder, req)
		if err == nil {
			h.breakerFor(primary.Name()).RecordSuccess()
			return resp, primary.Name(), nil
		}
		h.breakerFor(primary.Name()).RecordFailure()
		lastErr = err
		if !provider.IsRetryable(err) {
			return provider.EmbeddingResponse{}, "", err
		}
		h.log.Warn("embeddings primary failed, attempting fallback", "provider", primary.Name(), "model", req.Model, "request_id", requestID, "error", err)
	} else {
		// provider doesn't support embeddings, treat as retryable to try fallback
		lastErr = fmt.Errorf("provider %s does not support embeddings", primary.Name())
		h.log.Warn("provider does not support embeddings, trying fallback", "provider", primary.Name(), "request_id", requestID)
	}

	fallbackChain := h.getFallbackChain()
	for _, name := range fallbackChain {
		if name == primary.Name() {
			continue
		}
		fb, ok := h.registry.Get(name)
		if !ok {
			continue
		}
		embedder, ok := fb.(provider.Embedder)
		if !ok {
			continue
		}
		// remap model via aliases if needed (reuse chat remap logic by constructing dummy ChatRequest)
		mappedModel := req.Model
		dummy := provider.ChatRequest{Model: req.Model}
		remapped := h.registry.RemapForFallback(dummy, fb)
		if remapped.Model != req.Model {
			mappedModel = remapped.Model
		}
		mappedReq := req
		mappedReq.Model = mappedModel
		breaker := h.breakerFor(fb.Name())
		if !breaker.Allow() {
			continue
		}
		resp, err := h.embedWithRetry(ctx, embedder, mappedReq)
		if err == nil {
			breaker.RecordSuccess()
			h.log.Info("embeddings fallback succeeded", "provider", fb.Name(), "model", mappedReq.Model, "request_id", requestID)
			return resp, fb.Name(), nil
		}
		breaker.RecordFailure()
		lastErr = err
		h.log.Warn("embeddings fallback failed", "provider", fb.Name(), "request_id", requestID, "error", err)
		if !provider.IsRetryable(err) {
			continue
		}
	}
	if lastErr != nil {
		return provider.EmbeddingResponse{}, "", lastErr
	}
	return provider.EmbeddingResponse{}, "", errors.New("all providers failed for embeddings model " + req.Model)
}

func (h *EmbeddingsHandler) embedWithRetry(ctx context.Context, embedder provider.Embedder, req provider.EmbeddingRequest) (provider.EmbeddingResponse, error) {
	var response provider.EmbeddingResponse
	err := resilience.Do(ctx, h.getRetryConfig(), provider.IsRetryable, func() error {
		var err error
		response, err = embedder.Embed(ctx, req)
		return err
	})
	return response, err
}
