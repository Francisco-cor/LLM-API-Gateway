package pricing

import (
	"errors"
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

func TestCatalogEstimateUsesHighestMatchingProviderRate(t *testing.T) {
	catalog, err := NewCatalog([]Rule{
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
	if got.USD != 7 {
		t.Fatalf("estimate USD = %v, want 7", got.USD)
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
