package translate

import (
	"encoding/json"

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
	Tools       []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice  any                `json:"tool_choice,omitempty"`
}

type AnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type AnthropicContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
}

type AnthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

// AnthropicResponse is the native Anthropic Messages API response body.
type AnthropicResponse struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	Content    []AnthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
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
	if max := req.EffectiveMaxTokens(); max != nil {
		maxTokens = *max
	}
	native := AnthropicRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		Tools:       toAnthropicTools(req.Tools),
		ToolChoice:  toAnthropicToolChoice(req.ToolChoice),
	}
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			native.System = msg.Text()
			continue
		}
		if msg.Role == "tool" {
			native.Messages = append(native.Messages, AnthropicMessage{
				Role: "user",
				Content: []AnthropicContentBlock{{
					Type:      "tool_result",
					ToolUseID: msg.ToolCallID,
					Content:   msg.Text(),
				}},
			})
			continue
		}
		if len(msg.ToolCalls) > 0 {
			blocks := make([]AnthropicContentBlock, 0, len(msg.ToolCalls)+1)
			if text := msg.Text(); text != "" {
				blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: text})
			}
			for _, call := range msg.ToolCalls {
				blocks = append(blocks, AnthropicContentBlock{
					Type:  "tool_use",
					ID:    call.ID,
					Name:  call.Function.Name,
					Input: decodeToolArguments(call.Function.Arguments),
				})
			}
			native.Messages = append(native.Messages, AnthropicMessage{Role: msg.Role, Content: blocks})
			continue
		}
		native.Messages = append(native.Messages, AnthropicMessage{Role: msg.Role, Content: msg.Text()})
	}
	return native
}

func toAnthropicTools(tools []types.Tool) []AnthropicTool {
	if len(tools) == 0 {
		return nil
	}
	native := make([]AnthropicTool, 0, len(tools))
	for _, tool := range tools {
		native = append(native, AnthropicTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: tool.Function.Parameters,
		})
	}
	return native
}

func toAnthropicToolChoice(choice any) any {
	switch value := choice.(type) {
	case string:
		switch value {
		case "required":
			return map[string]string{"type": "any"}
		case "auto", "none":
			return map[string]string{"type": value}
		}
	case map[string]any:
		if function, ok := value["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok {
				return map[string]string{"type": "tool", "name": name}
			}
		}
	}
	return choice
}

func decodeToolArguments(arguments string) any {
	if arguments == "" {
		return map[string]any{}
	}
	var decoded any
	if json.Unmarshal([]byte(arguments), &decoded) == nil {
		return decoded
	}
	return arguments
}

// FromAnthropic converts an Anthropic Messages API response back into the
// gateway's OpenAI-compatible format.
func FromAnthropic(resp AnthropicResponse) types.ChatResponse {
	text := ""
	var toolCalls []types.ToolCall
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			text += block.Text
		case "tool_use":
			arguments := "{}"
			if block.Input != nil {
				if data, err := json.Marshal(block.Input); err == nil {
					arguments = string(data)
				}
			}
			toolCalls = append(toolCalls, types.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: types.ToolCallFunction{
					Name:      block.Name,
					Arguments: arguments,
				},
			})
		}
	}
	finishReason := "stop"
	if resp.StopReason == "max_tokens" {
		finishReason = "length"
	} else if resp.StopReason == "tool_use" {
		finishReason = "tool_calls"
	}
	return types.ChatResponse{
		ID:     resp.ID,
		Object: "chat.completion",
		Model:  resp.Model,
		Choices: []types.Choice{{
			Index:        0,
			Message:      types.ChatMessage{Role: "assistant", Content: text, ToolCalls: toolCalls},
			FinishReason: finishReason,
		}},
		Usage: types.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}
