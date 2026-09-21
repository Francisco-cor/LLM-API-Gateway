package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
)

func TestMetricPathBoundsUnknownRoutes(t *testing.T) {
	if got := metricPath("/v1/chat/completions"); got != "/v1/chat/completions" {
		t.Fatalf("known route normalized to %q", got)
	}
	if got := metricPath("/tenant/secret/123"); got != "/other" {
		t.Fatalf("unknown route normalized to %q, want /other", got)
	}
}

func TestRateLimitWithEnabledCanToggle(t *testing.T) {
	limiter := ratelimit.New(60, 1)
	defer limiter.Close()
	enabled := &atomic.Bool{}
	nextCalls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	h := RateLimitWithEnabled(limiter, nil, enabled, next)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || nextCalls != 1 {
		t.Fatalf("disabled limiter: status=%d nextCalls=%d", w.Code, nextCalls)
	}

	enabled.Store(true)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || nextCalls != 2 {
		t.Fatalf("enabled limiter first request: status=%d nextCalls=%d", w.Code, nextCalls)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests || nextCalls != 2 {
		t.Fatalf("enabled limiter second request: status=%d nextCalls=%d", w.Code, nextCalls)
	}

	enabled.Store(false)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || nextCalls != 3 {
		t.Fatalf("re-disabled limiter: status=%d nextCalls=%d", w.Code, nextCalls)
	}
}
