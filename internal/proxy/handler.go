package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// Handler serves POST /v1/chat/completions, routing each request to the
// provider that owns the requested model and falling back through
// fallbackChain on retryable errors.
type Handler struct {
	registry      *Registry
	fallbackChain []string
	log           *slog.Logger
}

// NewHandler creates a chat completions Handler.
func NewHandler(registry *Registry, fallbackChain []string, log *slog.Logger) *Handler {
	return &Handler{registry: registry, fallbackChain: fallbackChain, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
		return
	}

	var req provider.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
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

	resp, providerName, err := h.dispatch(r.Context(), req, requestID)
	if err != nil {
		h.log.Error("dispatch failed",
			"model", req.Model,
			"request_id", requestID,
			"error", err,
		)
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}

	h.log.Info("chat completion",
		"model", req.Model,
		"provider", providerName,
		"request_id", requestID,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// dispatch sends req to the provider that owns req.Model. If that provider
// returns a retryable error, dispatch tries each remaining provider in
// fallbackChain in order, using the same request.
func (h *Handler) dispatch(ctx context.Context, req provider.ChatRequest, requestID string) (provider.ChatResponse, string, error) {
	primary, err := h.registry.Resolve(req.Model)
	if err != nil {
		return provider.ChatResponse{}, "", err
	}

	resp, err := primary.Send(ctx, req)
	if err == nil {
		return resp, primary.Name(), nil
	}
	if !provider.IsRetryable(err) {
		return provider.ChatResponse{}, "", err
	}

	h.log.Warn("primary provider failed, attempting fallback",
		"provider", primary.Name(),
		"model", req.Model,
		"request_id", requestID,
		"error", err,
	)

	for _, name := range h.fallbackChain {
		if name == primary.Name() {
			continue
		}
		fallback, ok := h.registry.Get(name)
		if !ok {
			continue
		}

		resp, err = fallback.Send(ctx, req)
		if err == nil {
			h.log.Info("fallback succeeded",
				"provider", fallback.Name(),
				"model", req.Model,
				"request_id", requestID,
			)
			return resp, fallback.Name(), nil
		}
		h.log.Warn("fallback provider failed",
			"provider", fallback.Name(),
			"request_id", requestID,
			"error", err,
		)
	}

	return provider.ChatResponse{}, "", fmt.Errorf("all providers failed for model %q", req.Model)
}
