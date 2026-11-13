package auth

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

type contextKey string

const tenantKey contextKey = "tenant"
const scopesKey contextKey = "scopes"

const (
	defaultFailureLimit  = 20
	defaultFailureWindow = time.Minute
)

// Options configures the auth middleware. Zero values use safe defaults.
type Options struct {
	// Enabled is consulted per request so SIGHUP/admin reload can toggle auth
	// without rebuilding the HTTP server. A nil value means always enabled.
	Enabled *atomic.Bool
	// FailureLimit is the number of failed attempts allowed per client IP and
	// window before returning 429. <=0 disables the limiter.
	FailureLimit int
	// FailureWindow bounds the counting window. 0 uses one minute.
	FailureWindow time.Duration
	// Now is an optional clock for tests.
	Now func() time.Time
}

// Middleware enforces Bearer auth on /v1/* paths, allowing /health* passthrough.
// It keeps the historical always-enabled behavior.
func Middleware(store *Store, next http.Handler) http.Handler {
	enabled := &atomic.Bool{}
	enabled.Store(true)
	return MiddlewareWithEnabled(enabled, store, next)
}

// MiddlewareWithEnabled is Middleware with a reloadable enabled flag. The
// middleware is always installed; when the flag is false requests pass
// through, which makes enabling auth via SIGHUP/admin take effect immediately.
func MiddlewareWithEnabled(enabled *atomic.Bool, store *Store, next http.Handler) http.Handler {
	return MiddlewareWithOptions(store, next, Options{Enabled: enabled})
}

// MiddlewareWithOptions is the fully configurable variant.
func MiddlewareWithOptions(store *Store, next http.Handler, opts Options) http.Handler {
	limit := opts.FailureLimit
	if limit == 0 {
		limit = defaultFailureLimit
	}
	window := opts.FailureWindow
	if window <= 0 {
		window = defaultFailureWindow
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	failures := newFailureLimiter(limit, window, now)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health, metrics and non-API paths never require auth and must not be
		// blocked by the brute-force limiter.
		if isPublicPath(r.URL.Path) || !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		if opts.Enabled != nil && !opts.Enabled.Load() {
			next.ServeHTTP(w, r)
			return
		}
		scope, known := requiredScope(r)
		if !known {
			// Default deny: an unlisted /v1 route must be explicitly added to
			// the scope map, not silently treated as public or chat:write.
			writeForbiddenRoute(w, r.URL.Path)
			return
		}
		ip := clientIP(r)
		if blocked, retryAfter := failures.blocked(ip); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+0.5)))
			writeJSONError(w, http.StatusTooManyRequests, "too_many_authentication_failures", "invalid_api_key", "too many failed authentication attempts, retry later")
			return
		}
		authz := r.Header.Get("Authorization")
		if authz == "" {
			failures.record(ip)
			writeAuthError(w, "missing Authorization header")
			return
		}
		parts := strings.SplitN(authz, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			failures.record(ip)
			writeAuthError(w, "Authorization must be Bearer <token>")
			return
		}
		rawKey := strings.TrimSpace(parts[1])
		key, ok := store.Authenticate(rawKey)
		if !ok {
			failures.record(ip)
			writeAuthError(w, "invalid or expired API key")
			return
		}
		failures.reset(ip)
		if scope != "" && !key.HasScope(scope) {
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

func isPublicPath(path string) bool {
	if strings.HasPrefix(path, "/health") {
		return true
	}
	switch path {
	case "/metrics", "/livez", "/readyz":
		return true
	}
	return false
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

// requiredScope maps an API route to its scope. The second result is false for
// routes that are not explicitly allowed; callers must deny them.
func requiredScope(r *http.Request) (string, bool) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		return "models:read", true
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		return "chat:write", true
	case r.Method == http.MethodPost && r.URL.Path == "/v1/embeddings":
		return "embeddings:write", true
	default:
		return "", false
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// failureLimiter is a small per-IP counter for 401s. It is process-local on
// purpose: shared state for failed logins is not worth a Redis round trip on
// every unauthorized request, and a per-replica limit still caps brute force.
type failureLimiter struct {
	mu      sync.Mutex
	entries map[string]failureEntry
	limit   int
	window  time.Duration
	now     func() time.Time
}

type failureEntry struct {
	count int
	start time.Time
}

func newFailureLimiter(limit int, window time.Duration, now func() time.Time) *failureLimiter {
	return &failureLimiter{
		entries: make(map[string]failureEntry),
		limit:   limit,
		window:  window,
		now:     now,
	}
}

func (l *failureLimiter) blocked(ip string) (bool, time.Duration) {
	if l.limit <= 0 || ip == "" {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok {
		return false, 0
	}
	now := l.now()
	if now.Sub(e.start) >= l.window {
		delete(l.entries, ip)
		return false, 0
	}
	if e.count < l.limit {
		return false, 0
	}
	return true, l.window - now.Sub(e.start)
}

func (l *failureLimiter) record(ip string) {
	if l.limit <= 0 || ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[ip]
	if !ok || now.Sub(e.start) >= l.window {
		l.entries[ip] = failureEntry{count: 1, start: now}
		return
	}
	e.count++
	l.entries[ip] = e
	// Opportunistic cleanup so a long-running process does not accumulate one
	// entry per attacking IP forever.
	if len(l.entries) > 4096 {
		for key, entry := range l.entries {
			if now.Sub(entry.start) >= l.window {
				delete(l.entries, key)
			}
		}
	}
}

func (l *failureLimiter) reset(ip string) {
	if l.limit <= 0 || ip == "" {
		return
	}
	l.mu.Lock()
	delete(l.entries, ip)
	l.mu.Unlock()
}

func writeAuthError(w http.ResponseWriter, msg string) {
	writeJSONError(w, http.StatusUnauthorized, "invalid_api_key", "invalid_request_error", msg)
}

func writeScopeError(w http.ResponseWriter, scope string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+scope+`"`)
	writeJSONError(w, http.StatusForbidden, "insufficient_scope", "insufficient_scope", "API key is missing required scope: "+scope)
}

func writeForbiddenRoute(w http.ResponseWriter, path string) {
	writeJSONError(w, http.StatusForbidden, "insufficient_scope", "insufficient_scope", "route is not allowed by auth policy: "+path)
}

func writeJSONError(w http.ResponseWriter, status int, code, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(provider.ErrorResponse{
		Error: provider.Error{
			Message: msg,
			Type:    typ,
			Code:    code,
		},
	})
}
