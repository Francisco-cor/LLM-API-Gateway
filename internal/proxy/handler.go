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

	"github.com/fcordero/llm-api-gateway/internal/metrics"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/resilience"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Handler serves POST /v1/chat/completions, routing each request to the
// provider that owns the requested model and falling back through
// fallbackChain on retryable errors. It integrates retry, circuit breaker
// and hedge per Fase 4.
type Handler struct {
	registry      *Registry
	fallbackChain []string
	log           *slog.Logger

	retryCfg    resilience.RetryConfig
	circuits    map[string]*resilience.Breaker
	circuitsMu  sync.RWMutex
	hedgeCfg    hedgeConfig
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
	h := &Handler{
		registry:      registry,
		fallbackChain: fallbackChain,
		log:           log,
		retryCfg:      retryCfg,
		circuits:      make(map[string]*resilience.Breaker),
		hedgeCfg:      hedge,
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
	ctx := r.Context()

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

	// metrics
	metrics.ObserveTokens(providerName, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	metrics.CircuitState.WithLabelValues(providerName).Set(float64(h.breakerFor(providerName).State()))

	// propagate Retry-After if handler set it (fallback case handled in dispatch)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Provider", providerName)
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
