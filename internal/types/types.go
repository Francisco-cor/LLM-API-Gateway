package types

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ChatMessage is the OpenAI-compatible message format used across the gateway.
type ChatMessage struct {
	Role         string        `json:"role"`
	Content      string        `json:"content,omitempty"`
	ContentParts []ContentPart `json:"-"`
	Name         string        `json:"name,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
	FunctionCall *FunctionCall `json:"function_call,omitempty"`
	Audio        *Audio        `json:"audio,omitempty"`
	Refusal      string        `json:"refusal,omitempty"`
}

type ContentPart struct {
	Type       string      `json:"type"`
	Text       string      `json:"text,omitempty"`
	ImageURL   *ImageURL   `json:"image_url,omitempty"`
	InputAudio *InputAudio `json:"input_audio,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type InputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Audio struct {
	ID         string `json:"id,omitempty"`
	Data       string `json:"data,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
}

// Text returns the textual portion used by providers that do not support
// multimodal content. The original ContentParts remain available for OpenAI
// passthrough and response serialization.
func (m ChatMessage) Text() string {
	if m.Content != "" {
		return m.Content
	}
	var parts []string
	for _, part := range m.ContentParts {
		if part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "")
}

func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	type wire struct {
		Role         string          `json:"role"`
		Content      json.RawMessage `json:"content"`
		Name         string          `json:"name"`
		ToolCallID   string          `json:"tool_call_id"`
		ToolCalls    []ToolCall      `json:"tool_calls"`
		FunctionCall *FunctionCall   `json:"function_call"`
		Audio        *Audio          `json:"audio"`
		Refusal      string          `json:"refusal"`
	}
	var value wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*m = ChatMessage{Role: value.Role, Name: value.Name, ToolCallID: value.ToolCallID, ToolCalls: value.ToolCalls, FunctionCall: value.FunctionCall, Audio: value.Audio, Refusal: value.Refusal}
	if len(value.Content) == 0 || bytes.Equal(bytes.TrimSpace(value.Content), []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(value.Content, &m.Content); err == nil {
		return nil
	}
	if err := json.Unmarshal(value.Content, &m.ContentParts); err != nil {
		return fmt.Errorf("content must be a string or an array of content parts: %w", err)
	}
	return nil
}

func (m ChatMessage) MarshalJSON() ([]byte, error) {
	content := any(m.Content)
	if m.ContentParts != nil {
		content = m.ContentParts
	}
	return json.Marshal(struct {
		Role         string        `json:"role"`
		Content      any           `json:"content,omitempty"`
		Name         string        `json:"name,omitempty"`
		ToolCallID   string        `json:"tool_call_id,omitempty"`
		ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
		FunctionCall *FunctionCall `json:"function_call,omitempty"`
		Audio        *Audio        `json:"audio,omitempty"`
		Refusal      string        `json:"refusal,omitempty"`
	}{m.Role, content, m.Name, m.ToolCallID, m.ToolCalls, m.FunctionCall, m.Audio, m.Refusal})
}

// ChatRequest is the normalized request format accepted by the gateway and
// translated to each provider's native format.
type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []ChatMessage   `json:"messages"`
	Temperature         *float64        `json:"temperature,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	Logprobs            *bool           `json:"logprobs,omitempty"`
	TopLogprobs         *int            `json:"top_logprobs,omitempty"`
	Seed                *int            `json:"seed,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat      *ResponseFormat `json:"response_format,omitempty"`
	Stop                any             `json:"stop,omitempty"`
	N                   *int            `json:"n,omitempty"`
	User                string          `json:"user,omitempty"`
	ServiceTier         string          `json:"service_tier,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	Modalities          []string        `json:"modalities,omitempty"`
	Audio               *AudioRequest   `json:"audio,omitempty"`
}

type AudioRequest struct {
	Voice  string `json:"voice"`
	Format string `json:"format"`
}

func (r ChatRequest) EffectiveMaxTokens() *int {
	if r.MaxCompletionTokens != nil {
		return r.MaxCompletionTokens
	}
	return r.MaxTokens
}

// Tool is OpenAI-compatible tool definition (passthrough).
type Tool struct {
	Type     string   `json:"type"`
	Function ToolFunc `json:"function"`
}

type ToolFunc struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// ResponseFormat controls output formatting.
type ResponseFormat struct {
	Type string `json:"type"` // "text" | "json_object" | "json_schema"
}

// StreamOptions mirrors OpenAI stream_options.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatResponse is the normalized OpenAI-compatible response returned to clients.
type ChatResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             Usage    `json:"usage"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
	ServiceTier       string   `json:"service_tier,omitempty"`
}

// Choice represents a single completion choice.
type Choice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
	Logprobs     any         `json:"logprobs,omitempty"`
}

// Usage holds token consumption for a request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk is an OpenAI-compatible SSE chunk.
type StreamChunk struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	Choices           []StreamChoice `json:"choices"`
	Usage             *Usage         `json:"usage,omitempty"`
	SystemFingerprint string         `json:"system_fingerprint,omitempty"`
}

type StreamChoice struct {
	Index        int         `json:"index"`
	Delta        ChatMessage `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
	Logprobs     any         `json:"logprobs,omitempty"`
}

// EmbeddingRequest is OpenAI-compatible embeddings request.
type EmbeddingRequest struct {
	Model          string `json:"model"`
	Input          any    `json:"input"` // string or []string
	EncodingFormat string `json:"encoding_format,omitempty"`
	Dimensions     *int   `json:"dimensions,omitempty"`
	User           string `json:"user,omitempty"`
}

// EmbeddingResponse is OpenAI-compatible embeddings response.
type EmbeddingResponse struct {
	Object string          `json:"object"`
	Data   []EmbeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  EmbeddingUsage  `json:"usage"`
}

type EmbeddingData struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
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
