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
		ctx := context.WithValue(r.Context(), tenantKey, key.Tenant)
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
