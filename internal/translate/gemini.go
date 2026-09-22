package translate

import (
	"encoding/json"

	"github.com/fcordero/llm-api-gateway/internal/types"
)

// GeminiRequest is the native Gemini generateContent request body.
type GeminiRequest struct {
	Contents          []GeminiContent   `json:"contents"`
	SystemInstruction *GeminiContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *GeminiGenConfig  `json:"generationConfig,omitempty"`
	Tools             []GeminiTool      `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig `json:"toolConfig,omitempty"`
}

type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts"`
}

type GeminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *GeminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
}

type GeminiFunctionCall struct {
	Name string `json:"name"`
	Args any    `json:"args"`
}

type GeminiFunctionResponse struct {
	Name     string `json:"name"`
	Response any    `json:"response"`
}

type GeminiTool struct {
	FunctionDeclarations []GeminiFunctionDeclaration `json:"functionDeclarations"`
}

type GeminiFunctionDeclaration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type GeminiToolConfig struct {
	FunctionCallingConfig GeminiFunctionCallingConfig `json:"functionCallingConfig"`
}

type GeminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type GeminiGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
}

// GeminiResponse is the native Gemini generateContent response body.
type GeminiResponse struct {
	Candidates []struct {
		Content      GeminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// GeminiEmbeddingRequest is native Gemini embedContent request.
type GeminiEmbeddingRequest struct {
	Content GeminiContent `json:"content"`
}

// GeminiEmbeddingResponse is native Gemini embedContent response.
type GeminiEmbeddingResponse struct {
	Embedding struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
}

// ToGemini converts the gateway's OpenAI-compatible request into the
// Gemini generateContent format. System messages become "systemInstruction"
// and the assistant role is renamed to "model" as required by Gemini.
func ToGemini(req types.ChatRequest) GeminiRequest {
	native := GeminiRequest{
		GenerationConfig: &GeminiGenConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: req.EffectiveMaxTokens(),
		},
		Tools:      toGeminiTools(req.Tools),
		ToolConfig: toGeminiToolConfig(req.ToolChoice),
	}
	toolNames := make(map[string]string)
	for _, msg := range req.Messages {
		for _, call := range msg.ToolCalls {
			toolNames[call.ID] = call.Function.Name
		}
	}
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			native.SystemInstruction = &GeminiContent{
				Parts: []GeminiPart{{Text: msg.Text()}},
			}
			continue
		}
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		parts := make([]GeminiPart, 0, len(msg.ToolCalls)+1)
		if text := msg.Text(); text != "" {
			parts = append(parts, GeminiPart{Text: text})
		}
		for _, call := range msg.ToolCalls {
			parts = append(parts, GeminiPart{FunctionCall: &GeminiFunctionCall{
				Name: call.Function.Name,
				Args: decodeGeminiArguments(call.Function.Arguments),
			}})
		}
		if msg.Role == "tool" {
			role = "user"
			name := msg.ToolCallID
			if mapped := toolNames[msg.ToolCallID]; mapped != "" {
				name = mapped
			}
			parts = []GeminiPart{{FunctionResponse: &GeminiFunctionResponse{
				Name:     name,
				Response: map[string]any{"content": msg.Text()},
			}}}
		}
		if len(parts) == 0 {
			parts = []GeminiPart{{Text: ""}}
		}
		native.Contents = append(native.Contents, GeminiContent{Role: role, Parts: parts})
	}
	return native
}

func toGeminiTools(tools []types.Tool) []GeminiTool {
	if len(tools) == 0 {
		return nil
	}
	declarations := make([]GeminiFunctionDeclaration, 0, len(tools))
	for _, tool := range tools {
		declarations = append(declarations, GeminiFunctionDeclaration{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}
	return []GeminiTool{{FunctionDeclarations: declarations}}
}

func toGeminiToolConfig(choice any) *GeminiToolConfig {
	if choice == nil {
		return nil
	}
	config := &GeminiToolConfig{}
	switch value := choice.(type) {
	case string:
		switch value {
		case "none":
			config.FunctionCallingConfig.Mode = "NONE"
		case "required":
			config.FunctionCallingConfig.Mode = "ANY"
		default:
			config.FunctionCallingConfig.Mode = "AUTO"
		}
	case map[string]any:
		config.FunctionCallingConfig.Mode = "ANY"
		if function, ok := value["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok {
				config.FunctionCallingConfig.AllowedFunctionNames = []string{name}
			}
		}
	default:
		return nil
	}
	return config
}

func decodeGeminiArguments(arguments string) any {
	if arguments == "" {
		return map[string]any{}
	}
	var decoded any
	if json.Unmarshal([]byte(arguments), &decoded) == nil {
		return decoded
	}
	return map[string]any{"raw": arguments}
}

// FromGemini converts a Gemini generateContent response back into the
// gateway's OpenAI-compatible format.
func FromGemini(resp GeminiResponse, model string) types.ChatResponse {
	text := ""
	var toolCalls []types.ToolCall
	finishReason := "stop"
	if len(resp.Candidates) > 0 {
		candidate := resp.Candidates[0]
		for _, part := range candidate.Content.Parts {
			text += part.Text
			if part.FunctionCall != nil {
				arguments := "{}"
				if data, err := json.Marshal(part.FunctionCall.Args); err == nil {
					arguments = string(data)
				}
				toolCalls = append(toolCalls, types.ToolCall{
					ID:   part.FunctionCall.Name,
					Type: "function",
					Function: types.ToolCallFunction{
						Name:      part.FunctionCall.Name,
						Arguments: arguments,
					},
				})
			}
		}
		if candidate.FinishReason == "MAX_TOKENS" {
			finishReason = "length"
		} else if candidate.FinishReason == "STOP" && len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}
	}
	return types.ChatResponse{
		Object: "chat.completion",
		Model:  model,
		Choices: []types.Choice{{
			Index:        0,
			Message:      types.ChatMessage{Role: "assistant", Content: text, ToolCalls: toolCalls},
			FinishReason: finishReason,
		}},
		Usage: types.Usage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		},
	}
}

// ToGeminiEmbedding converts an OpenAI embedding request to Gemini embedContent.
// For batch (multiple inputs) the caller should invoke per-input.
func ToGeminiEmbedding(input string) GeminiEmbeddingRequest {
	return GeminiEmbeddingRequest{
		Content: GeminiContent{
			Parts: []GeminiPart{{Text: input}},
		},
	}
}

// FromGeminiEmbedding converts a Gemini embedContent response to OpenAI embedding data.
func FromGeminiEmbedding(resp GeminiEmbeddingResponse, model string, index int) types.EmbeddingData {
	return types.EmbeddingData{
		Object:    "embedding",
		Index:     index,
		Embedding: resp.Embedding.Values,
	}
}
