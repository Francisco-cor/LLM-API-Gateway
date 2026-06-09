package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHandler_ChatCompletions(t *testing.T) {
	successResp := provider.ChatResponse{
		ID:    "test-id",
		Model: "gpt-4o",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.ChatMessage{Role: "assistant", Content: "Hello!"},
			FinishReason: "stop",
		}},
	}

	cases := []struct {
		name       string
		method     string
		body       map[string]any
		wantStatus int
	}{
		{
			name:   "valid request succeeds",
			method: http.MethodPost,
			body: map[string]any{
				"model":    "gpt-4o",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "missing model returns 400",
			method: http.MethodPost,
			body: map[string]any{
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:   "empty messages returns 400",
			method: http.MethodPost,
			body: map[string]any{
				"model":    "gpt-4o",
				"messages": []map[string]string{},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:   "unknown model returns 502",
			method: http.MethodPost,
			body: map[string]any{
				"model":    "llama-3-70b",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			},
			wantStatus: http.StatusBadGateway,
		},
	}

	registry := proxy.NewRegistry([]provider.Provider{
		&mockProvider{name: "openai", models: []string{"gpt-4o"}, resp: successResp},
	})
	handler := proxy.NewHandler(registry, []string{"openai"}, discardLogger())

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(tc.method, "/v1/chat/completions", bytes.NewReader(bodyBytes))
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("got status %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

func TestHandler_FallbackOnRetryableError(t *testing.T) {
	primary := &mockProvider{
		name:   "openai",
		models: []string{"gpt-4o"},
		err: &provider.ProviderError{
			ProviderName: "openai",
			StatusCode:   http.StatusTooManyRequests,
			Message:      "rate limited",
			Retryable:    true,
		},
	}
	fallback := &mockProvider{
		name:   "anthropic",
		models: []string{"claude-sonnet-4-6"},
		resp: provider.ChatResponse{
			ID:    "fallback-id",
			Model: "gpt-4o",
			Choices: []provider.Choice{{
				Message:      provider.ChatMessage{Role: "assistant", Content: "from fallback"},
				FinishReason: "stop",
			}},
		},
	}

	registry := proxy.NewRegistry([]provider.Provider{primary, fallback})
	handler := proxy.NewHandler(registry, []string{"openai", "anthropic"}, discardLogger())

	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	var resp provider.ChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != "fallback-id" {
		t.Errorf("got response from %q, want fallback response", resp.ID)
	}
	if primary.callCount != 1 {
		t.Errorf("primary called %d times, want 1", primary.callCount)
	}
	if fallback.callCount != 1 {
		t.Errorf("fallback called %d times, want 1", fallback.callCount)
	}
}

func TestHandler_NonRetryableErrorSkipsFallback(t *testing.T) {
	primary := &mockProvider{
		name:   "openai",
		models: []string{"gpt-4o"},
		err: &provider.ProviderError{
			ProviderName: "openai",
			StatusCode:   http.StatusBadRequest,
			Message:      "invalid request",
			Retryable:    false,
		},
	}
	fallback := &mockProvider{name: "anthropic", models: []string{"claude-sonnet-4-6"}}

	registry := proxy.NewRegistry([]provider.Provider{primary, fallback})
	handler := proxy.NewHandler(registry, []string{"openai", "anthropic"}, discardLogger())

	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("got status %d, want 502", w.Code)
	}
	if fallback.callCount != 0 {
		t.Errorf("fallback called %d times, want 0 for non-retryable error", fallback.callCount)
	}
}
