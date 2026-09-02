package tests

import (
	"context"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// mockProvider is a test double implementing provider.Provider.
type mockProvider struct {
	name         string
	models       []string
	resp         provider.ChatResponse
	err          error
	callCount    int
	embedResp    provider.EmbeddingResponse
	embedErr     error
	embedCount   int
	discoverResp []string
	discoverErr  error
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Models() []string { return m.models }

func (m *mockProvider) SetModels(models []string) { m.models = models }

func (m *mockProvider) Send(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	m.callCount++
	if m.err != nil {
		return provider.ChatResponse{}, m.err
	}
	return m.resp, nil
}

func (m *mockProvider) SendStream(_ context.Context, _ provider.ChatRequest) (<-chan provider.StreamChunk, <-chan error) {
	ch := make(chan provider.StreamChunk)
	errCh := make(chan error)
	close(ch)
	close(errCh)
	return ch, errCh
}

func (m *mockProvider) HealthCheck(_ context.Context) error { return nil }

func (m *mockProvider) Embed(_ context.Context, _ provider.EmbeddingRequest) (provider.EmbeddingResponse, error) {
	m.embedCount++
	if m.embedErr != nil {
		return provider.EmbeddingResponse{}, m.embedErr
	}
	return m.embedResp, nil
}

func (m *mockProvider) DiscoverModels(_ context.Context) ([]string, error) {
	if m.discoverErr != nil {
		return nil, m.discoverErr
	}
	return m.discoverResp, nil
}
