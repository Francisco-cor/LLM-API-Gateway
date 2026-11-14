package idempotency

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestMiddleware(t *testing.T, status int, calls *atomic.Int64) http.Handler {
	t.Helper()
	cfg := Defaults()
	cfg.WaitTimeout = 2 * time.Second
	m := New(cfg, nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Gateway-Provider", "openai")
		w.WriteHeader(status)
		_, _ = w.Write(append([]byte(`{"echo":`), append(body, '}')...))
	})
	return Middleware(m, next)
}

func post(t *testing.T, h http.Handler, path, key, tenant, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set(headerKey, key)
	}
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func decodeCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error: %v (%s)", err, w.Body.String())
	}
	return out.Error.Code
}

func TestMiddlewareReplaysSuccessfulResponse(t *testing.T) {
	var calls atomic.Int64
	h := newTestMiddleware(t, http.StatusOK, &calls)
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`

	first := post(t, h, "/v1/chat/completions", "key-1", "tenant-a", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	if got := first.Header().Get("Idempotent-Replayed"); got != "" {
		t.Errorf("first response has replay header %q", got)
	}

	second := post(t, h, "/v1/chat/completions", "key-1", "tenant-a", body)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d", second.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", calls.Load())
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("replay body = %s, want %s", second.Body.String(), first.Body.String())
	}
	if got := second.Header().Get("Idempotent-Replayed"); got != "true" {
		t.Errorf("replay header = %q, want true", got)
	}
	if got := second.Header().Get("X-Gateway-Provider"); got != "openai" {
		t.Errorf("provider header = %q", got)
	}
}

func TestMiddlewareRejectsKeyReuse(t *testing.T) {
	var calls atomic.Int64
	h := newTestMiddleware(t, http.StatusOK, &calls)

	post(t, h, "/v1/chat/completions", "key-1", "tenant-a", `{"model":"a","messages":[]}`)
	second := post(t, h, "/v1/chat/completions", "key-1", "tenant-a", `{"model":"b","messages":[]}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", second.Code)
	}
	if code := decodeCode(t, second); code != "idempotency_key_reuse" {
		t.Fatalf("code = %q", code)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler called %d times", calls.Load())
	}
}

func TestMiddlewareScopesByTenant(t *testing.T) {
	var calls atomic.Int64
	h := newTestMiddleware(t, http.StatusOK, &calls)
	body := `{"model":"a","messages":[]}`

	post(t, h, "/v1/chat/completions", "shared", "tenant-a", body)
	second := post(t, h, "/v1/chat/completions", "shared", "tenant-b", body)
	if second.Code != http.StatusOK {
		t.Fatalf("status = %d", second.Code)
	}
	if calls.Load() != 2 {
		t.Fatalf("handler called %d times, want 2 (different tenants)", calls.Load())
	}
}

func TestMiddlewareSkipsStreaming(t *testing.T) {
	var calls atomic.Int64
	h := newTestMiddleware(t, http.StatusOK, &calls)
	body := `{"model":"a","stream":true,"messages":[]}`

	post(t, h, "/v1/chat/completions", "key-1", "tenant-a", body)
	post(t, h, "/v1/chat/completions", "key-1", "tenant-a", body)
	if calls.Load() != 2 {
		t.Fatalf("streaming requests were deduplicated: calls=%d", calls.Load())
	}
}

func TestMiddlewareDoesNotStoreErrors(t *testing.T) {
	var calls atomic.Int64
	h := newTestMiddleware(t, http.StatusBadGateway, &calls)
	body := `{"model":"a","messages":[]}`

	first := post(t, h, "/v1/chat/completions", "key-1", "tenant-a", body)
	second := post(t, h, "/v1/chat/completions", "key-1", "tenant-a", body)
	if first.Code != http.StatusBadGateway || second.Code != http.StatusBadGateway {
		t.Fatalf("statuses = %d/%d", first.Code, second.Code)
	}
	if calls.Load() != 2 {
		t.Fatalf("error responses were stored: calls=%d", calls.Load())
	}
}

func TestMiddlewareEmbeddingsReplay(t *testing.T) {
	var calls atomic.Int64
	h := newTestMiddleware(t, http.StatusOK, &calls)
	body := `{"model":"text-embedding-3-small","input":"hi"}`

	post(t, h, "/v1/embeddings", "emb-1", "tenant-a", body)
	second := post(t, h, "/v1/embeddings", "emb-1", "tenant-a", body)
	if second.Code != http.StatusOK {
		t.Fatalf("status = %d", second.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("missing replay header")
	}
}

func TestMiddlewareConcurrentDuplicatesCallOnce(t *testing.T) {
	var calls atomic.Int64
	cfg := Defaults()
	cfg.WaitTimeout = 3 * time.Second
	m := New(cfg, nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := Middleware(m, next)

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := post(t, h, "/v1/chat/completions", "same", "tenant-a", `{"model":"a"}`)
			statuses[i] = w.Code
		}(i)
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Errorf("request %d status = %d", i, status)
		}
	}
}

func TestMiddlewareInvalidKey(t *testing.T) {
	var calls atomic.Int64
	h := newTestMiddleware(t, http.StatusOK, &calls)
	w := post(t, h, "/v1/chat/completions", "bad key", "tenant-a", `{"model":"a"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	if code := decodeCode(t, w); code != "invalid_idempotency_key" {
		t.Fatalf("code = %q", code)
	}
	if calls.Load() != 0 {
		t.Fatalf("handler was called")
	}
}

func TestMiddlewareIgnoresOtherPaths(t *testing.T) {
	var calls atomic.Int64
	h := newTestMiddleware(t, http.StatusOK, &calls)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set(headerKey, "key-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d", calls.Load())
	}
}

func TestMiddlewareRestoresBody(t *testing.T) {
	var got []byte
	h := Middleware(New(Defaults(), nil), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	body := `{"model":"a","messages":[]}`
	post(t, h, "/v1/chat/completions", "key-1", "tenant-a", body)
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("handler body = %s", got)
	}
}
