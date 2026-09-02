package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
)

func TestEmbeddings_Success(t *testing.T) {
	embedResp := provider.EmbeddingResponse{
		Object: "list",
		Data: []provider.EmbeddingData{
			{Object: "embedding", Index: 0, Embedding: []float32{0.1, 0.2, 0.3}},
		},
		Model: "text-embedding-3-small",
		Usage: provider.EmbeddingUsage{PromptTokens: 5, TotalTokens: 5},
	}
	openai := &mockProvider{name: "openai", models: []string{"text-embedding-3-small"}, embedResp: embedResp}
	registry := proxy.NewRegistry([]provider.Provider{openai})
	handler := proxy.NewEmbeddingsHandler(registry, []string{"openai"}, discardLogger())

	body, _ := json.Marshal(map[string]any{
		"model": "text-embedding-3-small",
		"input": "hello world",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d want 200 body %s", w.Code, w.Body.String())
	}
	var got provider.EmbeddingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data) != 1 || len(got.Data[0].Embedding) != 3 {
		t.Errorf("unexpected embedding data %+v", got)
	}
	if got.Model != "text-embedding-3-small" {
		t.Errorf("model %q want text-embedding-3-small", got.Model)
	}
	if openai.embedCount != 1 {
		t.Errorf("embed called %d want 1", openai.embedCount)
	}
}

func TestEmbeddings_ArrayInput(t *testing.T) {
	embedResp := provider.EmbeddingResponse{
		Object: "list",
		Data: []provider.EmbeddingData{
			{Object: "embedding", Index: 0, Embedding: []float32{0.1}},
			{Object: "embedding", Index: 1, Embedding: []float32{0.2}},
		},
		Model: "text-embedding-3-small",
		Usage: provider.EmbeddingUsage{PromptTokens: 10, TotalTokens: 10},
	}
	openai := &mockProvider{name: "openai", models: []string{"text-embedding-3-small"}, embedResp: embedResp}
	registry := proxy.NewRegistry([]provider.Provider{openai})
	handler := proxy.NewEmbeddingsHandler(registry, []string{"openai"}, discardLogger())

	body, _ := json.Marshal(map[string]any{
		"model": "text-embedding-3-small",
		"input": []string{"hello", "world"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d want 200 body %s", w.Code, w.Body.String())
	}
	var got provider.EmbeddingResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Data) != 2 {
		t.Errorf("got %d embeddings want 2", len(got.Data))
	}
}

func TestEmbeddings_Validation(t *testing.T) {
	openai := &mockProvider{name: "openai", models: []string{"text-embedding-3-small"}}
	registry := proxy.NewRegistry([]provider.Provider{openai})
	handler := proxy.NewEmbeddingsHandler(registry, []string{"openai"}, discardLogger())

	cases := []struct {
		name       string
		body       map[string]any
		wantStatus int
	}{
		{name: "missing model", body: map[string]any{"input": "hi"}, wantStatus: http.StatusBadRequest},
		{name: "missing input", body: map[string]any{"model": "text-embedding-3-small"}, wantStatus: http.StatusBadRequest},
		{name: "unknown model", body: map[string]any{"model": "unknown", "input": "hi"}, wantStatus: http.StatusNotFound},
		{name: "empty input string (should be caught as missing?)", body: map[string]any{"model": "text-embedding-3-small", "input": ""}, wantStatus: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(b))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Errorf("got %d want %d body %s", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

func TestEmbeddings_FallbackOnRetryable(t *testing.T) {
	primary := &mockProvider{
		name:     "openai",
		models:   []string{"text-embedding-3-small"},
		embedErr: &provider.ProviderError{ProviderName: "openai", StatusCode: 429, Message: "rate limited", Retryable: true},
	}
	fallbackResp := provider.EmbeddingResponse{
		Object: "list",
		Data:   []provider.EmbeddingData{{Object: "embedding", Index: 0, Embedding: []float32{0.9}}},
		Model:  "text-embedding-3-small",
	}
	fallback := &mockProvider{name: "gemini", models: []string{"text-embedding-004"}, embedResp: fallbackResp}
	// Need to allow model alias or ensure registry can remap gpt model to gemini? For embeddings test, use same model name on both providers to simplify
	primary.models = []string{"text-embedding-3-small"}
	fallback.models = []string{"text-embedding-3-small"}
	registry := proxy.NewRegistry([]provider.Provider{primary, fallback})
	handler := proxy.NewEmbeddingsHandler(registry, []string{"openai", "gemini"}, discardLogger())

	body, _ := json.Marshal(map[string]any{"model": "text-embedding-3-small", "input": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d want 200 body %s", w.Code, w.Body.String())
	}
	if fallback.embedCount != 1 {
		t.Errorf("fallback embedCount %d want 1", fallback.embedCount)
	}
	if w.Header().Get("X-Gateway-Provider") != "gemini" {
		t.Errorf("X-Gateway-Provider %q want gemini", w.Header().Get("X-Gateway-Provider"))
	}
}

func TestEmbeddings_GeminiTranslation(t *testing.T) {
	// Real Gemini provider test via httptest to verify translate
	// Use a test server that captures Gemini embedContent request and returns valid response
	captured := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embedding": map[string]any{"values": []float32{0.1, 0.2, 0.3}},
		})
	}))
	defer srv.Close()

	// Use provider.Gemini directly with test server URL
	gem := provider.NewGemini("test-key", srv.URL, 0, []string{"text-embedding-004"})
	resp, err := gem.Embed(context.Background(), provider.EmbeddingRequest{Model: "text-embedding-004", Input: "hello"})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 3 {
		t.Errorf("unexpected gemini embed resp %+v", resp)
	}
	if captured["content"] == nil {
		t.Error("expected content in captured Gemini request")
	}
	// also test array input via handler
	registry := proxy.NewRegistry([]provider.Provider{gem})
	handler := proxy.NewEmbeddingsHandler(registry, []string{"gemini"}, discardLogger())
	body, _ := json.Marshal(map[string]any{"model": "text-embedding-004", "input": []string{"a", "b"}})
	// gemini handler will call gemini embed twice (one per input). Need server to handle multiple calls.
	// Our test server already handles any call, so next calls will succeed.
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handler with array input got %d want 200 body %s", w.Code, w.Body.String())
	}
	var got provider.EmbeddingResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Data) != 2 {
		t.Errorf("got %d embeddings want 2 for array input", len(got.Data))
	}
}
