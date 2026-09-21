package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
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
