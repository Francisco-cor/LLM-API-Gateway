package budget

import (
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/pricing"
)

func TestManagerPricingAndHotReload(t *testing.T) {
	catalog, err := pricing.NewCatalog([]pricing.Rule{{
		Provider:                  "openai",
		Model:                     "gpt-4o",
		InputPerMillionTokensUSD:  2,
		OutputPerMillionTokensUSD: 4,
	}}, 0.01, "USD", "v1", pricing.UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewConfigured(false, 100, 10, 0.01, nil, catalog)
	if reservation, err := mgr.Reserve("tenant-a", 1, 1); err != nil || reservation != nil {
		t.Fatalf("disabled manager reserve = %v, %v", reservation, err)
	}

	mgr.UpdateConfig(true, 100, 10, 0.01, catalog)
	estimate, err := mgr.CostForChat("openai", "gpt-4o", 1_000_000, 1_000_000)
	if err != nil || estimate.USD != 6 || !estimate.Known || estimate.Version != "v1" {
		t.Fatalf("chat estimate = %+v, err=%v", estimate, err)
	}
	reservation, err := mgr.Reserve("tenant-a", 100, 6)
	if err != nil || reservation == nil {
		t.Fatalf("reserve after enable = %v, %v", reservation, err)
	}
	reservation.Cancel()
}
