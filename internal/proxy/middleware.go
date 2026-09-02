package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
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
				w.Header().Set("Access-Control-Allow-Origin", origin)
				if origin == "" && allowed["*"] {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID")
			}
			if r.Method == http.MethodOptions {
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
		path := r.URL.Path
		// Normalize path to avoid cardinality explosion: keep /v1/chat/completions, /health*, /metrics
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
			ctx, span := tracer.Start(ctx, r.Method+" "+r.URL.Path,
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.route", r.URL.Path),
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
func RateLimit(limiter *ratelimit.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		if key == "" {
			key = r.RemoteAddr
		}

		if !limiter.Allow(key) {
			retryAfter := limiter.RetryAfter(key)
			w.Header().Set("Retry-After", fmt.Sprintf("%.0f", retryAfter.Seconds()))
			writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded, retry later")
			return
		}

		next.ServeHTTP(w, r)
	})
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
			Code:    fmt.Sprintf("%d", status),
		},
	})
}
