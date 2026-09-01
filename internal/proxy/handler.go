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

	resp, providerName, err := h.dispatch(ctx, req, requestID)
	if err != nil {
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
	w.Header().Set("X-Gateway-Provider", providerName)
	_ = json.NewEncoder(w).Encode(resp)
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
