package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBuildPricingCatalogFallbackPerToken guards the P0 pricing bug: the
// gateway's cost_per_token_usd setting is USD per token, so 1M tokens at
// 0.00001 must cost 10 USD, not 0.00001 USD.
func TestBuildPricingCatalogFallbackPerToken(t *testing.T) {
	cfg := &config.Config{}
	cfg.RateLimit.Budget = &config.BudgetConfig{CostPerTokenUSD: 0.00001}

	catalog, err := buildPricingCatalog(cfg)
	if err != nil {
		t.Fatalf("buildPricingCatalog: %v", err)
	}
	est, err := catalog.Chat("openai", "unknown-model", 1_000_000, 0)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if est.USD != 10 {
		t.Fatalf("1M tokens at 0.00001/token = %v USD, want 10", est.USD)
	}
	if est.Known {
		t.Fatal("unknown model should be reported as fallback-priced")
	}
	if got := catalog.FallbackPerTokenUSD(); got != 0.00001 {
		t.Fatalf("fallback per token = %v, want 0.00001", got)
	}
}

func TestBuildPricingCatalogNoBudgetUsesDocumentedDefault(t *testing.T) {
	catalog, err := buildPricingCatalog(&config.Config{})
	if err != nil {
		t.Fatalf("buildPricingCatalog: %v", err)
	}
	est, err := catalog.Chat("any", "unlisted", 1_000_000, 0)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// The historical default fallback is 0.00001 USD per token.
	if est.USD != 10 {
		t.Fatalf("default fallback 1M tokens = %v USD, want 10", est.USD)
	}
}

// TestBuildPricingCatalogWiresCacheRates makes sure the reload/build path does
// not silently drop the cache read/write columns.
func TestBuildPricingCatalogWiresCacheRates(t *testing.T) {
	cfg := &config.Config{}
	cfg.RateLimit.Budget = &config.BudgetConfig{
		Pricing: config.PricingConfig{
			Currency: "USD",
			Catalog: []config.PricingRule{{
				Provider:                      "openai",
				Model:                         "gpt-4o",
				InputPerMillionTokensUSD:      5,
				OutputPerMillionTokensUSD:     15,
				CacheReadPerMillionTokensUSD:  2.5,
				CacheWritePerMillionTokensUSD: 3.75,
			}},
		},
	}
	catalog, err := buildPricingCatalog(cfg)
	if err != nil {
		t.Fatalf("buildPricingCatalog: %v", err)
	}
	est, err := catalog.ChatWithCacheTokens("openai", "gpt-4o", 1_000_000, 0, 1_000_000, 0)
	if err != nil {
		t.Fatalf("ChatWithCacheTokens: %v", err)
	}
	if est.USD != 2.5 {
		t.Fatalf("1M cache-read tokens priced at %v USD, want 2.5", est.USD)
	}
	if !est.Known {
		t.Fatal("gpt-4o should have a configured rule")
	}
}

func TestCacheConfigOfProjectsEveryRebuildField(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache = config.CacheConfig{
		Enabled:                true,
		TTL:                    5 * time.Minute,
		MaxSize:                100,
		MaxBytes:               1 << 20,
		MaxTTL:                 time.Hour,
		SemanticEnabled:        true,
		SemanticThreshold:      0.9,
		SemanticEmbeddingModel: "text-embedding-3-small",
		SemanticTopK:           3,
		SemanticMaxEntries:     10,
	}
	got := cacheConfigOf(cfg, true)
	want := cache.Config{
		Enabled:                true,
		TTL:                    5 * time.Minute,
		MaxSize:                100,
		MaxBytes:               1 << 20,
		SemanticEnabled:        true,
		SemanticThreshold:      0.9,
		SemanticEmbeddingModel: "text-embedding-3-small",
		SemanticTopK:           3,
		SemanticMaxEntries:     10,
		Redis:                  true,
	}
	if got != want {
		t.Fatalf("cacheConfigOf = %+v, want %+v", got, want)
	}

	// max_ttl is enforced by the proxy handlers, so changing it alone must not
	// invalidate a warm cache.
	cfg.Cache.MaxTTL = 2 * time.Hour
	if !cache.ConfigEquals(got, cacheConfigOf(cfg, true)) {
		t.Fatal("changing cache.max_ttl alone triggered a cache rebuild")
	}
}

func TestBuildCacheForConfigHonorsEnabled(t *testing.T) {
	disabled := buildCacheForConfig(cache.Config{MaxSize: 10}, nil, proxy.NewRegistry(nil), discardLogger())
	if disabled != nil {
		t.Fatalf("disabled cache = %T, want nil", disabled)
	}
	enabled := buildCacheForConfig(cache.Config{Enabled: true, MaxSize: 10, MaxBytes: 4096}, nil, proxy.NewRegistry(nil), discardLogger())
	if enabled == nil {
		t.Fatal("enabled cache is nil")
	}
}

func TestDynamicCORSReload(t *testing.T) {
	cors := newDynamicCORS([]string{"https://a.example"})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := cors.Wrap(next)

	request := func(origin, method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/models", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if got := request("https://a.example", http.MethodGet).Header().Get("Access-Control-Allow-Origin"); got != "https://a.example" {
		t.Fatalf("initial allowed origin header = %q", got)
	}

	// Reload the policy: the previous origin must stop being allowed and the
	// new one must work without rebuilding the handler.
	cors.Set([]string{"https://b.example"})
	if got := request("https://a.example", http.MethodGet).Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("origin removed from policy still echoed: %q", got)
	}
	if got := request("https://b.example", http.MethodGet).Header().Get("Access-Control-Allow-Origin"); got != "https://b.example" {
		t.Fatalf("new origin not allowed: %q", got)
	}
	if got := request("https://a.example", http.MethodOptions).Code; got != http.StatusForbidden {
		t.Fatalf("preflight from disallowed origin = %d, want 403", got)
	}
	if got := request("https://b.example", http.MethodOptions).Code; got != http.StatusNoContent {
		t.Fatalf("preflight from allowed origin = %d, want 204", got)
	}

	// Disabling CORS (empty policy) must pass requests through untouched.
	cors.Set(nil)
	if got := request("https://b.example", http.MethodGet).Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("CORS disabled but header still set: %q", got)
	}
}

func TestTenantsOfDeduplicatesAndSkipsEmpty(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{Keys: []config.APIKeyConfig{
		{Key: "a", Tenant: "alpha"},
		{Key: "b", Tenant: "alpha"},
		{Key: "c", Tenant: "beta"},
		{Key: "d"},
	}}}
	got := tenantsOf(cfg)
	if len(got) != 2 {
		t.Fatalf("tenantsOf = %v, want 2 entries", got)
	}
	seen := map[string]bool{}
	for _, tenant := range got {
		if seen[tenant] {
			t.Fatalf("duplicate tenant %q in %v", tenant, got)
		}
		seen[tenant] = true
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Fatalf("tenantsOf = %v, want alpha and beta", got)
	}
}

// TestPricingCatalogPerMillionRuleNotScaledTwice guards the rule path: rates
// are already per million and must not be multiplied by the fallback unit.
func TestPricingCatalogPerMillionRuleNotScaledTwice(t *testing.T) {
	cfg := &config.Config{}
	cfg.RateLimit.Budget = &config.BudgetConfig{Pricing: config.PricingConfig{
		Catalog: []config.PricingRule{{Provider: "p", Model: "m", InputPerMillionTokensUSD: 3}},
	}}
	catalog, err := buildPricingCatalog(cfg)
	if err != nil {
		t.Fatalf("buildPricingCatalog: %v", err)
	}
	est, err := catalog.Chat("p", "m", 1_000_000, 0)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if est.USD != 3 {
		t.Fatalf("1M input tokens at 3 USD/M = %v, want 3", est.USD)
	}
}
