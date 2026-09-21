package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/metrics"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// statusRecorder captures the status code written by the wrapped handler so
// it can be logged after the response is sent.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach optional capabilities on the
// underlying writer, while the explicit methods below keep compatibility with
// handlers that use type assertions (notably SSE streaming).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (r *statusRecorder) Push(target string, opts *http.PushOptions) error {
	p, ok := r.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

func (r *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(r.ResponseWriter, src)
}

// RequestID ensures every request carries an X-Request-ID header, generating
// one if the caller didn't supply it, and reflects it back in the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = generateRequestID()
			r.Header.Set("X-Request-ID", id)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

// Logging logs each request's method, path, status, latency, and request ID.
// It also extracts tenant/provider/trace_id for contextual observability (Fase 5).
func Logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"latency_ms", time.Since(start).Milliseconds(),
			"request_id", r.Header.Get("X-Request-ID"),
		}
		if tenant := r.Header.Get("X-Tenant-ID"); tenant != "" {
			attrs = append(attrs, "tenant", tenant)
		}
		if prov := rec.Header().Get("X-Gateway-Provider"); prov != "" {
			attrs = append(attrs, "provider", prov)
		} else if prov := w.Header().Get("X-Gateway-Provider"); prov != "" {
			attrs = append(attrs, "provider", prov)
		}
		if span := trace.SpanFromContext(r.Context()); span.SpanContext().IsValid() {
			attrs = append(attrs, "trace_id", span.SpanContext().TraceID().String())
		}
		log.Info("request", attrs...)
	})
}

// SecurityHeaders adds baseline security headers.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// CORS returns middleware that handles CORS headers. If allowedOrigins is empty, CORS is disabled.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(allowedOrigins) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			origin := r.Header.Get("Origin")
			if allowed["*"] || allowed[origin] {
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Origin", origin)
				if origin == "" && allowed["*"] {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID")
			}
			if r.Method == http.MethodOptions {
				if origin != "" && !allowed["*"] && !allowed[origin] {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Metrics records Prometheus metrics per request (Fase 5).
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		duration := time.Since(start).Seconds()
		provider := rec.Header().Get("X-Gateway-Provider")
		if provider == "" {
			provider = w.Header().Get("X-Gateway-Provider")
		}
		if provider == "" {
			provider = "unknown"
		}
		path := metricPath(r.URL.Path)
		metrics.RequestsTotal.WithLabelValues(r.Method, path, strconv.Itoa(rec.status), provider).Inc()
		metrics.RequestDuration.WithLabelValues(r.Method, path, provider).Observe(duration)
		if rec.status >= 500 {
			metrics.ProviderErrors.WithLabelValues(provider, strconv.Itoa(rec.status)).Inc()
		}
	})
}

// Tracing creates an OTEL span per HTTP request and propagates traceparent (Fase 5).
func Tracing(serviceName string) func(http.Handler) http.Handler {
	tracer := otel.Tracer(serviceName)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			route := metricPath(r.URL.Path)
			ctx, span := tracer.Start(ctx, r.Method+" "+route,
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.route", route),
				),
				trace.WithSpanKind(trace.SpanKindServer),
			)
			defer span.End()

			// Inject traceparent into response for debugging
			otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(w.Header()))

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			span.SetAttributes(
				attribute.Int("http.status_code", rec.status),
				attribute.String("gateway.provider", rec.Header().Get("X-Gateway-Provider")),
			)
			if rec.status >= 500 {
				span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", rec.status))
			}
		})
	}
}

// RateLimit enforces a per-API-key token bucket, identifying clients by their
// Authorization header (falling back to remote address if absent).
func RateLimit(limiter ratelimit.Backend, next http.Handler) http.Handler {
	return RateLimitWithOverrides(limiter, nil, next)
}

// RateLimitWithOverrides applies the model-specific RPM/burst override before
// the downstream handler consumes the request body. The body is restored so
// request decoding remains the handler's responsibility.
func RateLimitWithOverrides(limiter ratelimit.Backend, overrides *ratelimit.OverrideStore, next http.Handler) http.Handler {
	return RateLimitWithEnabled(limiter, overrides, nil, next)
}

// RateLimitWithEnabled keeps the middleware in the chain while allowing the
// control plane to enable or disable it without rebuilding the HTTP server.
// A nil flag preserves the historical always-enabled behavior.
func RateLimitWithEnabled(limiter ratelimit.Backend, overrides *ratelimit.OverrideStore, enabled *atomic.Bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if enabled != nil && !enabled.Load() {
			next.ServeHTTP(w, r)
			return
		}
		// Health, readiness and metrics must remain probeable when traffic is
		// exhausted. User-facing API routes are the only rate-limited surface.
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		key := clientRateLimitKey(r)
		rpm, burst := 0, 0
		if overrides != nil && r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/embeddings") {
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
			r.Body = io.NopCloser(bytes.NewReader(body))
			if err == nil {
				var meta struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(body, &meta) == nil && meta.Model != "" {
					rpm, burst = overrides.Resolve(r.Header.Get("X-Tenant-ID"), meta.Model)
				}
			}
		}

		var allowed bool
		var retryAfter time.Duration
		if rpm > 0 && burst > 0 {
			allowed = limiter.AllowWithLimits(key, 1, rpm, burst)
			retryAfter = limiter.RetryAfterWithLimits(key, rpm, burst)
		} else {
			allowed = limiter.Allow(key)
			retryAfter = limiter.RetryAfter(key)
		}
		if !allowed {
			seconds := int64(retryAfter / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
			writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded, retry later")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// metricPath bounds the path label. Unknown paths are grouped together so a
// client cannot create an unbounded number of Prometheus time series by
// varying URL paths.
func metricPath(path string) string {
	switch path {
	case "/v1/chat/completions", "/v1/embeddings", "/v1/models",
		"/health", "/health/providers", "/livez", "/readyz", "/metrics":
		return path
	default:
		return "/other"
	}
}

// clientRateLimitKey hashes credentials before they enter a long-lived bucket
// map or Redis key. This preserves per-client isolation without retaining raw
// bearer tokens in process memory or external storage.
func clientRateLimitKey(r *http.Request) string {
	identity := r.Header.Get("X-Tenant-ID")
	if identity == "" {
		identity = r.Header.Get("Authorization")
	}
	if identity == "" {
		identity = r.RemoteAddr
	}
	sum := sha256.Sum256([]byte(identity))
	return "client:" + hex.EncodeToString(sum[:])
}

func generateRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(b)
}

// writeError writes an OpenAI-compatible error response.
func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(provider.ErrorResponse{
		Error: provider.Error{
			Message: message,
			Type:    errType,
			Code:    errType,
		},
	})
}
