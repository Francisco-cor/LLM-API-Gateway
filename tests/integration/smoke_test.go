package integration

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/auth"
	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
	"github.com/fcordero/llm-api-gateway/internal/resilience"
)

const (
	testAPIKey      = "sk-integration-full"
	testChatOnlyKey = "sk-integration-chat-only"
)

// gateway builds the same route table main uses (minus observability
// middleware) over a real httptest server, with a fake OpenAI upstream.
func gateway(t *testing.T, upstreamURL string, models []string) *httptest.Server {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := provider.NewOpenAI("upstream-secret", upstreamURL, 5*time.Second, models)
	registry := proxy.NewRegistry([]provider.Provider{upstream})

	chat := proxy.NewHandler(registry, nil, log)
	// One attempt keeps upstream error assertions fast; production uses the
	// configurable retry chain.
	chat.SetRetryConfig(resilience.RetryConfig{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	chat.SetCircuitConfig(resilience.CircuitConfig{FailureThreshold: 100, OpenTimeout: time.Second})

	store := auth.New([]config.APIKeyConfig{
		{Key: testAPIKey, Tenant: "tenant-full", Scopes: []string{"*"}},
		{Key: testChatOnlyKey, Tenant: "tenant-chat", Scopes: []string{"chat:write"}},
	})
	enabled := &atomic.Bool{}
	enabled.Store(true)

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", chat)
	mux.Handle("GET /v1/models", proxy.NewModelsHandler(registry))

	srv := httptest.NewServer(auth.MiddlewareWithEnabled(enabled, store, mux))
	t.Cleanup(srv.Close)
	return srv
}

func doRequest(t *testing.T, method, url, key, body string) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, resp.Header, data
}

func TestChatCompletionsHappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello from upstream"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`))
	}))
	defer upstream.Close()

	srv := gateway(t, upstream.URL, []string{"gpt-4o"})
	status, headers, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", testAPIKey,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if got := headers.Get("X-Gateway-Provider"); got != "openai" {
		t.Fatalf("X-Gateway-Provider = %q, want openai", got)
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage provider.Usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response %s: %v", body, err)
	}
	if resp.Model != "gpt-4o" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello from upstream" {
		t.Fatalf("unexpected response: %s", body)
	}
	if resp.Usage.TotalTokens != 9 {
		t.Fatalf("usage = %+v, want total 9", resp.Usage)
	}
}

func TestUpstreamRateLimitBecomesGateway429WithRetryAfter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down please"}}`))
	}))
	defer upstream.Close()

	srv := gateway(t, upstream.URL, []string{"gpt-4o"})
	status, headers, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", testAPIKey,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body: %s)", status, body)
	}
	if got := headers.Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope %s: %v", body, err)
	}
	if envelope.Error.Type != "rate_limit_error" || envelope.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("error = %+v, want rate_limit_error/rate_limit_exceeded", envelope.Error)
	}
}

func TestStreamingRelaysChunksAndDone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req provider.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !req.Stream {
			http.Error(w, "expected a streaming request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv := gateway(t, upstream.URL, []string{"gpt-4o"})
	status, headers, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", testAPIKey,
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if got := headers.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}
	text := string(body)
	for _, want := range []string{`"content":"Hel"`, `"content":"lo"`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream body missing %q:\n%s", want, text)
		}
	}
}

func TestStreamingAbruptCloseEmitsErrorWithoutDone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"s2\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		// Return without [DONE] or a finish_reason: truncated upstream.
	}))
	defer upstream.Close()

	srv := gateway(t, upstream.URL, []string{"gpt-4o"})
	status, _, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", testAPIKey,
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	text := string(body)
	if !strings.Contains(text, "event: error") {
		t.Fatalf("missing error event:\n%s", text)
	}
	if !strings.Contains(text, "stream_truncated") {
		t.Fatalf("missing stream_truncated code:\n%s", text)
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("truncated stream must not advertise completion:\n%s", text)
	}
}

func TestStreamingToolCallsKeepIndexAndUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"s3\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"s3\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"s3\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv := gateway(t, upstream.URL, []string{"gpt-4o"})
	status, _, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", testAPIKey,
		`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	var toolIndex = -1
	var toolName, arguments string
	var total int
	for _, payload := range ssePayloads(body) {
		if payload == "[DONE]" {
			continue
		}
		var chunk provider.StreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", payload, err)
		}
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				toolIndex = call.Index
				if call.Function.Name != "" {
					toolName = call.Function.Name
				}
				arguments += call.Function.Arguments
			}
		}
		if chunk.Usage != nil {
			total = chunk.Usage.TotalTokens
		}
	}
	if toolIndex != 0 {
		t.Fatalf("tool call index = %d, want 0", toolIndex)
	}
	if toolName != "lookup" || arguments != `{"q":"x"}` {
		t.Fatalf("tool call = %q %q", toolName, arguments)
	}
	if total != 18 {
		t.Fatalf("stream usage total = %d, want 18", total)
	}
}

// ssePayloads extracts the data field of each SSE event.
func ssePayloads(body []byte) []string {
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if payload, ok := strings.CutPrefix(line, "data: "); ok {
			out = append(out, strings.TrimSpace(payload))
		}
	}
	return out
}

func TestModelsEndpointHidesRoutingPatterns(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	srv := gateway(t, upstream.URL, []string{"gpt-4o", "gpt-4*", "gpt-4o"})
	status, _, body := doRequest(t, http.MethodGet, srv.URL+"/v1/models", testAPIKey, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(string(body), `"gpt-4o"`) {
		t.Fatalf("concrete model missing: %s", body)
	}
	if strings.Contains(string(body), "gpt-4*") {
		t.Fatalf("routing pattern leaked in /v1/models: %s", body)
	}
}

func TestAuthValidInvalidAndInsufficientScope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-auth","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	srv := gateway(t, upstream.URL, []string{"gpt-4o"})
	chatBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`

	t.Run("valid key", func(t *testing.T) {
		status, _, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", testAPIKey, chatBody)
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %s", status, body)
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		status, _, body := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", "sk-not-a-key", chatBody)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (body: %s)", status, body)
		}
		if !strings.Contains(string(body), "invalid_api_key") {
			t.Fatalf("unexpected error body: %s", body)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		status, _, _ := doRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", "", chatBody)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})

	t.Run("insufficient scope", func(t *testing.T) {
		status, headers, body := doRequest(t, http.MethodGet, srv.URL+"/v1/models", testChatOnlyKey, "")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body: %s)", status, body)
		}
		if got := headers.Get("WWW-Authenticate"); !strings.Contains(got, "insufficient_scope") {
			t.Fatalf("WWW-Authenticate = %q", got)
		}
	})
}
