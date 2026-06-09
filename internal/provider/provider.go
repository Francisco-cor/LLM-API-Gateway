package provider

import (
	"context"
	"fmt"
)

// Provider is the contract every LLM backend must implement.
type Provider interface {
	Name() string
	Send(ctx context.Context, req ChatRequest) (ChatResponse, error)
	Models() []string
	HealthCheck(ctx context.Context) error
}

// ChatMessage is the OpenAI-compatible message format used across the gateway.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the normalized request format accepted by the gateway and
// translated to each provider's native format.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

// ChatResponse is the normalized OpenAI-compatible response returned to clients.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice represents a single completion choice.
type Choice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// Usage holds token consumption for a request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Error is the OpenAI-compatible error body.
type Error struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// ErrorResponse wraps Error for JSON serialization.
type ErrorResponse struct {
	Error Error `json:"error"`
}

// ProviderError carries provider-specific error context, including whether
// the gateway should attempt a fallback provider.
type ProviderError struct {
	ProviderName string
	StatusCode   int
	Message      string
	Retryable    bool
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
