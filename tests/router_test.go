package tests

import (
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
)

func TestRegistry_Resolve(t *testing.T) {
	registry := proxy.NewRegistry([]provider.Provider{
		&mockProvider{name: "openai", models: []string{"gpt-4o", "gpt-4o-mini"}},
		&mockProvider{name: "anthropic", models: []string{"claude-sonnet-4-6", "claude-haiku-4-5-20251001"}},
		&mockProvider{name: "gemini", models: []string{"gemini-2.5-flash", "gemini-2.5-pro"}},
	})

	cases := []struct {
		name      string
		model     string
		wantName  string
		wantError bool
	}{
		{name: "gpt-4o routes to openai", model: "gpt-4o", wantName: "openai"},
		{name: "gpt-4o-mini routes to openai", model: "gpt-4o-mini", wantName: "openai"},
		{name: "claude-sonnet-4-6 routes to anthropic", model: "claude-sonnet-4-6", wantName: "anthropic"},
		{name: "claude-haiku-4-5-20251001 routes to anthropic", model: "claude-haiku-4-5-20251001", wantName: "anthropic"},
		{name: "gemini-2.5-flash routes to gemini", model: "gemini-2.5-flash", wantName: "gemini"},
		{name: "unknown model returns error", model: "llama-3-70b", wantError: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := registry.Resolve(tc.model)
			if tc.wantError {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Name() != tc.wantName {
				t.Errorf("got provider %q, want %q", p.Name(), tc.wantName)
			}
		})
	}
}

func TestRegistry_Get(t *testing.T) {
	registry := proxy.NewRegistry([]provider.Provider{
		&mockProvider{name: "openai", models: []string{"gpt-4o"}},
	})

	if _, ok := registry.Get("openai"); !ok {
		t.Error("expected openai to be registered")
	}
	if _, ok := registry.Get("anthropic"); ok {
		t.Error("did not expect anthropic to be registered")
	}
}
