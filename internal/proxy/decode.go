package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// decodeSingleJSON accepts exactly one JSON value and rejects trailing data.
// It deliberately does not use DisallowUnknownFields: the data plane must
// tolerate fields OpenAI adds over time instead of rejecting requests that a
// newer client sends. The control plane (config/auth) keeps its strict
// decoding elsewhere.
func decodeSingleJSON(body []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

// decodeChatRequest decodes a chat request without leaning on
// ChatMessage.UnmarshalJSON, which is strict by design (it is used on the
// control plane and for cache key canonicalization). The data plane mirrors the
// wire shape but ignores unknown message fields, so a new OpenAI message
// attribute does not turn a valid request into a 400.
func decodeChatRequest(body []byte) (provider.ChatRequest, error) {
	var wire chatRequestWire
	if err := decodeSingleJSON(body, &wire); err != nil {
		return provider.ChatRequest{}, err
	}
	return wire.toChatRequest()
}

// chatRequestWire mirrors provider.ChatRequest field-for-field except Messages,
// which is decoded through a tolerant per-message wire type.
type chatRequestWire struct {
	Model               string                   `json:"model"`
	Messages            []chatMessageWire        `json:"messages"`
	Temperature         *float64                 `json:"temperature,omitempty"`
	MaxTokens           *int                     `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                     `json:"max_completion_tokens,omitempty"`
	TopP                *float64                 `json:"top_p,omitempty"`
	TopK                *int                     `json:"top_k,omitempty"`
	PresencePenalty     *float64                 `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64                 `json:"frequency_penalty,omitempty"`
	Logprobs            *bool                    `json:"logprobs,omitempty"`
	TopLogprobs         *int                     `json:"top_logprobs,omitempty"`
	Seed                *int                     `json:"seed,omitempty"`
	Stream              bool                     `json:"stream,omitempty"`
	StreamOptions       *provider.StreamOptions  `json:"stream_options,omitempty"`
	Tools               []provider.Tool          `json:"tools,omitempty"`
	ToolChoice          any                      `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                    `json:"parallel_tool_calls,omitempty"`
	ResponseFormat      *provider.ResponseFormat `json:"response_format,omitempty"`
	Stop                any                      `json:"stop,omitempty"`
	N                   *int                     `json:"n,omitempty"`
	User                string                   `json:"user,omitempty"`
	ServiceTier         string                   `json:"service_tier,omitempty"`
	ReasoningEffort     string                   `json:"reasoning_effort,omitempty"`
	Modalities          []string                 `json:"modalities,omitempty"`
	Audio               *provider.AudioRequest   `json:"audio,omitempty"`
}

type chatMessageWire struct {
	Role         string                 `json:"role"`
	Content      json.RawMessage        `json:"content"`
	Name         string                 `json:"name"`
	ToolCallID   string                 `json:"tool_call_id"`
	ToolCalls    []provider.ToolCall    `json:"tool_calls"`
	FunctionCall *provider.FunctionCall `json:"function_call"`
	Audio        *provider.Audio        `json:"audio"`
	Refusal      string                 `json:"refusal"`
}

func (w chatRequestWire) toChatRequest() (provider.ChatRequest, error) {
	req := provider.ChatRequest{
		Model:               w.Model,
		Temperature:         w.Temperature,
		MaxTokens:           w.MaxTokens,
		MaxCompletionTokens: w.MaxCompletionTokens,
		TopP:                w.TopP,
		TopK:                w.TopK,
		PresencePenalty:     w.PresencePenalty,
		FrequencyPenalty:    w.FrequencyPenalty,
		Logprobs:            w.Logprobs,
		TopLogprobs:         w.TopLogprobs,
		Seed:                w.Seed,
		Stream:              w.Stream,
		StreamOptions:       w.StreamOptions,
		Tools:               w.Tools,
		ToolChoice:          w.ToolChoice,
		ParallelToolCalls:   w.ParallelToolCalls,
		ResponseFormat:      w.ResponseFormat,
		Stop:                w.Stop,
		N:                   w.N,
		User:                w.User,
		ServiceTier:         w.ServiceTier,
		ReasoningEffort:     w.ReasoningEffort,
		Modalities:          w.Modalities,
		Audio:               w.Audio,
	}
	if len(w.Messages) == 0 {
		return req, nil
	}
	req.Messages = make([]provider.ChatMessage, len(w.Messages))
	for i, message := range w.Messages {
		converted, err := message.toChatMessage()
		if err != nil {
			return provider.ChatRequest{}, err
		}
		req.Messages[i] = converted
	}
	return req, nil
}

func (w chatMessageWire) toChatMessage() (provider.ChatMessage, error) {
	message := provider.ChatMessage{
		Role:         w.Role,
		Name:         w.Name,
		ToolCallID:   w.ToolCallID,
		ToolCalls:    w.ToolCalls,
		FunctionCall: w.FunctionCall,
		Audio:        w.Audio,
		Refusal:      w.Refusal,
	}
	if len(w.Content) == 0 || bytes.Equal(bytes.TrimSpace(w.Content), []byte("null")) {
		return message, nil
	}
	if err := json.Unmarshal(w.Content, &message.Content); err == nil {
		return message, nil
	}
	if err := json.Unmarshal(w.Content, &message.ContentParts); err != nil {
		return provider.ChatMessage{}, fmt.Errorf("content must be a string or an array of content parts: %w", err)
	}
	return message, nil
}
