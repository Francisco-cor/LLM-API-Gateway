package tests

import (
	"context"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// mockProvider is a test double implementing provider.Provider.
type mockProvider struct {
	name      string
	models    []string
	resp      provider.ChatResponse
	err       error
	callCount int
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Models() []string { return m.models }

func (m *mockProvider) Send(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	m.callCount++
	if m.err != nil {
		return provider.ChatResponse{}, m.err
	}
	return m.resp, nil
}

func (m *mockProvider) HealthCheck(_ context.Context) error { return nil }
