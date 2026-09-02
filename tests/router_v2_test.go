package tests

import (
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
)

func TestRouter_WeightedDistribution(t *testing.T) {
	// Two providers claiming same model with explicit weighted config 90/10
	openai := &mockProvider{name: "openai", models: []string{"gpt-4o"}}
	anthropic := &mockProvider{name: "anthropic", models: []string{"claude-sonnet-4-6"}}

	weighted := map[string][]proxy.WeightedConfig{
		"gpt-4o": {
			{Provider: "openai", Weight: 90},
			{Provider: "anthropic", Weight: 10},
		},
	}
	registry := proxy.NewRegistryWithWeighted([]provider.Provider{openai, anthropic}, nil, weighted)

	counts := map[string]int{"openai": 0, "anthropic": 0}
	const N = 1000
	for i := 0; i < N; i++ {
		p, err := registry.Resolve("gpt-4o")
		if err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
		counts[p.Name()]++
	}
	// 90/10 tolerance +-5% => openai 850-950, anthropic 50-150
	if counts["openai"] < 850 || counts["openai"] > 950 {
		t.Errorf("weighted distribution openai got %d/1000, want 900 +-50", counts["openai"])
	}
	if counts["anthropic"] < 50 || counts["anthropic"] > 150 {
		t.Errorf("weighted distribution anthropic got %d/1000, want 100 +-50", counts["anthropic"])
	}
	t.Logf("weighted 90/10 distribution: openai=%d anthropic=%d", counts["openai"], counts["anthropic"])
}

func TestRouter_WildcardMatching(t *testing.T) {
	openai := &mockProvider{name: "openai", models: []string{"gpt-4*", "gpt-3.5-turbo"}}
	anthropic := &mockProvider{name: "anthropic", models: []string{"claude-*"}}
	gemini := &mockProvider{name: "gemini", models: []string{"gemini-2.5-pro", "gemini-*"}}
	registry := proxy.NewRegistry([]provider.Provider{openai, anthropic, gemini})

	cases := []struct {
		model    string
		wantProv string
		wantErr  bool
	}{
		{model: "gpt-4o", wantProv: "openai"},
		{model: "gpt-4o-mini", wantProv: "openai"},
		{model: "gpt-3.5-turbo", wantProv: "openai"},
		{model: "claude-sonnet-4-6", wantProv: "anthropic"},
		{model: "claude-haiku-4-5-20251001", wantProv: "anthropic"},
		{model: "gemini-2.5-pro", wantProv: "gemini"}, // exact should still match gemini (first exact? but in this registry gemini has exact "gemini-2.5-pro" as first pattern? It is exact but treated as exact if no wildcard? Actually gemini-2.5-pro has no wildcard, so it's exact. It will be in byModel exact map, should resolve.)
		{model: "gemini-2.5-flash", wantProv: "gemini"}, // wildcard gemini-*
		{model: "gemini-unknown", wantProv: "gemini"},
		{model: "llama-3-70b", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			p, err := registry.Resolve(tc.model)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.model)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.model, err)
			}
			if p.Name() != tc.wantProv {
				t.Errorf("model %q got provider %q want %q", tc.model, p.Name(), tc.wantProv)
			}
		})
	}
}

func TestRouter_WildcardWithWeighted(t *testing.T) {
	// provider models use wildcard, weighted config uses exact model name
	openai := &mockProvider{name: "openai", models: []string{"gpt-4*"}}
	anthropic := &mockProvider{name: "anthropic", models: []string{"gpt-4*"}}
	weighted := map[string][]proxy.WeightedConfig{
		"gpt-4o": {{Provider: "openai", Weight: 80}, {Provider: "anthropic", Weight: 20}},
	}
	registry := proxy.NewRegistryWithWeighted([]provider.Provider{openai, anthropic}, nil, weighted)
	// gpt-4o should use weighted even though providers are wildcard
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		p, err := registry.Resolve("gpt-4o")
		if err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
		counts[p.Name()]++
	}
	if counts["openai"] == 0 || counts["anthropic"] == 0 {
		t.Errorf("expected both providers to be hit with weighted wildcard, got %v", counts)
	}
}

func TestRouter_RegexPattern(t *testing.T) {
	openai := &mockProvider{name: "openai", models: []string{"gpt-4.*", "gpt-3.*"}} // treated as regex
	registry := proxy.NewRegistry([]provider.Provider{openai})
	p, err := registry.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("regex not matched: %v", err)
	}
	if p.Name() != "openai" {
		t.Errorf("got %q want openai", p.Name())
	}
	// non-matching
	if _, err := registry.Resolve("claude-sonnet"); err == nil {
		t.Error("expected error for non-matching regex")
	}
}
