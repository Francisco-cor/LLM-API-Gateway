package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
	"github.com/fcordero/llm-api-gateway/internal/resilience"
)

func TestCache_HitMissTTL(t *testing.T) {
	m := cache.NewMemory(10)
	req := provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hello"}},
	}
	key := cache.BuildKey(req)
	if _, ok := m.Get(key); ok {
		t.Fatal("should be miss initially")
	}
	resp := provider.ChatResponse{ID: "cached", Model: "gpt-4o"}
	data, _ := json.Marshal(resp)
	m.Set(key, data, 100*time.Millisecond)
	if data2, ok := m.Get(key); !ok || string(data2) != string(data) {
		t.Fatal("should be hit after set")
	}
	time.Sleep(120 * time.Millisecond)
	if _, ok := m.Get(key); ok {
		t.Fatal("should be miss after TTL")
	}
}

func TestCache_EvictionLRU(t *testing.T) {
	m := cache.NewMemory(2)
	m.Set("a", []byte("1"), 5*time.Minute)
	m.Set("b", []byte("2"), 5*time.Minute)
	m.Set("c", []byte("3"), 5*time.Minute)
	if _, ok := m.Get("a"); ok {
		t.Error("a should be evicted")
	}
	if _, ok := m.Get("b"); !ok {
		t.Error("b should exist")
	}
	if _, ok := m.Get("c"); !ok {
		t.Error("c should exist")
	}
	if m.Stats().Size != 2 {
		t.Errorf("size %d, want 2", m.Stats().Size)
	}
}

func TestCache_BuildKeyDeterministic(t *testing.T) {
	req1 := provider.ChatRequest{Model: "gpt-4o", Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}}, Temperature: floatPtr(0.7)}
	req2 := provider.ChatRequest{Model: "gpt-4o", Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}}, Temperature: floatPtr(0.7)}
	if cache.BuildKey(req1) != cache.BuildKey(req2) {
		t.Error("same request should have same key")
	}
	req2.Temperature = floatPtr(0.9)
	if cache.BuildKey(req1) == cache.BuildKey(req2) {
		t.Error("different temperature should have different key")
	}
}

func TestHandler_CacheHITMISS(t *testing.T) {
	m := cache.NewMemory(100)
	registry := proxy.NewRegistry([]provider.Provider{
		&mockProvider{name: "openai", models: []string{"gpt-4o"}, resp: provider.ChatResponse{ID: "resp1", Model: "gpt-4o", Choices: []provider.Choice{{Message: provider.ChatMessage{Role: "assistant", Content: "hi"}, FinishReason: "stop"}}}},
	})
	handler := proxy.NewHandlerWithCache(registry, []string{"openai"}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		resilience.DefaultRetryConfig(),
		resilience.DefaultCircuitConfig(),
		struct {
			Enabled bool
			Delay   time.Duration
		}{},
		nil, nil, nil, false, m, 5*time.Minute)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first request status %d, want 200 body:%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Cache") != "MISS" {
		t.Errorf("first X-Cache = %q, want MISS", w.Header().Get("X-Cache"))
	}
	// second identical request should be HIT
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("second X-Cache = %q, want HIT", w2.Header().Get("X-Cache"))
	}
	if w2.Body.String() != w.Body.String() {
		t.Error("cached body mismatch")
	}

	// X-Cache-Skip should bypass
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	req3.Header.Set("X-Cache-Skip", "true")
	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)
	if w3.Header().Get("X-Cache") == "HIT" {
		t.Error("X-Cache-Skip should not be HIT")
	}

	// stream should not be cached
	bodyStream := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"stream":true}`
	req4 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(bodyStream)))
	w4 := httptest.NewRecorder()
	handler.ServeHTTP(w4, req4)
	// stream returns SSE, but should not be HIT
	if w4.Header().Get("X-Cache") == "HIT" {
		t.Error("stream should not be cached")
	}

	// X-Cache-TTL custom
	req5 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"unique-ttl"}]}`)))
	req5.Header.Set("X-Cache-TTL", "100ms")
	w5 := httptest.NewRecorder()
	handler.ServeHTTP(w5, req5)
	if w5.Header().Get("X-Cache") != "MISS" {
		t.Errorf("ttl custom first should be MISS, got %q", w5.Header().Get("X-Cache"))
	}
}

func TestCache_SemanticSimilarity(t *testing.T) {
	if cache.Similarity("hello world", "hello world") < 0.99 {
		t.Error("identical should be ~1")
	}
	if cache.Similarity("hello world", "goodbye world") > 0.8 {
		t.Error("different should be low")
	}
	if cache.Similarity("", "hello") != 0 {
		t.Error("empty should be 0")
	}
}

func TestCache_SemanticWrapper(t *testing.T) {
	mem := cache.NewMemory(10)
	sem := cache.NewSemantic(mem, true, 0.97)
	sem.Set("k", []byte("v"), 5*time.Minute)
	if v, ok := sem.Get("k"); !ok || string(v) != "v" {
		t.Error("semantic wrapper should delegate to exact")
	}
}

func TestCache_BenchHash(t *testing.T) {
	req := provider.ChatRequest{Model: "gpt-4o", Messages: []provider.ChatMessage{{Role: "user", Content: "hello world benchmark test content"}}}
	start := time.Now()
	for i := 0; i < 10000; i++ {
		_ = cache.BuildKey(req)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Errorf("10k BuildKey took %v, want <200ms", elapsed)
	}
}

func floatPtr(f float64) *float64 { return &f }
