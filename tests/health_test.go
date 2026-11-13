package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
)

// A fan-out killed by its own deadline is a partial view. Caching it would
// keep reporting an outage for the whole TTL after providers recover.
func TestReadiness_PartialSnapshotIsNotCached(t *testing.T) {
	slow := &mockProvider{
		name:        "openai",
		models:      []string{"gpt-4o"},
		healthDelay: 200 * time.Millisecond,
	}
	handler := proxy.NewReadinessHandlerWithOptions(
		proxy.NewRegistry([]provider.Provider{slow}),
		nil,
		proxy.HealthOptions{
			CheckTimeout:      30 * time.Millisecond,
			ProviderTimeout:   500 * time.Millisecond,
			ReadinessCacheTTL: time.Minute,
		},
	)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("probe %d got %d, want 503: %s", i+1, w.Code, w.Body.String())
		}
	}
	if got := slow.healthCalls.Load(); got != 2 {
		t.Fatalf("health checks = %d, want 2 (partial snapshot must not be cached)", got)
	}
}

// A client that disconnects must not cancel the shared health fan-out, and the
// completed snapshot is still cached for other probes.
func TestReadiness_CanceledClientDoesNotPoisonSnapshot(t *testing.T) {
	fast := &mockProvider{
		name:        "openai",
		models:      []string{"gpt-4o"},
		healthDelay: 10 * time.Millisecond,
	}
	handler := proxy.NewReadinessHandlerWithOptions(
		proxy.NewRegistry([]provider.Provider{fast}),
		nil,
		proxy.HealthOptions{
			CheckTimeout:      time.Second,
			ProviderTimeout:   500 * time.Millisecond,
			ReadinessCacheTTL: time.Minute,
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("canceled client ctx broke the probe: got %d body %s", w.Code, w.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("second probe got %d body %s", w2.Code, w2.Body.String())
	}
	if got := fast.healthCalls.Load(); got != 1 {
		t.Fatalf("health checks = %d, want 1 (second probe should hit the cache)", got)
	}
}

// /health/providers must fail with 503 when a provider is degraded so status
// code based alerting sees the outage.
func TestHealthProviders_DegradedReturns503(t *testing.T) {
	bad := &mockProvider{name: "openai", models: []string{"gpt-4o"}, healthErr: errors.New("connection refused")}
	good := &mockProvider{name: "anthropic", models: []string{"claude-sonnet-4-6"}}
	handler := proxy.NewHealthProvidersHandler(proxy.NewRegistry([]provider.Provider{bad, good}))

	req := httptest.NewRequest(http.MethodGet, "/health/providers", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["overall"] != "degraded" {
		t.Fatalf("overall = %v, want degraded", body["overall"])
	}
	if openai, ok := body["openai"].(map[string]any); !ok || openai["status"] != "unhealthy" {
		t.Fatalf("openai status missing or wrong: %v", body["openai"])
	}
}

func TestHealthProviders_AllHealthyReturns200(t *testing.T) {
	good := &mockProvider{name: "openai", models: []string{"gpt-4o"}}
	handler := proxy.NewHealthProvidersHandler(proxy.NewRegistry([]provider.Provider{good}))

	req := httptest.NewRequest(http.MethodGet, "/health/providers", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["overall"] != "healthy" {
		t.Fatalf("overall = %v, want healthy", body["overall"])
	}
}
