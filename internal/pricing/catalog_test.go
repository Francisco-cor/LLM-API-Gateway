package pricing

import (
	"errors"
	"math"
	"testing"
)

func TestCatalogPrefersExactModel(t *testing.T) {
	catalog, err := NewCatalog([]Rule{
		{Provider: "openai", Model: "gpt-*", InputPerMillionTokensUSD: 1, OutputPerMillionTokensUSD: 2},
		{Provider: "openai", Model: "gpt-4o", InputPerMillionTokensUSD: 5, OutputPerMillionTokensUSD: 6},
	}, 0.01, "USD", "2026-09", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := catalog.Chat("openai", "gpt-4o", 1_000_000, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if got.USD != 11 || !got.Known || got.Version != "2026-09" {
		t.Fatalf("unexpected exact estimate: %+v", got)
	}
}

// TestCatalogPerTokenFallback pins the unit conversion that cmd/gateway needs:
// cost_per_token_usd is USD per token, not USD per million tokens.
func TestCatalogPerTokenFallback(t *testing.T) {
	catalog, err := NewCatalogWithFallbackPerToken(nil, 0.00001, "USD", "", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.FallbackPerMillionTokensUSD(); got != 10 {
		t.Fatalf("fallback per million = %v, want 10", got)
	}
	if got := catalog.FallbackPerTokenUSD(); math.Abs(got-0.00001) > 1e-15 {
		t.Fatalf("fallback per token = %v, want 0.00001", got)
	}
	estimate, err := catalog.Chat("openai", "unknown-model", 1_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.USD != 10 || estimate.Known {
		t.Fatalf("1M fallback tokens = %+v, want 10 USD unknown", estimate)
	}

	perMillion, err := NewCatalog(nil, 10, "USD", "", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	estimate, err = perMillion.Chat("openai", "unknown-model", 1_000_000, 0)
	if err != nil || estimate.USD != 10 {
		t.Fatalf("per-million fallback = %+v, %v; want 10 USD", estimate, err)
	}
	if _, err := NewCatalogWithFallbackPerToken(nil, -1, "USD", "", UnknownUseFallback); err == nil {
		t.Fatal("negative per-token fallback must be rejected")
	}
}

func TestCatalogSpecificityBeatsHigherRate(t *testing.T) {
	catalog, err := NewCatalog([]Rule{
		// Deliberately inverted prices: the wildcard is far more expensive than
		// the more specific family rule.
		{Provider: "openai", Model: "gpt-*", InputPerMillionTokensUSD: 100, OutputPerMillionTokensUSD: 100},
		{Provider: "openai", Model: "gpt-4o-*", InputPerMillionTokensUSD: 1, OutputPerMillionTokensUSD: 1},
	}, 0, "USD", "", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	estimate, err := catalog.Chat("openai", "gpt-4o-mini", 1_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.USD != 1 {
		t.Fatalf("specific rule estimate = %v USD, want 1 (not the expensive wildcard)", estimate.USD)
	}

	// Same rank (both globs): more literal characters win.
	catalog, err = NewCatalog([]Rule{
		{Provider: "anthropic", Model: "claude-*", InputPerMillionTokensUSD: 9},
		{Provider: "anthropic", Model: "claude-sonnet-*", InputPerMillionTokensUSD: 3},
	}, 0, "USD", "", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	estimate, err = catalog.Chat("anthropic", "claude-sonnet-4-6", 1_000_000, 0)
	if err != nil || estimate.USD != 3 {
		t.Fatalf("sonnet estimate = %+v, %v; want 3 USD", estimate, err)
	}
}

func TestCatalogEstimateChatPrefersSpecificityThenHighestRate(t *testing.T) {
	catalog, err := NewCatalog([]Rule{
		{Provider: "openai", Model: "model-*", InputPerMillionTokensUSD: 100, OutputPerMillionTokensUSD: 100},
		{Provider: "openai", Model: "model-x", InputPerMillionTokensUSD: 1, OutputPerMillionTokensUSD: 1},
		{Provider: "anthropic", Model: "model-x", InputPerMillionTokensUSD: 3, OutputPerMillionTokensUSD: 4},
	}, 0.01, "USD", "", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := catalog.EstimateChat("model-x", 1_000_000, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	// The exact pattern beats the expensive wildcard; between the two exact
	// rules the higher rate wins so a fallback provider cannot under-reserve.
	if got.USD != 7 {
		t.Fatalf("estimate USD = %v, want 7", got.USD)
	}
}

func TestCatalogGlobsCrossSlashes(t *testing.T) {
	rules := []Rule{
		{Provider: "meta", Model: "meta-llama/*", InputPerMillionTokensUSD: 2},
		{Provider: "google", Model: "gemini-?-flash", InputPerMillionTokensUSD: 1},
		{Provider: "openai", Model: "gpt-[45]o", InputPerMillionTokensUSD: 3},
	}
	catalog, err := NewCatalog(rules, 0, "USD", "", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		provider string
		model    string
		want     float64
		known    bool
	}{
		{"slash namespace", "meta", "meta-llama/Llama-3-70B", 2, true},
		{"question mark", "google", "gemini-2-flash", 1, true},
		{"class", "openai", "gpt-4o", 3, true},
		{"class miss", "openai", "gpt-6o", 0, false},
		{"slash is literal for other providers", "google", "meta-llama/other", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := catalog.Chat(tt.provider, tt.model, 1_000_000, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.USD != tt.want || got.Known != tt.known {
				t.Fatalf("Chat(%q) = %+v, want %v known=%v", tt.model, got, tt.want, tt.known)
			}
		})
	}
	if _, err := NewCatalog([]Rule{{Provider: "x", Model: "broken[", InputPerMillionTokensUSD: 1}}, 0, "USD", "", UnknownUseFallback); err == nil {
		t.Fatal("invalid glob must be rejected at construction")
	}
}

func TestCatalogCacheRates(t *testing.T) {
	catalog, err := NewCatalog([]Rule{{
		Provider:                      "openai",
		Model:                         "gpt-4o",
		InputPerMillionTokensUSD:      10,
		OutputPerMillionTokensUSD:     20,
		CacheReadPerMillionTokensUSD:  1,
		CacheWritePerMillionTokensUSD: 5,
	}}, 1, "USD", "", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	// 500k uncached * 10 + 400k read * 1 + 100k write * 5 + 1M output * 20
	// = 5 + 0.4 + 0.5 + 20 = 25.9 USD.
	got, err := catalog.ChatWithCacheTokens("openai", "gpt-4o", 1_000_000, 1_000_000, 400_000, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.USD-25.9) > 1e-9 || !got.Known {
		t.Fatalf("cached estimate = %+v, want 25.9 USD", got)
	}

	// Without cache rates every prompt token uses the regular input rate.
	plain, err := NewCatalog([]Rule{{
		Provider:                  "openai",
		Model:                     "gpt-4o",
		InputPerMillionTokensUSD:  10,
		OutputPerMillionTokensUSD: 20,
	}}, 1, "USD", "", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err = plain.ChatWithCacheTokens("openai", "gpt-4o", 1_000_000, 1_000_000, 400_000, 100_000)
	if err != nil || math.Abs(got.USD-30) > 1e-9 {
		t.Fatalf("plain cached estimate = %+v, %v; want 30 USD", got, err)
	}

	// Malformed usage (cache tokens exceed prompt) is clamped, never negative.
	got, err = catalog.ChatWithCacheTokens("openai", "gpt-4o", 100, 0, 1_000, 1_000)
	if err != nil || got.USD != 0.0001 {
		t.Fatalf("clamped estimate = %+v, %v; want 0.0001 USD", got, err)
	}
	got, err = catalog.ChatWithCacheTokens("openai", "gpt-4o", 0, 0, -5, -5)
	if err != nil || got.USD != 0 {
		t.Fatalf("negative estimate = %+v, %v; want 0", got, err)
	}
	got, err = plain.ChatWithCacheTokens("other", "unknown", -100, -10, -5, -5)
	if err != nil || got.USD != 0 || got.Known {
		t.Fatalf("unknown negative estimate = %+v, %v; want 0 unknown", got, err)
	}

	// Chat keeps its historical contract: cache tokens are billed as input.
	got, err = catalog.Chat("openai", "gpt-4o", 1_000_000, 1_000_000)
	if err != nil || got.USD != 30 {
		t.Fatalf("Chat = %+v, %v; want 30 USD", got, err)
	}
}

func TestCatalogEstimateChatWithCacheTokens(t *testing.T) {
	catalog, err := NewCatalog([]Rule{
		{Provider: "cheap", Model: "shared-model", InputPerMillionTokensUSD: 1, OutputPerMillionTokensUSD: 1, CacheReadPerMillionTokensUSD: 0.1},
		{Provider: "pricey", Model: "shared-model", InputPerMillionTokensUSD: 5, OutputPerMillionTokensUSD: 5, CacheReadPerMillionTokensUSD: 0.5},
	}, 0, "USD", "", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := catalog.EstimateChatWithCacheTokens("shared-model", 1_000_000, 1_000_000, 1_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.USD-5.5) > 1e-9 {
		t.Fatalf("conservative cached estimate = %v, want 5.5", got.USD)
	}
}

func TestCatalogRejectsNegativeCacheRate(t *testing.T) {
	_, err := NewCatalog([]Rule{{
		Provider:                     "openai",
		Model:                        "gpt-4o",
		InputPerMillionTokensUSD:     1,
		CacheReadPerMillionTokensUSD: -1,
	}}, 0, "USD", "", UnknownUseFallback)
	if err == nil {
		t.Fatal("negative cache rate must be rejected")
	}
}

func TestCatalogUnknownPolicy(t *testing.T) {
	fallback, err := NewCatalog(nil, 0.25, "USD", "", UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fallback.Embedding("openai", "unknown", 1_000_000)
	if err != nil || got.USD != 0.25 || got.Known {
		t.Fatalf("unexpected fallback estimate: %+v, %v", got, err)
	}

	reject, err := NewCatalog(nil, 0.25, "USD", "", UnknownReject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reject.Embedding("openai", "unknown", 1); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("error = %v, want ErrUnknownPrice", err)
	}
}
