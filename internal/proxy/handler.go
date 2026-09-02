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
type Handler struct {
	registry      *Registry
	fallbackChain []string
	log           *slog.Logger

	retryCfg       resilience.RetryConfig
	circuits       map[string]*resilience.Breaker
	circuitsMu     sync.RWMutex
	hedgeCfg       hedgeConfig
	limiter        *ratelimit.Limiter
	overrideStore  *ratelimit.OverrideStore
	budgetMgr      *budget.Manager
	tokenAware     bool
	cache          cache.Cache
	cacheTTL       time.Duration
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

// NewHandlerWithResilienceAndBudget extends NewHandlerWithResilience with Fase 6 budget and token-aware rate limit.
func NewHandlerWithResilienceAndBudget(registry *Registry, fallbackChain []string, log *slog.Logger, retryCfg resilience.RetryConfig, circuitCfg resilience.CircuitConfig, hedge hedgeConfig, limiter *ratelimit.Limiter, overrides *ratelimit.OverrideStore, budgetMgr *budget.Manager, tokenAware bool) *Handler {
	return NewHandlerWithCache(registry, fallbackChain, log, retryCfg, circuitCfg, hedge, limiter, overrides, budgetMgr, tokenAware, nil, 0)
}

// NewHandlerWithCache extends with Fase 7 cache support.
func NewHandlerWithCache(registry *Registry, fallbackChain []string, log *slog.Logger, retryCfg resilience.RetryConfig, circuitCfg resilience.CircuitConfig, hedge hedgeConfig, limiter *ratelimit.Limiter, overrides *ratelimit.OverrideStore, budgetMgr *budget.Manager, tokenAware bool, c cache.Cache, ttl time.Duration) *Handler {
	h := &Handler{
		registry:      registry,
		fallbackChain: fallbackChain,
		log:           log,
		retryCfg:      retryCfg,
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
	b = resilience.NewBreaker(resilience.DefaultCircuitConfig())
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
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
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

	// Fase 6: per-tenant/model override + budget check
	if h.overrideStore != nil && h.limiter != nil {
		rpm, burst := h.overrideStore.Resolve(tenant, req.Model)
		_ = rpm
		_ = burst
		if h.tokenAware {
			chars := 0
			for _, m := range req.Messages {
				chars += len(m.Content)
			}
			estTokens := ratelimit.EstimateTokens(chars)
			key := r.Header.Get("Authorization")
			if key == "" {
				key = r.RemoteAddr
			}
			if tenant != "" {
				key = tenant
			}
			if !h.limiter.AllowN(key+"_tokens", estTokens) {
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "token budget exceeded, retry later")
				return
			}
		}
	}
	if h.budgetMgr != nil {
		if err := h.budgetMgr.Check(tenant); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(provider.ErrorResponse{
				Error: provider.Error{Message: err.Error(), Type: "insufficient_quota", Code: "429"},
			})
			return
		}
	}

	// Fase 7: cache lookup (only non-streaming 200 responses are cached)
	var cacheKey string
	var cacheTTL time.Duration
	if h.cache != nil && !req.Stream && r.Header.Get("X-Cache-Skip") != "true" {
		if ttlStr := r.Header.Get("X-Cache-TTL"); ttlStr != "" {
			if d, err := time.ParseDuration(ttlStr); err == nil && d > 0 {
				cacheTTL = d
			}
		}
		if cacheTTL == 0 {
			cacheTTL = h.cacheTTL
			if cacheTTL == 0 {
				cacheTTL = 5 * time.Minute
			}
		}
		cacheKey = cache.BuildKey(req)
		if data, ok := h.cache.Get(cacheKey); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("X-Gateway-Provider", "cache")
			metrics.CacheHits.WithLabelValues("hit").Inc()
			metrics.RequestsTotal.WithLabelValues(r.Method, r.URL.Path, "200", "cache").Inc()
			_, _ = w.Write(data)
			h.log.Info("cache hit", "model", req.Model, "request_id", requestID, "cache_key", cacheKey[:8])
			return
		}
		metrics.CacheHits.WithLabelValues("miss").Inc()
	}

	if req.Stream {
		h.handleStream(w, r, req, requestID, ctx)
		return
	}

	// OTEL span per chat completion
	tracer := otel.Tracer("gateway.handler")
	ctx, span := tracer.Start(ctx, "chat.completions",
	)
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
		tenant := r.Header.Get("X-Tenant-ID")
		usd := float64(resp.Usage.TotalTokens) * 0.00001
		h.budgetMgr.Record(tenant, resp.Usage.TotalTokens, usd)
	}
	// Fase 7: cache store (only cache successful non-streaming)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Provider", providerName)
	if h.cache != nil && cacheKey != "" {
		data, _ := json.Marshal(resp)
		h.cache.Set(cacheKey, data, cacheTTL)
		w.Header().Set("X-Cache", "MISS")
		metrics.CacheSize.Set(float64(h.cache.Stats().Size))
		_, _ = w.Write(data)
		return
	} else if h.cache != nil {
		w.Header().Set("X-Cache", "MISS")
	}
	_ = json.NewEncoder(w).Encode(resp)
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

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request, req provider.ChatRequest, requestID string, ctx context.Context) {
	primary, err := h.registry.Resolve(req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Gateway-Provider", primary.Name())
	w.WriteHeader(http.StatusOK)

	ch, errCh := primary.SendStream(ctx, req)

	enc := json.NewEncoder(w)
	_ = enc

	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				ch = nil
				// check errCh before closing
				select {
				case err := <-errCh:
					if err != nil {
						h.log.Error("stream error", "provider", primary.Name(), "request_id", requestID, "error", err)
					}
				default:
				}
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				flusher.Flush()
				h.log.Info("stream completed", "model", req.Model, "provider", primary.Name(), "request_id", requestID)
				return
			}
			data, _ := json.Marshal(chunk)
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(data)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		case err := <-errCh:
			if err != nil {
				if provider.IsRetryable(err) {
					h.log.Warn("stream primary failed, fallback not yet implemented for streams", "provider", primary.Name(), "request_id", requestID, "error", err)
				}
				h.log.Error("stream provider error", "provider", primary.Name(), "request_id", requestID, "error", err)
			}
			errCh = nil
		case <-ctx.Done():
			return
		}
		if ch == nil && errCh == nil {
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}
	}
}

// dispatch sends req to the provider that owns req.Model with retry, circuit breaker and hedge.
// If primary returns retryable error, it tries each fallback in chain, remapping model via aliases.
func (h *Handler) dispatch(ctx context.Context, req provider.ChatRequest, requestID string) (provider.ChatResponse, string, error) {
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
	if h.hedgeCfg.Enabled && len(h.fallbackChain) > 1 {
		if resp, name, ok := h.dispatchHedge(ctx, req, requestID, primary.Name()); ok {
			return resp, name, nil
		}
	}

	for _, name := range h.fallbackChain {
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

	return provider.ChatResponse{}, "", fmt.Errorf("all providers failed for model %q", req.Model)
}

func (h *Handler) sendWithRetry(ctx context.Context, p provider.Provider, req provider.ChatRequest) (provider.ChatResponse, error) {
	var resp provider.ChatResponse
	var lastErr error
	err := resilience.Do(ctx, h.retryCfg, provider.IsRetryable, func() error {
		var err error
		resp, err = p.Send(ctx, req)
		lastErr = err
		return err
	})
	if err != nil {
		return provider.ChatResponse{}, lastErr
	}
	return resp, nil
}

func (h *Handler) dispatchHedge(ctx context.Context, req provider.ChatRequest, requestID, primaryName string) (provider.ChatResponse, string, bool) {
	// pick first two fallbacks
	var candidates []provider.Provider
	for _, name := range h.fallbackChain {
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
	ctx, cancel := context.WithTimeout(ctx, h.retryCfg.MaxDelay*2+5*time.Second)
	defer cancel()
	val, err := resilience.DoHedge(ctx, h.hedgeCfg.Delay,
		func() (any, error) {
			mapped := h.registry.RemapForFallback(req, candidates[0])
			resp, e := h.sendWithRetry(ctx, candidates[0], mapped)
			if e == nil {
				h.breakerFor(candidates[0].Name()).RecordSuccess()
				return resp, nil
			}
			h.breakerFor(candidates[0].Name()).RecordFailure()
			return nil, e
		},
		func() (any, error) {
			mapped := h.registry.RemapForFallback(req, candidates[1])
			resp, e := h.sendWithRetry(ctx, candidates[1], mapped)
			if e == nil {
				h.breakerFor(candidates[1].Name()).RecordSuccess()
				return resp, nil
			}
			h.breakerFor(candidates[1].Name()).RecordFailure()
			return nil, e
		},
	)
	if err != nil {
		return provider.ChatResponse{}, "", false
	}
	if resp, ok := val.(provider.ChatResponse); ok {
		// determine which provider won: best-effort by checking which one returned without error first
		// we return first candidate as name if both could have won; ideally DoHedge returns name too
		return resp, candidates[0].Name(), true
	}
	return provider.ChatResponse{}, "", false
}
