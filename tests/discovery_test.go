package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

func TestOpenAI_DiscoverModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" && r.URL.Path != "/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "gpt-4o", "object": "model"},
				{"id": "gpt-4o-mini", "object": "model"},
			},
		})
	}))
	defer srv.Close()

	// provider baseURL will be srv.URL + "/v1" – NewOpenAI appends "/chat/completions" but Discover uses "/models"
	p := provider.NewOpenAI("test-key", srv.URL+"/v1", 5*time.Second, nil)
	models, err := p.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("DiscoverModels failed: %v", err)
	}
	if len(models) != 2 || models[0] != "gpt-4o" {
		t.Errorf("got %v want [gpt-4o gpt-4o-mini]", models)
	}
	// test SetModels
	p.SetModels(models)
	if len(p.Models()) != 2 {
		t.Errorf("SetModels failed")
	}
}

func TestGemini_DiscoverModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "models/gemini-2.5-flash"},
				{"name": "models/gemini-2.5-pro"},
				{"name": "models/text-embedding-004"},
			},
		})
	}))
	defer srv.Close()

	p := provider.NewGemini("test-key", srv.URL, 5*time.Second, nil)
	models, err := p.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("DiscoverModels failed: %v", err)
	}
	if len(models) != 3 || models[0] != "gemini-2.5-flash" {
		t.Errorf("got %v want gemini models", models)
	}
	p.SetModels(models)
	if len(p.Models()) != 3 {
		t.Error("SetModels failed for gemini")
	}
}
