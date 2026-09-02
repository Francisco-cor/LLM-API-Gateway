package translate

import (
	"github.com/fcordero/llm-api-gateway/internal/types"
)

const AnthropicVersion = "2023-06-01"

const DefaultMaxTokens = 4096

// AnthropicRequest is the native Anthropic Messages API request body.
type AnthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []AnthropicMessage `json:"messages"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type AnthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AnthropicResponse is the native Anthropic Messages API response body.
type AnthropicResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// ToAnthropic converts the gateway's OpenAI-compatible request into the
// Anthropic Messages API format. System messages are extracted into the
// top-level "system" field since Anthropic does not accept a "system" role
// inside the messages array.
func ToAnthropic(req types.ChatRequest) AnthropicRequest {
	maxTokens := DefaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	native := AnthropicRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
	}
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			native.System = msg.Content
			continue
		}
		native.Messages = append(native.Messages, AnthropicMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}
	return native
}

// FromAnthropic converts an Anthropic Messages API response back into the
// gateway's OpenAI-compatible format.
func FromAnthropic(resp AnthropicResponse) types.ChatResponse {
	text := ""
	if len(resp.Content) > 0 {
		text = resp.Content[0].Text
	}
	finishReason := "stop"
	if resp.StopReason == "max_tokens" {
		finishReason = "length"
	}
	return types.ChatResponse{
		ID:     resp.ID,
		Object: "chat.completion",
		Model:  resp.Model,
		Choices: []types.Choice{{
			Index:        0,
			Message:      types.ChatMessage{Role: "assistant", Content: text},
			FinishReason: finishReason,
		}},
		Usage: types.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}
