package contract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/capability"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
)

func capBool(v bool) *bool { return &v }

func newCapabilityGateway(t *testing.T, providers []provider.Provider, fallback []string, tables map[string]capability.Table) http.Handler {
	t.Helper()
	reg := proxy.NewRegistryWithCapabilities(providers, nil, nil, tables)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", proxy.NewHandler(reg, fallback, discardLogger()))
	return mux
}

func doChat(t *testing.T, h http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(data))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	if out.Error.Param == "" {
		t.Logf("error without param: %s", w.Body.String())
	}
	return out.Error.Code
}

func errorParam(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return out.Error.Param
}

func TestContract_CapabilityRejectedBeforeProviderCall(t *testing.T) {
	primary := &mockProvider{name: "openai", models: []string{"gpt-4o"}, resp: sampleChatResponse("gpt-4o")}
	fallback := &mockProvider{name: "anthropic", models: []string{"claude-sonnet-4-6"}, resp: sampleChatResponse("claude-sonnet-4-6")}
	tables := map[string]capability.Table{
		"openai":    capability.NewTable("openai", map[string]capability.Overrides{"gpt-4o": {Tools: capBool(false)}}),
		"anthropic": capability.NewTable("anthropic", nil),
	}
	h := newCapabilityGateway(t, []provider.Provider{primary, fallback}, []string{"openai", "anthropic"}, tables)

	w := doChat(t, h, map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}},
		}},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "model_capability_mismatch" {
		t.Fatalf("code = %q, want model_capability_mismatch", code)
	}
	if param := errorParam(t, w); param != "tools" {
		t.Errorf("param = %q, want tools", param)
	}
	if primary.callCount != 0 || fallback.callCount != 0 {
		t.Errorf("providers were called: primary=%d fallback=%d", primary.callCount, fallback.callCount)
	}
}

func TestContract_CapabilityMismatchOnStream(t *testing.T) {
	primary := &mockProvider{name: "openai", models: []string{"gpt-4o"}}
	tables := map[string]capability.Table{
		"openai": capability.NewTable("openai", map[string]capability.Overrides{"gpt-4o": {Tools: capBool(false)}}),
	}
	h := newCapabilityGateway(t, []provider.Provider{primary}, nil, tables)

	w := doChat(t, h, map[string]any{
		"model":    "gpt-4o",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}},
		}},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 before SSE: %s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "model_capability_mismatch" {
		t.Fatalf("code = %q, want model_capability_mismatch", code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json (not SSE)", got)
	}
	if primary.callCount != 0 {
		t.Errorf("provider was called %d times", primary.callCount)
	}
}

func TestContract_VisionMismatch(t *testing.T) {
	primary := &mockProvider{name: "openai", models: []string{"gpt-4o"}, resp: sampleChatResponse("gpt-4o")}
	tables := map[string]capability.Table{
		"openai": capability.NewTable("openai", map[string]capability.Overrides{"gpt-4o": {Vision: capBool(false)}}),
	}
	h := newCapabilityGateway(t, []provider.Provider{primary}, nil, tables)

	w := doChat(t, h, map[string]any{
		"model": "gpt-4o",
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type":      "image_url",
				"image_url": map[string]string{"url": "https://example.com/cat.png"},
			}},
		}},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "model_capability_mismatch" {
		t.Fatalf("code = %q, want model_capability_mismatch", code)
	}
	if param := errorParam(t, w); param != "messages" {
		t.Errorf("param = %q, want messages", param)
	}
	if primary.callCount != 0 {
		t.Errorf("provider was called %d times", primary.callCount)
	}
}

func TestContract_FallbackSkipsModelWithoutTools(t *testing.T) {
	primary := &mockProvider{
		name:   "openai",
		models: []string{"gpt-4o"},
		err:    &provider.ProviderError{ProviderName: "openai", StatusCode: 500, Message: "internal", Retryable: true},
	}
	noTools := &mockProvider{name: "anthropic", models: []string{"claude-sonnet-4-6"}, resp: sampleChatResponse("claude-sonnet-4-6")}
	withTools := &mockProvider{name: "gemini", models: []string{"gemini-2.5-pro"}, resp: sampleChatResponse("gemini-2.5-pro")}
	tables := map[string]capability.Table{
		"openai":    capability.NewTable("openai", nil),
		"anthropic": capability.NewTable("anthropic", map[string]capability.Overrides{"claude-sonnet-4-6": {Tools: capBool(false)}}),
		"gemini":    capability.NewTable("gemini", nil),
	}
	reg := proxy.NewRegistryWithCapabilities(
		[]provider.Provider{primary, noTools, withTools},
		map[string][]string{"gpt-4o": {"claude-sonnet-4-6", "gemini-2.5-pro"}},
		nil,
		tables,
	)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", proxy.NewHandler(reg, []string{"openai", "anthropic", "gemini"}, discardLogger()))

	w := doChat(t, mux, map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}},
		}},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Gateway-Provider"); got != "gemini" {
		t.Fatalf("provider = %q, want gemini", got)
	}
	if noTools.callCount != 0 {
		t.Errorf("incompatible fallback was called %d times", noTools.callCount)
	}
	if withTools.callCount == 0 {
		t.Error("compatible fallback was not called")
	}
}

func TestContract_NoCompatibleFallbackSkipsAll(t *testing.T) {
	primary := &mockProvider{
		name:   "openai",
		models: []string{"gpt-4o"},
		err:    &provider.ProviderError{ProviderName: "openai", StatusCode: 500, Message: "internal", Retryable: true},
	}
	noTools := &mockProvider{name: "anthropic", models: []string{"claude-sonnet-4-6"}, resp: sampleChatResponse("claude-sonnet-4-6")}
	tables := map[string]capability.Table{
		"openai":    capability.NewTable("openai", nil),
		"anthropic": capability.NewTable("anthropic", map[string]capability.Overrides{"*": {Tools: capBool(false)}}),
	}
	reg := proxy.NewRegistryWithCapabilities(
		[]provider.Provider{primary, noTools},
		map[string][]string{"gpt-4o": {"claude-sonnet-4-6"}},
		nil,
		tables,
	)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", proxy.NewHandler(reg, []string{"openai", "anthropic"}, discardLogger()))

	w := doChat(t, mux, map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}},
		}},
	})

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (primary error): %s", w.Code, w.Body.String())
	}
	if noTools.callCount != 0 {
		t.Errorf("incompatible fallback was called %d times", noTools.callCount)
	}
}

func TestContract_WeightedRoutingFiltersIncompatibleProvider(t *testing.T) {
	openai := &mockProvider{name: "openai", models: []string{"gpt-4o"}, resp: sampleChatResponse("gpt-4o")}
	anthropic := &mockProvider{name: "anthropic", models: []string{"gpt-4o"}, resp: sampleChatResponse("gpt-4o")}
	tables := map[string]capability.Table{
		"openai":    capability.NewTable("openai", nil),
		"anthropic": capability.NewTable("anthropic", map[string]capability.Overrides{"gpt-4o": {Vision: capBool(false)}}),
	}
	reg := proxy.NewRegistryWithCapabilities(
		[]provider.Provider{openai, anthropic},
		nil,
		map[string][]proxy.WeightedConfig{"gpt-4o": {
			{Provider: "openai", Weight: 1},
			{Provider: "anthropic", Weight: 100},
		}},
		tables,
	)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", proxy.NewHandler(reg, nil, discardLogger()))

	for i := 0; i < 10; i++ {
		w := doChat(t, mux, map[string]any{
			"model": "gpt-4o",
			"messages": []map[string]any{{
				"role": "user",
				"content": []map[string]any{{
					"type":      "image_url",
					"image_url": map[string]string{"url": "https://example.com/cat.png"},
				}},
			}},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d: %s", i, w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Gateway-Provider"); got != "openai" {
			t.Fatalf("request %d routed to %q; incompatible weighted provider was selected", i, got)
		}
	}
	if anthropic.callCount != 0 {
		t.Errorf("incompatible provider handled %d requests", anthropic.callCount)
	}
}

func TestContract_JSONSchemaMismatchIsActionable(t *testing.T) {
	primary := &mockProvider{name: "anthropic", models: []string{"claude-sonnet-4-6"}, resp: sampleChatResponse("claude-sonnet-4-6")}
	tables := map[string]capability.Table{
		"anthropic": capability.NewTable("anthropic", nil),
	}
	h := newCapabilityGateway(t, []provider.Provider{primary}, nil, tables)

	w := doChat(t, h, map[string]any{
		"model":    "claude-sonnet-4-6",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "answer", "schema": map[string]any{"type": "object"}},
		},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "model_capability_mismatch" {
		t.Fatalf("code = %q, want model_capability_mismatch", code)
	}
	if param := errorParam(t, w); param != "response_format" {
		t.Errorf("param = %q, want response_format", param)
	}
	if primary.callCount != 0 {
		t.Errorf("provider was called %d times", primary.callCount)
	}
}
