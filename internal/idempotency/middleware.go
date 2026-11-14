package idempotency

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/metrics"
)

const (
	headerKey = "Idempotency-Key"
	// maxBody mirrors the data-plane limit; larger bodies are left to the
	// handler so it can return its own 413.
	maxBody = 8 << 20
)

// Middleware deduplicates POST chat/embeddings requests that carry an
// Idempotency-Key. Streaming requests bypass it: replaying SSE would violate
// the streaming contract.
func Middleware(m *Manager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m == nil || !m.Config().Enabled {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || !idempotentPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		key := strings.TrimSpace(r.Header.Get(headerKey))
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !validKey(key) {
			metrics.IdempotencyRequests.WithLabelValues("invalid").Inc()
			writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must be 1-255 printable ASCII characters")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if len(body) > maxBody {
			metrics.IdempotencyRequests.WithLabelValues("bypass").Inc()
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/v1/chat/completions" && streams(body) {
			metrics.IdempotencyRequests.WithLabelValues("bypass").Inc()
			next.ServeHTTP(w, r)
			return
		}

		storageKey := Key(tenantID(r), r.URL.Path, key)
		fingerprint := Fingerprint(body)
		reservation, outcome, entry := m.Begin(r.Context(), storageKey, fingerprint)
		switch outcome {
		case Replay:
			metrics.IdempotencyRequests.WithLabelValues("replay").Inc()
			writeEntry(w, entry)
			return
		case Conflict:
			metrics.IdempotencyRequests.WithLabelValues("conflict").Inc()
			writeError(w, http.StatusConflict, "idempotency_key_reuse", "Idempotency-Key was already used with a different request body")
			return
		case InProgress:
			metrics.IdempotencyRequests.WithLabelValues("in_progress").Inc()
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusConflict, "idempotency_in_progress", "a request with this Idempotency-Key is still in progress")
			return
		}

		if reservation == nil {
			next.ServeHTTP(w, r)
			return
		}

		defer func() {
			if reservation != nil {
				reservation.Abort()
			}
		}()
		metrics.IdempotencyRequests.WithLabelValues("miss").Inc()
		cw := &captureWriter{w: w, max: m.Config().EntryMaxBytes}
		next.ServeHTTP(cw, r)
		if reservation != nil && cw.storable() {
			// The client may disconnect right after reading the response;
			// storing it must still succeed. Redis operations have their own
			// short timeout.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
			defer cancel()
			reservation.Complete(ctx, Entry{
				Status:  cw.statusCode(),
				Headers: cw.storedHeaders(),
				Body:    cw.body.Bytes(),
			})
		}
	})
}

func idempotentPath(path string) bool {
	return path == "/v1/chat/completions" || path == "/v1/embeddings"
}

func validKey(key string) bool {
	if len(key) == 0 || len(key) > 255 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// tenantID scopes keys to the authenticated identity so one tenant cannot
// replay another tenant's response. Auth sets X-Tenant-ID; when auth is off,
// the Authorization header (if any) is hashed instead of trusting a client
// supplied tenant header.
func tenantID(r *http.Request) string {
	if tenant := strings.TrimSpace(r.Header.Get("X-Tenant-ID")); tenant != "" {
		return tenant
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		return Fingerprint([]byte(auth))
	}
	return "anonymous"
}

func streams(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false // invalid JSON: let the handler return its 400
	}
	return probe.Stream
}

func writeEntry(w http.ResponseWriter, entry Entry) {
	for name, values := range entry.Headers {
		if skipHeader(name) {
			continue
		}
		w.Header().Del(name)
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.Header().Set("Idempotent-Replayed", "true")
	w.Header().Set("Content-Length", strconv.Itoa(len(entry.Body)))
	w.WriteHeader(entry.Status)
	_, _ = w.Write(entry.Body)
}

func skipHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Content-Length", "Date", "Connection", "Keep-Alive", "Transfer-Encoding", "Idempotent-Replayed":
		return true
	}
	return false
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
}

// captureWriter passes bytes through immediately and keeps a bounded copy for
// a possible replay. Responses above EntryMaxBytes are not stored.
type captureWriter struct {
	w        http.ResponseWriter
	status   int
	headers  http.Header
	body     bytes.Buffer
	max      int64
	overflow bool
}

func (c *captureWriter) Header() http.Header { return c.w.Header() }

func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
		c.headers = cloneHeader(c.w.Header())
		c.w.WriteHeader(status)
	}
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
		c.headers = cloneHeader(c.w.Header())
	}
	if !c.overflow {
		if int64(c.body.Len()+len(p)) > c.max {
			c.overflow = true
			c.body.Reset()
		} else {
			_, _ = c.body.Write(p)
		}
	}
	return c.w.Write(p)
}

// Unwrap lets http.ResponseController reach the real writer (for example to
// set a write deadline).
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.w }

func (c *captureWriter) Flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *captureWriter) statusCode() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}

func (c *captureWriter) storable() bool {
	status := c.statusCode()
	return !c.overflow && status >= 200 && status < 300
}

func (c *captureWriter) storedHeaders() map[string][]string {
	if c.headers == nil {
		c.headers = cloneHeader(c.w.Header())
	}
	return cloneHeader(c.headers)
}

func cloneHeader(h http.Header) map[string][]string {
	if h == nil {
		return nil
	}
	out := make(map[string][]string, len(h))
	for name, values := range h {
		if skipHeader(name) {
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}
