package tests

import (
	"context"
	"sync/atomic"
	"time"

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

	// sendDelay makes Send slow so concurrent-call coalescing can be observed.
	sendDelay time.Duration

	// streamCh/streamErr, when set, replace the default terminal stream.
	streamCh  chan provider.StreamChunk
	streamErr chan error

	healthErr   error
	healthDelay time.Duration
	healthCalls atomic.Int32
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Models() []string { return m.models }

func (m *mockProvider) SetModels(models []string) { m.models = models }

func (m *mockProvider) Send(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	m.callCount++
	if m.sendDelay > 0 {
		time.Sleep(m.sendDelay)
	}
	if m.err != nil {
		return provider.ChatResponse{}, m.err
	}
	return m.resp, nil
}

func (m *mockProvider) SendStream(_ context.Context, _ provider.ChatRequest) (<-chan provider.StreamChunk, <-chan error) {
	if m.streamCh != nil || m.streamErr != nil {
		ch := m.streamCh
		if ch == nil {
			ch = make(chan provider.StreamChunk)
			close(ch)
		}
		errCh := m.streamErr
		if errCh == nil {
			errCh = make(chan error)
			close(errCh)
		}
		return ch, errCh
	}
	ch := make(chan provider.StreamChunk, 1)
	finish := "stop"
	ch <- provider.StreamChunk{
		ID:      "chatcmpl-stream",
		Object:  "chat.completion.chunk",
		Model:   m.firstModel(),
		Choices: []provider.StreamChoice{{Index: 0, Delta: provider.ChatMessage{Role: "assistant"}, FinishReason: &finish}},
		Usage:   &provider.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}
	close(ch)
	errCh := make(chan error)
	close(errCh)
	return ch, errCh
}

func (m *mockProvider) firstModel() string {
	if len(m.models) > 0 {
		return m.models[0]
	}
	return ""
}

func (m *mockProvider) HealthCheck(ctx context.Context) error {
	m.healthCalls.Add(1)
	if m.healthDelay > 0 {
		select {
		case <-time.After(m.healthDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.healthErr
}

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

// countingProvider is a concurrency-safe provider double. Hedge tests assert
// call counts from the test goroutine while a losing branch may still be
// unwinding, so the counter must be atomic.
type countingProvider struct {
	name   string
	models []string
	delay  time.Duration
	err    error
	resp   provider.ChatResponse
	calls  atomic.Int32
}

func (p *countingProvider) Name() string                      { return p.name }
func (p *countingProvider) Models() []string                  { return p.models }
func (p *countingProvider) SetModels(models []string)         { p.models = models }
func (p *countingProvider) HealthCheck(context.Context) error { return nil }

func (p *countingProvider) Send(ctx context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	p.calls.Add(1)
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return provider.ChatResponse{}, ctx.Err()
		}
	}
	if p.err != nil {
		return provider.ChatResponse{}, p.err
	}
	return p.resp, nil
}

func (p *countingProvider) SendStream(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, <-chan error) {
	ch := make(chan provider.StreamChunk)
	errCh := make(chan error)
	close(ch)
	close(errCh)
	return ch, errCh
}
