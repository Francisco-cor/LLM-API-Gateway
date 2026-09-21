package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

type contextKey string

const tenantKey contextKey = "tenant"
const scopesKey contextKey = "scopes"

// Middleware enforces Bearer auth on /v1/* paths, allowing /health* passthrough.
func Middleware(store *Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// allow health checks without auth
		if strings.HasPrefix(r.URL.Path, "/health") || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "" {
			writeAuthError(w, "missing Authorization header")
			return
		}
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeAuthError(w, "Authorization must be Bearer <token>")
			return
		}
		rawKey := strings.TrimSpace(parts[1])
		key, ok := store.Authenticate(rawKey)
		if !ok {
			writeAuthError(w, "invalid or expired API key")
			return
		}
		if scope := requiredScope(r); scope != "" && !key.HasScope(scope) {
			writeScopeError(w, scope)
			return
		}
		// Ignore any client-supplied tenant header. The authenticated key is the
		// sole authority for tenant identity when auth is enabled.
		ctx := context.WithValue(r.Context(), tenantKey, key.Tenant)
		ctx = context.WithValue(ctx, scopesKey, append([]string(nil), key.Scopes...))
		// also set tenant header for rate limiter downstream (optional)
		r.Header.Set("X-Tenant-ID", key.Tenant)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TenantFromContext returns tenant id if set.
func TenantFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tenantKey).(string); ok {
		return v
	}
	return ""
}

// ScopesFromContext returns a copy of the authenticated key scopes.
func ScopesFromContext(ctx context.Context) []string {
	if v, ok := ctx.Value(scopesKey).([]string); ok {
		return append([]string(nil), v...)
	}
	return nil
}

func requiredScope(r *http.Request) string {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		return "models:read"
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		return "chat:write"
	case r.Method == http.MethodPost && r.URL.Path == "/v1/embeddings":
		return "embeddings:write"
	default:
		return ""
	}
}

func writeAuthError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(provider.ErrorResponse{
		Error: provider.Error{
			Message: msg,
			Type:    "invalid_request_error",
			Code:    "invalid_api_key",
		},
	})
}

func writeScopeError(w http.ResponseWriter, scope string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+scope+`"`)
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(provider.ErrorResponse{
		Error: provider.Error{
			Message: "API key is missing required scope: " + scope,
			Type:    "insufficient_scope",
			Code:    "insufficient_scope",
		},
	})
}
