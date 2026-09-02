package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/fcordero/llm-api-gateway/internal/provider"
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
}

func NewEmbeddingsHandler(registry *Registry, fallbackChain []string, log *slog.Logger) *EmbeddingsHandler {
	return &EmbeddingsHandler{
		registry:      registry,
		fallbackChain: append([]string(nil), fallbackChain...),
		log:           log,
	}
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
	span.SetAttributes(attribute.String("provider", provName))
	h.log.Info("embeddings", "model", req.Model, "provider", provName, "request_id", requestID, "input_count", len(resp.Data))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Provider", provName)
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *EmbeddingsHandler) dispatch(ctx context.Context, req provider.EmbeddingRequest, requestID string) (provider.EmbeddingResponse, string, error) {
	// try primary
	primary, err := h.registry.Resolve(req.Model)
	if err != nil {
		return provider.EmbeddingResponse{}, "", err
	}
	if embedder, ok := primary.(provider.Embedder); ok {
		resp, err := embedder.Embed(ctx, req)
		if err == nil {
			return resp, primary.Name(), nil
		}
		if !provider.IsRetryable(err) {
			return provider.EmbeddingResponse{}, "", err
		}
		h.log.Warn("embeddings primary failed, attempting fallback", "provider", primary.Name(), "model", req.Model, "request_id", requestID, "error", err)
	} else {
		// provider doesn't support embeddings, treat as retryable to try fallback
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
		resp, err := embedder.Embed(ctx, mappedReq)
		if err == nil {
			h.log.Info("embeddings fallback succeeded", "provider", fb.Name(), "model", mappedReq.Model, "request_id", requestID)
			return resp, fb.Name(), nil
		}
		h.log.Warn("embeddings fallback failed", "provider", fb.Name(), "request_id", requestID, "error", err)
		if !provider.IsRetryable(err) {
			continue
		}
	}
	return provider.EmbeddingResponse{}, "", errors.New("all providers failed for embeddings model " + req.Model)
}
