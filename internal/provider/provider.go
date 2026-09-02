package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/types"
)

// Re-export types from internal/types for backward compatibility.
// New code may import internal/types directly; existing code using
// provider.ChatRequest continues to work because these are type aliases.
type ChatMessage = types.ChatMessage
type ChatRequest = types.ChatRequest
type ChatResponse = types.ChatResponse
type Choice = types.Choice
type Usage = types.Usage
type StreamChunk = types.StreamChunk
type StreamChoice = types.StreamChoice
type Tool = types.Tool
type ToolFunc = types.ToolFunc
type ResponseFormat = types.ResponseFormat
type StreamOptions = types.StreamOptions
type Error = types.Error
type ErrorResponse = types.ErrorResponse
type EmbeddingRequest = types.EmbeddingRequest
type EmbeddingResponse = types.EmbeddingResponse
type EmbeddingData = types.EmbeddingData
type EmbeddingUsage = types.EmbeddingUsage

// Provider is the contract every LLM backend must implement.
type Provider interface {
	Name() string
	Send(ctx context.Context, req ChatRequest) (ChatResponse, error)
	SendStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, <-chan error)
	Models() []string
	HealthCheck(ctx context.Context) error
}

// Embedder is optionally implemented by providers that support embeddings.
type Embedder interface {
	Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
}

// ErrNoProvider is returned when no provider is configured for a model.
var ErrNoProvider = fmt.Errorf("no provider configured")

// ProviderError carries provider-specific error context, including whether
// the gateway should attempt a fallback provider.
type ProviderError struct {
	ProviderName string
	StatusCode   int
	Message      string
	Retryable    bool
	RetryAfter   time.Duration
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("[%s] %s (HTTP %d)", e.ProviderName, e.Message, e.StatusCode)
}

// IsRetryable reports whether err signals that another provider should be tried.
func IsRetryable(err error) bool {
	if pe, ok := err.(*ProviderError); ok {
		return pe.Retryable
	}
	return false
}

// IsNoProvider reports whether err indicates an unknown model.
func IsNoProvider(err error) bool {
	return errors.Is(err, ErrNoProvider)
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := time.ParseDuration(v + "s"); err == nil {
		return secs
	}
	var s int
	if _, err := fmt.Sscanf(v, "%d", &s); err == nil {
		return time.Duration(s) * time.Second
	}
	return 0
}
