package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/auth"
	"github.com/fcordero/llm-api-gateway/internal/config"
)

func TestAuth_EnforcesEndpointScopes(t *testing.T) {
	store := auth.New([]config.APIKeyConfig{{
		Key:    "chat-key",
		Tenant: "tenant-a",
		Scopes: []string{"chat:write"},
	}})
	wrapped := auth.Middleware(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer chat-key")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("models without models:read got %d, want 403", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer chat-key")
	w = httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("chat with chat:write got %d, want 204", w.Code)
	}
	if got := req.Header.Get("X-Tenant-ID"); got != "tenant-a" {
		t.Errorf("tenant header %q, want tenant-a", got)
	}
}

func TestAuth_ExpiredKeyRejected(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	store := auth.New([]config.APIKeyConfig{{Key: "expired", ExpiresAt: &expired}})
	if _, ok := store.Authenticate("expired"); ok {
		t.Fatal("expired key authenticated")
	}
	if auth.TenantFromContext(context.Background()) != "" {
		t.Fatal("empty context unexpectedly has tenant")
	}
}

func TestAuth_RouteNotInPolicyIsDenied(t *testing.T) {
	store := auth.New([]config.APIKeyConfig{{
		Key:    "all-scopes-key",
		Tenant: "tenant-a",
		Scopes: []string{"chat:write", "models:read", "embeddings:write", "*"},
	}})
	wrapped := auth.Middleware(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// An authenticated key with "*" still cannot reach a route that auth does
	// not know about: default deny is about the route, not the key.
	req := httptest.NewRequest(http.MethodPost, "/v1/files", nil)
	req.Header.Set("Authorization", "Bearer all-scopes-key")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unmapped /v1 route got %d, want 403", w.Code)
	}
}

func TestAuth_KeyWithoutScopesIsDenied(t *testing.T) {
	store := auth.New([]config.APIKeyConfig{{Key: "scopeless", Tenant: "tenant-a"}})
	wrapped := auth.Middleware(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer scopeless")
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("key without scopes got %d, want 403", w.Code)
	}
}

func TestAuth_EnabledFlagHotReload(t *testing.T) {
	enabled := &atomic.Bool{}
	store := auth.New([]config.APIKeyConfig{{
		Key:    "reload-key",
		Tenant: "tenant-a",
		Scopes: []string{"chat:write"},
	}})
	wrapped := auth.MiddlewareWithEnabled(enabled, store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// Disabled: no header required.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("auth disabled got %d, want 204", w.Code)
	}

	// Enabling via the atomic flag takes effect on the next request.
	enabled.Store(true)
	w = httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("auth enabled without key got %d, want 401", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer reload-key")
	w = httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("auth enabled with valid key got %d, want 204", w.Code)
	}
}

func TestAuth_KeyReloadTakesEffectImmediately(t *testing.T) {
	enabled := &atomic.Bool{}
	enabled.Store(true)
	store := auth.New([]config.APIKeyConfig{{Key: "old-key", Tenant: "a", Scopes: []string{"chat:write"}}})
	wrapped := auth.MiddlewareWithEnabled(enabled, store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	do := func(key string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)
		return w.Code
	}
	if got := do("old-key"); got != http.StatusNoContent {
		t.Fatalf("old key before reload got %d", got)
	}

	store.ReloadWithPepper([]config.APIKeyConfig{{Key: "new-key", Tenant: "a", Scopes: []string{"chat:write"}}}, "pepper")
	if got := do("old-key"); got != http.StatusUnauthorized {
		t.Fatalf("rotated-out key got %d, want 401", got)
	}
	if got := do("new-key"); got != http.StatusNoContent {
		t.Fatalf("rotated-in key got %d, want 204", got)
	}
}

func TestAuth_FailedAttemptsAreRateLimitedPerIP(t *testing.T) {
	store := auth.New([]config.APIKeyConfig{{Key: "valid-key", Tenant: "a", Scopes: []string{"chat:write"}}})
	wrapped := auth.MiddlewareWithOptions(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), auth.Options{FailureLimit: 2, FailureWindow: time.Minute})

	do := func(remoteAddr, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.RemoteAddr = remoteAddr
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)
		return w
	}

	// Two failures fill the bucket for this IP.
	if got := do("203.0.113.9:1111", "bad"); got.Code != http.StatusUnauthorized {
		t.Fatalf("first failure got %d, want 401", got.Code)
	}
	if got := do("203.0.113.9:1112", "bad"); got.Code != http.StatusUnauthorized {
		t.Fatalf("second failure got %d, want 401", got.Code)
	}
	// Even a valid key is rejected while the IP is blocked.
	blocked := do("203.0.113.9:1113", "valid-key")
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked IP with valid key got %d, want 429", blocked.Code)
	}
	if blocked.Header().Get("Retry-After") == "" {
		t.Fatal("429 response is missing Retry-After")
	}
	// Other clients are unaffected.
	if got := do("198.51.100.7:2222", "valid-key"); got.Code != http.StatusNoContent {
		t.Fatalf("different IP got %d, want 204", got.Code)
	}
	// A successful authentication resets the counter for its IP.
	if got := do("198.51.100.7:2223", "bad"); got.Code != http.StatusUnauthorized {
		t.Fatalf("new failure got %d, want 401", got.Code)
	}
	if got := do("198.51.100.7:2224", "valid-key"); got.Code != http.StatusNoContent {
		t.Fatalf("successful auth after reset got %d, want 204", got.Code)
	}
}
