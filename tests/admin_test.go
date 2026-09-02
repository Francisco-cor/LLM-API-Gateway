package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/admin"
	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
	"io"
	"log/slog"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func minimalValidConfig() string {
	return `
server:
  port: 8080
  read_timeout: 30s
  write_timeout: 60s
providers:
  openai:
    api_key: test-key
    base_url: https://api.openai.com/v1
    timeout: 30s
    models: [gpt-4o]
fallback_chain: [openai]
admin:
  port: 8081
  api_key: secret123
logging:
  level: info
  format: json
`
}

func TestAdmin_Auth(t *testing.T) {
	cfg, err := config.Load(writeTempConfig(t, minimalValidConfig()))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	registry := proxy.NewRegistry([]provider.Provider{
		&mockProvider{name: "openai", models: []string{"gpt-4o"}},
	})
	srv := admin.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "secret123", "", cfg, registry, nil, nil)
	h := srv.Handler()

	// no auth => 401
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth got %d want 401", w.Code)
	}
	// wrong key => 401
	req = httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong key got %d want 401", w.Code)
	}
	// correct Bearer => 200
	req = httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("correct auth got %d want 200 body %s", w.Code, w.Body.String())
	}
	// X-Admin-API-Key header => 200
	req = httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.Header.Set("X-Admin-API-Key", "secret123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("X-Admin-API-Key got %d want 200", w.Code)
	}
	// auth disabled when apiKey empty => allow without header
	srv2 := admin.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "", "", cfg, registry, nil, nil)
	h2 := srv2.Handler()
	req = httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("no apiKey disabled got %d want 200", w.Code)
	}
}

func TestAdmin_GetConfigRedacted(t *testing.T) {
	cfg, _ := config.Load(writeTempConfig(t, minimalValidConfig()))
	registry := proxy.NewRegistry([]provider.Provider{&mockProvider{name: "openai", models: []string{"gpt-4o"}}})
	srv := admin.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "secret123", "", cfg, registry, nil, nil)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/config %d", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	// providers api_key should be redacted
	provs, ok := body["Providers"].(map[string]any)
	if !ok {
		t.Fatalf("Providers not in body %v", body)
	}
	openai, ok := provs["openai"].(map[string]any)
	if !ok {
		t.Fatalf("openai not found")
	}
	if openai["APIKey"] != "***" {
		t.Errorf("APIKey not redacted: %v", openai["APIKey"])
	}
}

func TestAdmin_ReloadRollback(t *testing.T) {
	valid := minimalValidConfig()
	path := writeTempConfig(t, valid)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	registry := proxy.NewRegistry([]provider.Provider{&mockProvider{name: "openai", models: []string{"gpt-4o"}}})
	// track apply calls
	var applyCount int
	var mu sync.Mutex
	apply := func(newCfg *config.Config) error {
		mu.Lock()
		applyCount++
		mu.Unlock()
		// simulate live apply: update registry if needed (for test, just succeed)
		return nil
	}
	srv := admin.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "secret123", path, cfg, registry, apply, nil)
	h := srv.Handler()

	// invalid config: server.port out of range
	invalid := `
server:
  port: 99999
providers:
  openai:
    api_key: test
    base_url: https://api.openai.com/v1
    timeout: 30s
    models: [gpt-4o]
fallback_chain: [openai]
admin:
  port: 8081
  api_key: secret123
`
	if err := os.WriteFile(path, []byte(invalid), 0644); err != nil {
		t.Fatalf("write invalid: %v", err)
	}
	// POST reload should fail 400
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid reload got %d want 400 body %s", w.Code, w.Body.String())
	}
	// GET config should still be old valid (port 8080)
	req = httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	srvMap, _ := got["Server"].(map[string]any)
	if srvMap["Port"].(float64) != 8080 {
		t.Errorf("after failed reload port %v want 8080 (rollback)", srvMap["Port"])
	}
	// valid reload should succeed
	if err := os.WriteFile(path, []byte(valid), 0644); err != nil {
		t.Fatalf("write valid: %v", err)
	}
	// need to wait a tiny for file to settle? POST will Load directly, not watch.
	req = httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid reload got %d want 200 body %s", w.Code, w.Body.String())
	}
	mu.Lock()
	c := applyCount
	mu.Unlock()
	if c == 0 {
		t.Error("apply not called on valid reload")
	}
	// restore valid for other tests
	_ = os.WriteFile(path, []byte(valid), 0644)
}

func TestAdmin_PatchRuntime(t *testing.T) {
	cfg, _ := config.Load(writeTempConfig(t, minimalValidConfig()))
	registry := proxy.NewRegistry([]provider.Provider{&mockProvider{name: "openai", models: []string{"gpt-4o"}}})
	var applied *config.Config
	var mu sync.Mutex
	apply := func(newCfg *config.Config) error {
		mu.Lock()
		applied = newCfg
		mu.Unlock()
		return nil
	}
	srv := admin.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "secret123", "", cfg, registry, apply, nil)
	h := srv.Handler()

	// valid patch: change rate_limit burst and cache ttl
	patch := map[string]any{
		"rate_limit": map[string]any{"burst": 99},
		"cache":      map[string]any{"ttl": "10m"},
		"resilience": map[string]any{"circuit": map[string]any{"failure_threshold": 10}},
	}
	body, _ := json.Marshal(patch)
	req := httptest.NewRequest(http.MethodPatch, "/admin/config", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH got %d want 200 body %s", w.Code, w.Body.String())
	}
	mu.Lock()
	if applied == nil || applied.RateLimit.Burst != 99 {
		t.Errorf("patch not applied burst %v", applied)
	}
	if applied.Cache.TTL != 10*time.Minute {
		t.Errorf("cache ttl %v want 10m", applied.Cache.TTL)
	}
	if applied.Resilience.Circuit.FailureThreshold != 10 {
		t.Errorf("circuit threshold %v want 10", applied.Resilience.Circuit.FailureThreshold)
	}
	mu.Unlock()

	// GET should reflect new values
	req = httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	rl, _ := got["RateLimit"].(map[string]any)
	if rl["Burst"].(float64) != 99 {
		t.Errorf("GET after patch burst %v want 99", rl["Burst"])
	}

	// invalid patch: bad duration
	patch2 := map[string]any{"cache": map[string]any{"ttl": "not-a-duration"}}
	body, _ = json.Marshal(patch2)
	req = httptest.NewRequest(http.MethodPatch, "/admin/config", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid patch got %d want 400", w.Code)
	}
	// config should remain previous valid (burst 99)
	req = httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	rl, _ = got["RateLimit"].(map[string]any)
	if rl["Burst"].(float64) != 99 {
		t.Errorf("after invalid patch burst %v want 99 (rollback)", rl["Burst"])
	}

	// invalid patch: validation fails (burst negative)
	patch3 := map[string]any{"rate_limit": map[string]any{"burst": -1}}
	body, _ = json.Marshal(patch3)
	req = httptest.NewRequest(http.MethodPatch, "/admin/config", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("negative burst patch got %d want 400", w.Code)
	}
}

func TestAdmin_ConcurrentReloadSafety(t *testing.T) {
	cfg, _ := config.Load(writeTempConfig(t, minimalValidConfig()))
	registry := proxy.NewRegistry([]provider.Provider{&mockProvider{name: "openai", models: []string{"gpt-4o"}}})
	var mu sync.Mutex
	var count int
	apply := func(newCfg *config.Config) error {
		mu.Lock()
		count++
		mu.Unlock()
		// simulate small work
		time.Sleep(10 * time.Millisecond)
		return nil
	}
	srv := admin.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "secret123", "", cfg, registry, apply, nil)
	h := srv.Handler()

	var wg sync.WaitGroup
	errs := make([]int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			patch := map[string]any{"rate_limit": map[string]any{"burst": 10 + idx}}
			body, _ := json.Marshal(patch)
			req := httptest.NewRequest(http.MethodPatch, "/admin/config", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret123")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			errs[idx] = w.Code
		}(i)
	}
	wg.Wait()
	for i, code := range errs {
		if code != http.StatusOK && code != http.StatusBadRequest {
			t.Errorf("concurrent %d got %d", i, code)
		}
	}
	// final GET should be valid
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("final GET %d", w.Code)
	}
	mu.Lock()
	c := count
	mu.Unlock()
	if c != 20 {
		t.Errorf("apply count %d want 20", c)
	}
}

func TestAdmin_GetProviders(t *testing.T) {
	cfg, _ := config.Load(writeTempConfig(t, minimalValidConfig()))
	registry := proxy.NewRegistry([]provider.Provider{
		&mockProvider{name: "openai", models: []string{"gpt-4o", "text-embedding-3-small"}},
		&mockProvider{name: "gemini", models: []string{"gemini-2.5-flash"}},
	})
	srv := admin.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "secret123", "", cfg, registry, nil, nil)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/admin/providers", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/providers %d", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	provs, ok := body["providers"].([]any)
	// admin returns {"providers": [...]} lower case for this endpoint (custom)
	if !ok {
		// fallback to Providers capital if marshaled via config struct? but providers endpoint uses custom struct with json tag "providers"
		provs, ok = body["Providers"].([]any)
	}
	if !ok || len(provs) != 2 {
		t.Fatalf("providers %v", body["providers"])
	}
}

func TestAdmin_WatchFile(t *testing.T) {
	// test config.Watch polling detects change
	valid := minimalValidConfig()
	path := writeTempConfig(t, valid)
	cfg, _ := config.Load(path)
	var mu sync.Mutex
	var seen *config.Config
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = config.Watch(ctx, path, 200*time.Millisecond, func(newCfg *config.Config) error {
			mu.Lock()
			seen = newCfg
			mu.Unlock()
			return nil
		})
	}()
	// wait a bit then modify file to change fallback_chain
	time.Sleep(300 * time.Millisecond)
	modified := `
server:
  port: 8080
providers:
  openai:
    api_key: test-key
    base_url: https://api.openai.com/v1
    timeout: 30s
    models: [gpt-4o, gpt-4o-mini]
fallback_chain: [openai]
admin:
  port: 8081
  api_key: secret123
`
	if err := os.WriteFile(path, []byte(modified), 0644); err != nil {
		t.Fatalf("write modified: %v", err)
	}
	// wait for watcher to pick up (poll interval 200ms + debounce 100ms)
	time.Sleep(800 * time.Millisecond)
	mu.Lock()
	got := seen
	mu.Unlock()
	if got == nil {
		t.Fatal("watch did not detect change")
	}
	if len(got.Providers["openai"].Models) != 2 {
		t.Errorf("watched models %v want 2", got.Providers["openai"].Models)
	}
	_ = cfg // avoid unused
}

func TestAdmin_ReloadViaFileWatcherRollback(t *testing.T) {
	// Ensure invalid file change does not trigger onChange
	valid := minimalValidConfig()
	path := writeTempConfig(t, valid)
	var called bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = config.Watch(ctx, path, 200*time.Millisecond, func(newCfg *config.Config) error {
			called = true
			return nil
		})
	}()
	time.Sleep(300 * time.Millisecond)
	// write invalid yaml
	if err := os.WriteFile(path, []byte("not: yaml: [bad"), 0644); err != nil {
		t.Fatalf("write invalid: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if called {
		t.Error("watch should not call onChange for invalid yaml")
	}
}
