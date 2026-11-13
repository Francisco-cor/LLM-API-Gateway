package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/budget"
	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
	"github.com/fcordero/llm-api-gateway/internal/resilience"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHandler_BudgetReservationBlocksConcurrentQuotaOverrun(t *testing.T) {
	providerResp := provider.ChatResponse{
		ID:    "budgeted",
		Model: "gpt-4o",
		Usage: provider.Usage{PromptTokens: 1, TotalTokens: 1},
	}
	registry := proxy.NewRegistry([]provider.Provider{
		&mockProvider{name: "openai", models: []string{"gpt-4o"}, resp: providerResp},
	})
	manager := budget.New(2, 0, nil)
	handler := proxy.NewHandlerWithBudget(registry, []string{"openai"}, discardLogger(), manager)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("X-Tenant-ID", "tenant-a")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d got %d, want 200: %s", i+1, w.Code, w.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("X-Tenant-ID", "tenant-a")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third request got %d, want 429: %s", w.Code, w.Body.String())
	}
}

// A large prompt must pass token-aware limiting when the token bucket is sized
// in tokens, not in request burst units.
func TestHandler_TokenAwareAllowsLargePrompt(t *testing.T) {
	registry := proxy.NewRegistry([]provider.Provider{
		&mockProvider{name: "openai", models: []string{"gpt-4o"}, resp: provider.ChatResponse{ID: "x", Model: "gpt-4o"}},
	})
	limiter := ratelimit.New(60, 10)
	defer limiter.Close()
	handler := proxy.NewHandlerWithRateLimit(registry, []string{"openai"}, discardLogger(), limiter, nil, nil, true)
	handler.SetTokenLimits(60000, 10000)

	prompt := strings.Repeat("a", 2000) // ~500 tokens, far above request burst 10
	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d got %d, want 200: %s", i+1, w.Code, w.Body.String())
		}
	}
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
			name:   "unknown model returns 404",
			method: http.MethodPost,
			body: map[string]any{
				"model":    "llama-3-70b",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:   "stream true is now handled (not rejected as 400)",
			method: http.MethodPost,
			body: map[string]any{
				"model":    "gpt-4o",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
				"stream":   true,
			},
			wantStatus: http.StatusOK,
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
	// with retry (3 attempts) primary is retried before fallback
	if primary.callCount != 3 {
		t.Errorf("primary called %d times, want 3 (retry: 3 attempts)", primary.callCount)
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

	// A provider 400 is the caller's fault, not a gateway failure: it must
	// surface as 400 instead of being flattened into 502.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", w.Code)
	}
	if fallback.callCount != 0 {
		t.Errorf("fallback called %d times, want 0 for non-retryable error", fallback.callCount)
	}
}

// streamChunk builds one SSE chunk for tests that need a custom stream.
func streamChunk(id string, finish *string, usage *provider.Usage) provider.StreamChunk {
	return provider.StreamChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Model:   "gpt-4o",
		Choices: []provider.StreamChoice{{Index: 0, Delta: provider.ChatMessage{Role: "assistant", Content: "partial"}, FinishReason: finish}},
		Usage:   usage,
	}
}

// A stream that closes without finish_reason or usage is a truncation. The
// gateway must say so and must not send [DONE], which would look successful.
func TestHandler_StreamTruncatedEmitsErrorWithoutDONE(t *testing.T) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- streamChunk("chatcmpl-trunc", nil, nil)
	close(ch)
	errCh := make(chan error)
	close(errCh)

	p := &mockProvider{name: "openai", models: []string{"gpt-4o"}, streamCh: ch, streamErr: errCh}
	handler := proxy.NewHandler(proxy.NewRegistry([]provider.Provider{p}), []string{"openai"}, discardLogger())

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("headers were already sent, got %d: %s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	if strings.Contains(got, "data: [DONE]") {
		t.Fatalf("truncated stream must not send [DONE]: %s", got)
	}
	if !strings.Contains(got, "event: error") || !strings.Contains(got, "stream_truncated") {
		t.Fatalf("truncated stream missing error event: %s", got)
	}
}

// A provider error after the first chunk must close the stream with an error
// event and no [DONE].
func TestHandler_StreamMidStreamErrorNoDONE(t *testing.T) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- streamChunk("chatcmpl-mid", nil, nil)
	close(ch)
	errCh := make(chan error)
	go func() {
		// Deliver the error after the first chunk has been handed over, so the
		// failure is genuinely mid-stream.
		time.Sleep(50 * time.Millisecond)
		errCh <- &provider.ProviderError{ProviderName: "openai", StatusCode: http.StatusInternalServerError, Message: "upstream exploded", Retryable: true}
		close(errCh)
	}()

	p := &mockProvider{name: "openai", models: []string{"gpt-4o"}, streamCh: ch, streamErr: errCh}
	handler := proxy.NewHandler(proxy.NewRegistry([]provider.Provider{p}), []string{"openai"}, discardLogger())

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	got := w.Body.String()
	if strings.Contains(got, "data: [DONE]") {
		t.Fatalf("mid-stream error must not send [DONE]: %s", got)
	}
	if !strings.Contains(got, "event: error") || !strings.Contains(got, "upstream exploded") {
		t.Fatalf("mid-stream error event missing: %s", got)
	}
}

// The reservation must be settled with provider-reported usage, not with the
// prompt-based estimate reserved before the call.
func TestHandler_StreamBudgetCommitsRealUsage(t *testing.T) {
	finish := "stop"
	usage := provider.Usage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20}
	ch := make(chan provider.StreamChunk, 1)
	ch <- streamChunk("chatcmpl-usage", &finish, &usage)
	close(ch)
	errCh := make(chan error)
	close(errCh)

	p := &mockProvider{name: "openai", models: []string{"gpt-4o"}, streamCh: ch, streamErr: errCh}
	manager := budget.New(25, 0, nil)
	handler := proxy.NewHandlerWithBudget(proxy.NewRegistry([]provider.Provider{p}), []string{"openai"}, discardLogger(), manager)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("X-Tenant-ID", "tenant-stream")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("terminal stream missing [DONE]: %s", w.Body.String())
	}

	// 20 tokens were reported, so 10 more cannot fit in a 25-token budget. An
	// estimate of a 2-character prompt would have left plenty of room.
	if _, err := manager.Reserve("tenant-stream", 10, 0); err == nil {
		t.Fatal("budget did not record the stream's real usage: 10 extra tokens were accepted")
	}
	if _, err := manager.Reserve("tenant-stream", 5, 0); err != nil {
		t.Fatalf("budget overcharged: 5 tokens within the limit were rejected: %v", err)
	}
}

// A provider 4xx says nothing about provider health; it must not open the
// circuit and starve the provider for later, valid requests.
func TestHandler_ClientErrorsDoNotOpenCircuit(t *testing.T) {
	p := &mockProvider{
		name:   "openai",
		models: []string{"gpt-4o"},
		err:    &provider.ProviderError{ProviderName: "openai", StatusCode: http.StatusBadRequest, Message: "bad request", Retryable: false},
	}
	handler := proxy.NewHandler(proxy.NewRegistry([]provider.Provider{p}), []string{"openai"}, discardLogger())

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < 8; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("request %d got %d, want 400", i+1, w.Code)
		}
	}

	p.err = nil
	p.resp = provider.ChatResponse{ID: "recovered", Model: "gpt-4o"}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("provider was skipped after client errors: got %d body %s", w.Code, w.Body.String())
	}
}

// ttlRecordingCache records the TTLs the handler asks for.
type ttlRecordingCache struct {
	*cache.Memory
	mu   sync.Mutex
	ttls []time.Duration
}

func (c *ttlRecordingCache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	c.ttls = append(c.ttls, ttl)
	c.mu.Unlock()
	c.Memory.Set(key, value, ttl)
}

func (c *ttlRecordingCache) lastTTL() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ttls) == 0 {
		return 0
	}
	return c.ttls[len(c.ttls)-1]
}

// X-Cache-TTL is client input; the configured maximum must cap it.
func TestHandler_CacheTTLHeaderIsClamped(t *testing.T) {
	c := &ttlRecordingCache{Memory: cache.NewMemory(10)}
	p := &mockProvider{name: "openai", models: []string{"gpt-4o"}, resp: provider.ChatResponse{ID: "cached", Model: "gpt-4o"}}
	handler := proxy.NewHandlerWithCache(proxy.NewRegistry([]provider.Provider{p}), []string{"openai"}, discardLogger(),
		resilience.DefaultRetryConfig(), resilience.DefaultCircuitConfig(),
		struct {
			Enabled bool
			Delay   time.Duration
		}{}, nil, nil, nil, false, c, time.Minute)
	handler.SetCacheMaxTTL(2 * time.Minute)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"ttl"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("X-Cache-TTL", "720h")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if got := c.lastTTL(); got != 2*time.Minute {
		t.Fatalf("stored TTL = %v, want clamp to 2m", got)
	}
}

// Concurrent identical misses must run the provider once.
func TestHandler_SingleflightCoalescesMisses(t *testing.T) {
	c := cache.NewMemory(10)
	p := &mockProvider{
		name:      "openai",
		models:    []string{"gpt-4o"},
		sendDelay: 100 * time.Millisecond,
		resp:      provider.ChatResponse{ID: "once", Model: "gpt-4o"},
	}
	handler := proxy.NewHandlerWithCache(proxy.NewRegistry([]provider.Provider{p}), []string{"openai"}, discardLogger(),
		resilience.DefaultRetryConfig(), resilience.DefaultCircuitConfig(),
		struct {
			Enabled bool
			Delay   time.Duration
		}{}, nil, nil, nil, false, c, time.Minute)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"same"}]}`)
	var wg sync.WaitGroup
	responses := make([]*httptest.ResponseRecorder, 2)
	for i := range responses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			responses[i] = w
		}(i)
	}
	wg.Wait()

	if p.callCount != 1 {
		t.Fatalf("provider called %d times, want 1 (single-flight)", p.callCount)
	}
	for i, w := range responses {
		if w.Code != http.StatusOK {
			t.Fatalf("response %d got %d: %s", i, w.Code, w.Body.String())
		}
	}
	hits := 0
	for _, w := range responses {
		if w.Header().Get("X-Cache") == "HIT" {
			hits++
		}
	}
	if hits != 1 {
		t.Fatalf("want exactly one HIT for the coalesced follower, got %d", hits)
	}
}

// Hedged candidate selection must not retry the same providers in the fallback
// loop after the race.
func TestHandler_HedgeDoesNotRetryAttemptedProviders(t *testing.T) {
	primary := &countingProvider{
		name:   "openai",
		models: []string{"gpt-4o"},
		err:    &provider.ProviderError{ProviderName: "openai", StatusCode: http.StatusInternalServerError, Message: "down", Retryable: true},
	}
	// The hedge loser is slow and fails; the winner answers immediately.
	loser := &countingProvider{
		name:   "anthropic",
		models: []string{"claude-sonnet-4-6"},
		delay:  150 * time.Millisecond,
		err:    &provider.ProviderError{ProviderName: "anthropic", StatusCode: http.StatusInternalServerError, Message: "down", Retryable: true},
	}
	winner := &countingProvider{name: "gemini", models: []string{"gemini-pro"}, resp: provider.ChatResponse{ID: "winner", Model: "gemini-pro"}}

	handler := proxy.NewHandlerWithResilience(proxy.NewRegistry([]provider.Provider{primary, loser, winner}),
		[]string{"openai", "anthropic", "gemini"}, discardLogger(),
		resilience.RetryConfig{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		resilience.DefaultCircuitConfig(),
		struct {
			Enabled bool
			Delay   time.Duration
		}{Enabled: true, Delay: 5 * time.Millisecond})

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Gateway-Provider") != "gemini" {
		t.Fatalf("provider header %q, want gemini", w.Header().Get("X-Gateway-Provider"))
	}
	if primary.calls.Load() != 1 || loser.calls.Load() != 1 || winner.calls.Load() != 1 {
		t.Fatalf("providers called more than once (primary=%d loser=%d winner=%d): hedge loser was retried by fallback",
			primary.calls.Load(), loser.calls.Load(), winner.calls.Load())
	}
}
