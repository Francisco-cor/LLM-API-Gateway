package translate

import (
	"encoding/json"
	"strings"

	"github.com/fcordero/llm-api-gateway/internal/types"
)

const AnthropicVersion = "2023-06-01"

const DefaultMaxTokens = 4096

// AnthropicRequest is the native Anthropic Messages API request body.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
	Thinking      *AnthropicThinking `json:"thinking,omitempty"`
}

type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type AnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type AnthropicContentBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	Source    *AnthropicImageSource `json:"source,omitempty"`
	ID        string                `json:"id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Input     any                   `json:"input,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   any                   `json:"content,omitempty"`
}

type AnthropicImageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
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
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// ToAnthropic converts the gateway request, discarding translation errors. It
// exists for backward compatibility; providers use ToAnthropicStrict.
func ToAnthropic(req types.ChatRequest) AnthropicRequest {
	native, _ := ToAnthropicStrict(req)
	return native
}

// ToAnthropicStrict converts the gateway's OpenAI-compatible request into the
// Anthropic Messages API format. Every system/developer message is folded into
// the top-level "system" field because Anthropic accepts a single system
// prompt outside the messages array.
func ToAnthropicStrict(req types.ChatRequest) (AnthropicRequest, error) {
	maxTokens := DefaultMaxTokens
	if max := req.EffectiveMaxTokens(); max != nil {
		maxTokens = *max
	}
	native := AnthropicRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
	}

	temperature := req.Temperature
	if temperature != nil {
		clamped := clamp(*temperature, 0, 1)
		temperature = &clamped
	}
	native.Temperature = temperature
	if req.TopP != nil {
		topP := clamp(*req.TopP, 0, 1)
		native.TopP = &topP
	}
	if req.TopK != nil {
		if *req.TopK <= 0 {
			return native, invalidRequest("top_k must be positive")
		}
		native.TopK = req.TopK
	}

	stops, err := parseStopSequences(req.Stop)
	if err != nil {
		return native, err
	}
	native.StopSequences = stops

	if req.N != nil && *req.N > 1 {
		return native, unsupportedParameter("n", "anthropic returns a single candidate; n>1 is not supported")
	}
	if req.Seed != nil {
		return native, unsupportedParameter("seed", "anthropic does not support deterministic sampling")
	}
	if req.PresencePenalty != nil && *req.PresencePenalty != 0 {
		return native, unsupportedParameter("presence_penalty", "anthropic does not support presence penalties")
	}
	if req.FrequencyPenalty != nil && *req.FrequencyPenalty != 0 {
		return native, unsupportedParameter("frequency_penalty", "anthropic does not support frequency penalties")
	}
	if req.Logprobs != nil && *req.Logprobs {
		return native, unsupportedParameter("logprobs", "anthropic does not return token log probabilities")
	}
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
		case "", "text":
		case "json_object", "json_schema":
			return native, unsupportedParameter("response_format", "anthropic cannot guarantee JSON output for this schema")
		default:
			return native, invalidRequest("unsupported response_format type %q", req.ResponseFormat.Type)
		}
	}
	if req.ReasoningEffort != "" {
		budget := reasoningBudget(req.ReasoningEffort)
		if budget == 0 {
			return native, invalidRequest("unsupported reasoning_effort %q", req.ReasoningEffort)
		}
		if budget >= maxTokens {
			budget = maxTokens - 1024
		}
		if budget < 1024 {
			return native, unsupportedParameter("reasoning_effort", "max_tokens is too small for the reasoning budget")
		}
		if native.Temperature != nil && *native.Temperature != 1 {
			return native, invalidRequest("temperature must be 1 when reasoning_effort is set")
		}
		one := 1.0
		native.Temperature = &one
		native.Thinking = &AnthropicThinking{Type: "enabled", BudgetTokens: budget}
	}

	tools, err := toAnthropicTools(req.Tools)
	if err != nil {
		return native, err
	}
	native.Tools = tools
	native.ToolChoice = toAnthropicToolChoice(req.ToolChoice)

	var systemParts []string
	for _, msg := range req.Messages {
		role := normalizeRole(msg.Role)
		switch role {
		case "system":
			if len(msg.ContentParts) > 0 {
				for _, part := range msg.ContentParts {
					if part.Type != "text" {
						return native, invalidRequest("system messages only support text content")
					}
				}
			}
			systemParts = append(systemParts, msg.Text())
		case "user", "assistant":
			content, err := anthropicMessageContent(msg)
			if err != nil {
				return native, err
			}
			native.Messages = append(native.Messages, AnthropicMessage{Role: role, Content: content})
		case "tool":
			if msg.ToolCallID == "" {
				return native, invalidRequest("tool message requires tool_call_id")
			}
			native.Messages = append(native.Messages, AnthropicMessage{
				Role: "user",
				Content: []AnthropicContentBlock{{
					Type:      "tool_result",
					ToolUseID: msg.ToolCallID,
					Content:   msg.Text(),
				}},
			})
		default:
			return native, invalidRequest("unsupported message role %q", msg.Role)
		}
	}
	native.System = joinSystem(systemParts)
	return native, nil
}

func anthropicMessageContent(msg types.ChatMessage) (any, error) {
	if len(msg.ContentParts) > 0 {
		blocks, err := anthropicContentBlocks(msg.ContentParts)
		if err != nil {
			return nil, err
		}
		if len(msg.ToolCalls) > 0 {
			calls, err := anthropicToolUseBlocks(msg.ToolCalls)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, calls...)
		}
		return blocks, nil
	}
	if len(msg.ToolCalls) > 0 {
		blocks := make([]AnthropicContentBlock, 0, len(msg.ToolCalls)+1)
		if text := msg.Text(); text != "" {
			blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: text})
		}
		calls, err := anthropicToolUseBlocks(msg.ToolCalls)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, calls...)
		return blocks, nil
	}
	return msg.Text(), nil
}

func anthropicContentBlocks(parts []types.ContentPart) ([]AnthropicContentBlock, error) {
	blocks := make([]AnthropicContentBlock, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text":
			if part.Text != "" {
				blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: part.Text})
			}
		case "image_url":
			if part.ImageURL == nil {
				return nil, invalidRequest("image_url content part is missing image_url")
			}
			source, err := anthropicImageSource(part.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, AnthropicContentBlock{Type: "image", Source: source})
		case "input_audio":
			return nil, unsupportedParameter("messages.content.input_audio", "anthropic messages do not accept audio input")
		default:
			return nil, invalidRequest("unsupported content part type %q", part.Type)
		}
	}
	return blocks, nil
}

func anthropicImageSource(raw string) (*AnthropicImageSource, error) {
	if strings.HasPrefix(raw, "data:") {
		mediaType, data, ok := parseDataURI(raw)
		if !ok || !strings.HasPrefix(mediaType, "image/") {
			return nil, invalidRequest("image_url data URI must be a base64 image")
		}
		return &AnthropicImageSource{Type: "base64", MediaType: mediaType, Data: data}, nil
	}
	if isHTTPURL(raw) {
		return &AnthropicImageSource{Type: "url", URL: raw}, nil
	}
	return nil, invalidRequest("image_url must be an http(s) URL or a base64 data URI")
}

func anthropicToolUseBlocks(calls []types.ToolCall) ([]AnthropicContentBlock, error) {
	blocks := make([]AnthropicContentBlock, 0, len(calls))
	for _, call := range calls {
		if call.ID == "" {
			return nil, invalidRequest("tool call requires an id")
		}
		input, err := decodeToolArguments(call.Function.Arguments)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, AnthropicContentBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: input,
		})
	}
	return blocks, nil
}

func toAnthropicTools(tools []types.Tool) ([]AnthropicTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	native := make([]AnthropicTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != "" && tool.Type != "function" {
			return nil, invalidRequest("unsupported tool type %q", tool.Type)
		}
		schema := tool.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		native = append(native, AnthropicTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: schema,
		})
	}
	return native, nil
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

func decodeToolArguments(arguments string) (any, error) {
	if arguments == "" {
		return map[string]any{}, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		return nil, invalidRequest("tool call arguments are not valid JSON: %v", err)
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, invalidRequest("tool call arguments must be a JSON object")
	}
	return decoded, nil
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
				Index: len(toolCalls),
				ID:    block.ID,
				Type:  "function",
				Function: types.ToolCallFunction{
					Name:      block.Name,
					Arguments: arguments,
				},
			})
		}
	}

	promptTokens := resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens
	completionTokens := resp.Usage.OutputTokens
	usage := types.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}
	if resp.Usage.CacheReadInputTokens > 0 {
		usage.PromptTokensDetails = &types.PromptTokensDetails{CachedTokens: resp.Usage.CacheReadInputTokens}
	}

	return types.ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: responseCreated(),
		Model:   resp.Model,
		Choices: []types.Choice{{
			Index:        0,
			Message:      types.ChatMessage{Role: "assistant", Content: text, ToolCalls: toolCalls},
			FinishReason: AnthropicFinishReason(resp.StopReason),
		}},
		Usage: usage,
	}
}

// AnthropicFinishReason maps Anthropic stop reasons to OpenAI's vocabulary.
func AnthropicFinishReason(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func reasoningBudget(effort string) int {
	switch strings.ToLower(effort) {
	case "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 8192
	default:
		return 0
	}
}
