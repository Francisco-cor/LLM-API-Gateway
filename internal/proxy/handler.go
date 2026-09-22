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
	cache         cache.Cache
	cacheTTL      time.Duration
}

type hedgeConfig struct {
	Enabled bool
	Delay   time.Duration
}

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

// getFallbackChain returns a copy under RLock.
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

func (h *Handler) getCache() (cache.Cache, time.Duration) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cache, h.cacheTTL
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

const maxBodySize = 1 << 20 // 1 MiB

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	var req provider.ChatRequest
	if err := decodeSingleJSON(body, &req); err != nil {
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

	// Fase 6: per-tenant/model token override.
	if h.overrideStore != nil && h.limiter != nil {
		rpm, burst := h.overrideStore.Resolve(tenant, req.Model)
		if h.tokenAwareEnabled() {
			chars := 0
			for _, m := range req.Messages {
				chars += len(m.Content)
			}
			estTokens := ratelimit.EstimateTokens(chars)
			key := clientRateLimitKey(r)
			if !h.limiter.AllowWithLimits(key+":tokens", estTokens, rpm, burst) {
				retryAfter := h.limiter.RetryAfterWithLimits(key+":tokens", rpm, burst)
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
	cached, ttl := h.getCache()
	if cached != nil && !req.Stream && r.Header.Get("X-Cache-Skip") != "true" {
		if ttlStr := r.Header.Get("X-Cache-TTL"); ttlStr != "" {
			if d, err := time.ParseDuration(ttlStr); err == nil && d > 0 {
				cacheTTL = d
			}
		}
		if cacheTTL == 0 {
			cacheTTL = ttl
			if cacheTTL == 0 {
				cacheTTL = 5 * time.Minute
			}
		}
		cacheKey = cache.BuildKeyForIdentity(req, r.Header.Get("Authorization"))
		if data, ok := cached.Get(cacheKey); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("X-Gateway-Provider", "cache")
			metrics.CacheHits.WithLabelValues("hit").Inc()
			metrics.RequestsTotal.WithLabelValues(r.Method, r.URL.Path, "200", "cache").Inc()
			_, _ = w.Write(data)
			h.log.Info("cache hit", "model", req.Model, "request_id", requestID, "cache_key", cacheKey[:8])
			return
		}
		if semantic, ok := cached.(cache.SemanticLookup); ok {
			namespace := cache.BuildSemanticNamespace(req, r.Header.Get("Authorization"))
			if data, ok := semantic.Lookup(ctx, namespace, cache.SemanticQuery(req)); ok {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Cache", "HIT")
				w.Header().Set("X-Gateway-Provider", "cache-semantic")
				metrics.CacheHits.WithLabelValues("hit").Inc()
				metrics.CacheSize.Set(float64(cached.Stats().Size))
				_, _ = w.Write(data)
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

	if req.Stream {
		started := h.handleStream(w, r, req, requestID, ctx)
		if budgetReservation != nil {
			if started {
				if err := budgetReservation.Commit(estimatedTokens, estimatedUSD); err != nil {
					h.log.Warn("stream budget adjustment exceeded estimate", "tenant", tenant, "error", err)
				}
			} else {
				budgetReservation.Cancel()
			}
		}
		return
	}
	budgetCommitted := false
	defer func() {
		if budgetReservation != nil && !budgetCommitted {
			budgetReservation.Cancel()
		}
	}()

	// OTEL span per chat completion
	tracer := otel.Tracer("gateway.handler")
	ctx, span := tracer.Start(ctx, "chat.completions")
	span.SetAttributes(
		attribute.String("model", req.Model),
		attribute.String("request_id", requestID),
	)
	defer span.End()

	resp, providerName, err := h.dispatch(ctx, req, requestID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		if errors.Is(err, provider.ErrNoProvider) {
			h.log.Warn("unknown model",
				"model", req.Model,
				"request_id", requestID,
				"error", err,
			)
			writeError(w, http.StatusNotFound, "model_not_found", err.Error())
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "provider_error", "request timed out")
			return
		}
		h.log.Error("dispatch failed",
			"model", req.Model,
			"request_id", requestID,
			"error", err,
		)
		propagateRetryAfter(w, err)
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}

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
	metrics.CircuitState.WithLabelValues(providerName).Set(float64(h.breakerFor(providerName).State()))
	if h.budgetMgr != nil {
		actualUSD := h.budgetMgr.CostForTokens(resp.Usage.TotalTokens)
		pricingKnown := false
		if estimate, err := h.budgetMgr.CostForChat(providerName, req.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens); err == nil {
			actualUSD = estimate.USD
			pricingKnown = estimate.Known
		} else {
			h.log.Warn("provider/model price unavailable after dispatch; using fallback estimate", "provider", providerName, "model", req.Model, "error", err)
		}
		metrics.ObserveCost(providerName, req.Model, actualUSD, pricingKnown)
		if budgetReservation != nil {
			if err := budgetReservation.Commit(resp.Usage.TotalTokens, actualUSD); err != nil {
				h.log.Warn("budget adjustment exceeded estimate", "tenant", tenant, "error", err)
			}
			budgetCommitted = true
		}
	}
	// Fase 7: cache store (only cache successful non-streaming)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Provider", providerName)
	cch, _ := h.getCache()
	if cch != nil && cacheKey != "" {
		data, _ := json.Marshal(resp)
		if semantic, ok := cch.(cache.SemanticLookup); ok {
			semantic.SetSemantic(ctx, cache.BuildSemanticNamespace(req, r.Header.Get("Authorization")), cache.SemanticQuery(req), cacheKey, data, cacheTTL)
		} else {
			cch.Set(cacheKey, data, cacheTTL)
		}
		w.Header().Set("X-Cache", "MISS")
		metrics.CacheSize.Set(float64(cch.Stats().Size))
		_, _ = w.Write(data)
		return
	} else if cch != nil {
		w.Header().Set("X-Cache", "MISS")
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) reserveChatBudget(tenant string, req provider.ChatRequest) (*budget.Reservation, int, float64, error) {
	if h.budgetMgr == nil {
		return nil, 0, 0, nil
	}
	tokens := chatPromptTokens(req)
	completionTokens := 0
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		completionTokens = *req.MaxTokens
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

func chatPromptTokens(req provider.ChatRequest) int {
	chars := 0
	for _, message := range req.Messages {
		chars += len(message.Content)
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

// decodeSingleJSON accepts exactly one JSON value and rejects trailing data.
// Without the second Decode, bodies such as {"...":...}{"...":...} were
// silently accepted, which is surprising for clients and proxies.
func decodeSingleJSON(body []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

// propagateRetryAfter writes Retry-After header if err is retryable ProviderError with RetryAfter set.
func propagateRetryAfter(w http.ResponseWriter, err error) {
	var pe *provider.ProviderError
	if errors.As(err, &pe) && pe.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(pe.RetryAfter.Seconds())))
	} else if errors.As(err, &pe) && pe.StatusCode == 429 {
		// default 1s if provider was rate limited but no Retry-After parsed
		w.Header().Set("Retry-After", "1")
	}
}

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request, req provider.ChatRequest, requestID string, ctx context.Context) bool {
	primary, err := h.registry.Resolve(req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return false
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return false
	}

	session, err := h.openStream(ctx, req, primary, requestID)
	if err != nil {
		propagateRetryAfter(w, err)
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return false
	}
	defer session.cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Gateway-Provider", session.provider.Name())
	// http.Server.WriteTimeout is useful for ordinary responses but otherwise
	// caps valid long-lived SSE sessions. Clear the per-response deadline after
	// the first upstream chunk proves that this is an active stream.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	writeChunk := func(chunk provider.StreamChunk) {
		data, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(data)
		_, _ = w.Write([]byte("\n\n"))
		flusher.Flush()
	}
	if session.first != nil {
		writeChunk(*session.first)
	}

	for {
		select {
		case chunk, ok := <-session.ch:
			if !ok {
				session.ch = nil
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				flusher.Flush()
				h.log.Info("stream completed", "model", req.Model, "provider", session.provider.Name(), "request_id", requestID)
				return true
			}
			writeChunk(chunk)
		case streamErr, ok := <-session.errCh:
			session.errCh = nil
			if ok && streamErr != nil {
				h.log.Error("stream provider error", "provider", session.provider.Name(), "request_id", requestID, "error", streamErr)
				_, _ = w.Write([]byte("event: error\ndata: "))
				data, _ := json.Marshal(provider.ErrorResponse{Error: provider.Error{
					Message: streamErr.Error(), Type: "provider_error", Code: "provider_error",
				}})
				_, _ = w.Write(data)
				_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
				flusher.Flush()
				return true
			}
		case <-r.Context().Done():
			return true
		}
		if session.ch == nil && session.errCh == nil {
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return true
		}
	}
}

type streamSession struct {
	provider provider.Provider
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
		first, err := firstStreamChunk(streamCtx, ch, errCh)
		if err != nil {
			cancel()
			lastErr = err
			b.RecordFailure()
			if !provider.IsRetryable(err) {
				return nil, err
			}
			h.log.Warn("stream provider failed before first chunk, trying fallback", "provider", p.Name(), "request_id", requestID, "error", err)
			continue
		}
		b.RecordSuccess()
		return &streamSession{provider: p, ch: ch, errCh: errCh, first: first, cancel: cancel}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("all providers failed for streaming model %q", req.Model)
}

func firstStreamChunk(ctx context.Context, ch <-chan provider.StreamChunk, errCh <-chan error) (*provider.StreamChunk, error) {
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
	return nil, nil
}

// dispatch sends req to the provider that owns req.Model with retry, circuit breaker and hedge.
// If primary returns retryable error, it tries each fallback in chain, remapping model via aliases.
func (h *Handler) dispatch(ctx context.Context, req provider.ChatRequest, requestID string) (provider.ChatResponse, string, error) {
	var lastErr error
	primary, err := h.registry.Resolve(req.Model)
	if err != nil {
		return provider.ChatResponse{}, "", err
	}

	// circuit check
	if b := h.breakerFor(primary.Name()); !b.Allow() {
		h.log.Warn("circuit open, skipping primary", "provider", primary.Name(), "request_id", requestID, "state", b.State().String())
	} else {
		resp, err := h.sendWithRetry(ctx, primary, req)
		if err == nil {
			h.breakerFor(primary.Name()).RecordSuccess()
			return resp, primary.Name(), nil
		}
		lastErr = err
		h.breakerFor(primary.Name()).RecordFailure()
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
		if resp, name, ok := h.dispatchHedge(ctx, req, requestID, primary.Name()); ok {
			return resp, name, nil
		}
	}

	for _, name := range fallbackChain {
		if name == primary.Name() {
			continue
		}
		fallback, ok := h.registry.Get(name)
		if !ok {
			continue
		}
		if b := h.breakerFor(fallback.Name()); !b.Allow() {
			h.log.Warn("circuit open, skipping fallback", "provider", fallback.Name(), "request_id", requestID, "state", b.State().String())
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
		h.breakerFor(fallback.Name()).RecordFailure()
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

func (h *Handler) dispatchHedge(ctx context.Context, req provider.ChatRequest, requestID, primaryName string) (provider.ChatResponse, string, bool) {
	// pick first two fallbacks
	fallbackChain := h.getFallbackChain()
	hedgeCfg := h.getHedgeConfig()
	retryCfg := h.getRetryConfig()
	var candidates []provider.Provider
	for _, name := range fallbackChain {
		if name == primaryName {
			continue
		}
		if p, ok := h.registry.Get(name); ok && h.breakerFor(p.Name()).Allow() {
			candidates = append(candidates, p)
		}
		if len(candidates) == 2 {
			break
		}
	}
	if len(candidates) < 2 {
		return provider.ChatResponse{}, "", false
	}
	h.log.Info("hedge enabled, racing fallbacks", "request_id", requestID, "p1", candidates[0].Name(), "p2", candidates[1].Name())
	ctx, cancel := context.WithTimeout(ctx, retryCfg.MaxDelay*2+5*time.Second)
	defer cancel()
	type hedgeResult struct {
		response provider.ChatResponse
		provider string
	}
	val, err := resilience.DoHedge(ctx, hedgeCfg.Delay,
		func() (any, error) {
			mapped := h.registry.RemapForFallback(req, candidates[0])
			resp, e := h.sendWithRetry(ctx, candidates[0], mapped)
			if e == nil {
				h.breakerFor(candidates[0].Name()).RecordSuccess()
				return hedgeResult{response: resp, provider: candidates[0].Name()}, nil
			}
			h.breakerFor(candidates[0].Name()).RecordFailure()
			return nil, e
		},
		func() (any, error) {
			mapped := h.registry.RemapForFallback(req, candidates[1])
			resp, e := h.sendWithRetry(ctx, candidates[1], mapped)
			if e == nil {
				h.breakerFor(candidates[1].Name()).RecordSuccess()
				return hedgeResult{response: resp, provider: candidates[1].Name()}, nil
			}
			h.breakerFor(candidates[1].Name()).RecordFailure()
			return nil, e
		},
	)
	if err != nil {
		return provider.ChatResponse{}, "", false
	}
	if result, ok := val.(hedgeResult); ok {
		return result.response, result.provider, true
	}
	return provider.ChatResponse{}, "", false
}
