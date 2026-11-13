package translate

import (
	"encoding/json"
	"fmt"
	"strings"

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
	InlineData       *GeminiInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *GeminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
}

type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type GeminiFunctionCall struct {
	Name string `json:"name"`
	Args any    `json:"args,omitempty"`
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

type GeminiThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

type GeminiGenConfig struct {
	Temperature      *float64              `json:"temperature,omitempty"`
	TopP             *float64              `json:"topP,omitempty"`
	TopK             *int                  `json:"topK,omitempty"`
	MaxOutputTokens  *int                  `json:"maxOutputTokens,omitempty"`
	StopSequences    []string              `json:"stopSequences,omitempty"`
	CandidateCount   *int                  `json:"candidateCount,omitempty"`
	Seed             *int                  `json:"seed,omitempty"`
	PresencePenalty  *float64              `json:"presencePenalty,omitempty"`
	FrequencyPenalty *float64              `json:"frequencyPenalty,omitempty"`
	ResponseMimeType string                `json:"responseMimeType,omitempty"`
	ThinkingConfig   *GeminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type GeminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// GeminiResponse is the native Gemini generateContent response body.
type GeminiResponse struct {
	Candidates []struct {
		Content      GeminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata GeminiUsageMetadata `json:"usageMetadata"`
}

// GeminiEmbeddingRequest is native Gemini embedContent request.
type GeminiEmbeddingRequest struct {
	Model                string        `json:"model,omitempty"`
	Content              GeminiContent `json:"content"`
	OutputDimensionality *int          `json:"outputDimensionality,omitempty"`
}

// GeminiBatchEmbeddingRequest is native Gemini batchEmbedContents request.
type GeminiBatchEmbeddingRequest struct {
	Requests []GeminiEmbeddingRequest `json:"requests"`
}

// GeminiEmbeddingResponse is native Gemini embedContent response.
type GeminiEmbeddingResponse struct {
	Embedding struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
}

// GeminiBatchEmbeddingResponse is native Gemini batchEmbedContents response.
type GeminiBatchEmbeddingResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

// ToGemini converts the gateway request, discarding translation errors. It
// exists for backward compatibility; providers use ToGeminiStrict.
func ToGemini(req types.ChatRequest) GeminiRequest {
	native, _ := ToGeminiStrict(req)
	return native
}

// ToGeminiStrict converts the gateway's OpenAI-compatible request into the
// Gemini generateContent format. System/developer messages become the single
// systemInstruction and the assistant role is renamed to "model".
func ToGeminiStrict(req types.ChatRequest) (GeminiRequest, error) {
	config := &GeminiGenConfig{
		MaxOutputTokens: req.EffectiveMaxTokens(),
	}
	if req.Temperature != nil {
		clamped := clamp(*req.Temperature, 0, 2)
		config.Temperature = &clamped
	}
	if req.TopP != nil {
		clamped := clamp(*req.TopP, 0, 1)
		config.TopP = &clamped
	}
	if req.TopK != nil {
		if *req.TopK <= 0 {
			return GeminiRequest{}, invalidRequest("top_k must be positive")
		}
		config.TopK = req.TopK
	}
	stops, err := parseStopSequences(req.Stop)
	if err != nil {
		return GeminiRequest{}, err
	}
	config.StopSequences = stops
	if req.N != nil {
		if *req.N < 1 || *req.N > 8 {
			return GeminiRequest{}, unsupportedParameter("n", "gemini candidateCount must be between 1 and 8")
		}
		config.CandidateCount = req.N
	}
	if req.Seed != nil {
		config.Seed = req.Seed
	}
	if req.PresencePenalty != nil {
		clamped := clamp(*req.PresencePenalty, -2, 2)
		config.PresencePenalty = &clamped
	}
	if req.FrequencyPenalty != nil {
		clamped := clamp(*req.FrequencyPenalty, -2, 2)
		config.FrequencyPenalty = &clamped
	}
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
		case "", "text":
		case "json_object":
			config.ResponseMimeType = "application/json"
		case "json_schema":
			return GeminiRequest{}, unsupportedParameter("response_format", "gemini cannot guarantee arbitrary JSON Schema output")
		default:
			return GeminiRequest{}, invalidRequest("unsupported response_format type %q", req.ResponseFormat.Type)
		}
	}
	if req.ReasoningEffort != "" {
		budget := reasoningBudget(req.ReasoningEffort)
		if budget == 0 {
			return GeminiRequest{}, invalidRequest("unsupported reasoning_effort %q", req.ReasoningEffort)
		}
		config.ThinkingConfig = &GeminiThinkingConfig{ThinkingBudget: budget}
	}

	native := GeminiRequest{
		GenerationConfig: config,
		Tools:            toGeminiTools(req.Tools),
		ToolConfig:       toGeminiToolConfig(req.ToolChoice),
	}

	toolNames := make(map[string]string)
	for _, msg := range req.Messages {
		for _, call := range msg.ToolCalls {
			toolNames[call.ID] = call.Function.Name
		}
	}

	var systemParts []string
	for _, msg := range req.Messages {
		role := normalizeRole(msg.Role)
		switch role {
		case "system":
			if len(msg.ContentParts) > 0 {
				for _, part := range msg.ContentParts {
					if part.Type != "text" {
						return GeminiRequest{}, invalidRequest("system messages only support text content")
					}
				}
			}
			systemParts = append(systemParts, msg.Text())
		case "user", "assistant":
			parts, err := geminiMessageParts(msg)
			if err != nil {
				return GeminiRequest{}, err
			}
			geminiRole := "user"
			if role == "assistant" {
				geminiRole = "model"
			}
			native.Contents = append(native.Contents, GeminiContent{Role: geminiRole, Parts: parts})
		case "tool":
			if len(msg.ContentParts) > 0 {
				for _, part := range msg.ContentParts {
					if part.Type != "text" {
						return GeminiRequest{}, invalidRequest("tool messages only support text content")
					}
				}
			}
			name := toolNames[msg.ToolCallID]
			if name == "" {
				name = msg.Name
			}
			if name == "" {
				return GeminiRequest{}, invalidRequest("tool message references unknown tool_call_id %q", msg.ToolCallID)
			}
			response := any(map[string]any{"content": msg.Text()})
			if object, ok := jsonObject(msg.Text()); ok {
				response = object
			}
			native.Contents = append(native.Contents, GeminiContent{
				Role: "user",
				Parts: []GeminiPart{{FunctionResponse: &GeminiFunctionResponse{
					Name:     name,
					Response: response,
				}}},
			})
		default:
			return GeminiRequest{}, invalidRequest("unsupported message role %q", msg.Role)
		}
	}
	if len(systemParts) > 0 {
		native.SystemInstruction = &GeminiContent{
			Parts: []GeminiPart{{Text: joinSystem(systemParts)}},
		}
	}
	return native, nil
}

func geminiMessageParts(msg types.ChatMessage) ([]GeminiPart, error) {
	parts := make([]GeminiPart, 0, len(msg.ContentParts)+len(msg.ToolCalls)+1)
	if len(msg.ContentParts) > 0 {
		for _, part := range msg.ContentParts {
			switch part.Type {
			case "text":
				if part.Text != "" {
					parts = append(parts, GeminiPart{Text: part.Text})
				}
			case "image_url":
				if part.ImageURL == nil {
					return nil, invalidRequest("image_url content part is missing image_url")
				}
				inline, err := geminiInlineImage(part.ImageURL.URL)
				if err != nil {
					return nil, err
				}
				parts = append(parts, GeminiPart{InlineData: inline})
			case "input_audio":
				if part.InputAudio == nil || part.InputAudio.Data == "" {
					return nil, invalidRequest("input_audio content part is missing data")
				}
				parts = append(parts, GeminiPart{InlineData: &GeminiInlineData{
					MimeType: audioMimeType(part.InputAudio.Format),
					Data:     part.InputAudio.Data,
				}})
			default:
				return nil, invalidRequest("unsupported content part type %q", part.Type)
			}
		}
	} else if text := msg.Text(); text != "" {
		parts = append(parts, GeminiPart{Text: text})
	}
	for _, call := range msg.ToolCalls {
		args, err := decodeGeminiArguments(call.Function.Arguments)
		if err != nil {
			return nil, err
		}
		parts = append(parts, GeminiPart{FunctionCall: &GeminiFunctionCall{
			Name: call.Function.Name,
			Args: args,
		}})
	}
	if len(parts) == 0 {
		parts = []GeminiPart{{Text: ""}}
	}
	return parts, nil
}

func geminiInlineImage(raw string) (*GeminiInlineData, error) {
	if strings.HasPrefix(raw, "data:") {
		mediaType, data, ok := parseDataURI(raw)
		if !ok || !strings.HasPrefix(mediaType, "image/") {
			return nil, invalidRequest("image_url data URI must be a base64 image")
		}
		return &GeminiInlineData{MimeType: mediaType, Data: data}, nil
	}
	return nil, unsupportedParameter("messages.content.image_url", "gemini requires inline base64 images; remote URLs are not fetched")
}

func audioMimeType(format string) string {
	switch strings.ToLower(format) {
	case "":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "ogg", "oga":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	case "webm":
		return "audio/webm"
	case "aac":
		return "audio/aac"
	}
	if strings.Contains(format, "/") {
		return format
	}
	return "audio/" + format
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

func decodeGeminiArguments(arguments string) (any, error) {
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
				if part.FunctionCall.Args != nil {
					if data, err := json.Marshal(part.FunctionCall.Args); err == nil {
						arguments = string(data)
					}
				}
				toolCalls = append(toolCalls, types.ToolCall{
					Index: len(toolCalls),
					ID:    fmt.Sprintf("call_%d", len(toolCalls)),
					Type:  "function",
					Function: types.ToolCallFunction{
						Name:      part.FunctionCall.Name,
						Arguments: arguments,
					},
				})
			}
		}
		finishReason = GeminiFinishReason(candidate.FinishReason, len(toolCalls) > 0)
	}

	promptTokens := resp.UsageMetadata.PromptTokenCount
	completionTokens := resp.UsageMetadata.CandidatesTokenCount + resp.UsageMetadata.ThoughtsTokenCount
	usage := types.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}
	if resp.UsageMetadata.ThoughtsTokenCount > 0 {
		usage.CompletionTokensDetails = &types.CompletionTokensDetails{
			ReasoningTokens: resp.UsageMetadata.ThoughtsTokenCount,
		}
	}

	return types.ChatResponse{
		Object:  "chat.completion",
		Created: responseCreated(),
		Model:   model,
		Choices: []types.Choice{{
			Index:        0,
			Message:      types.ChatMessage{Role: "assistant", Content: text, ToolCalls: toolCalls},
			FinishReason: finishReason,
		}},
		Usage: usage,
	}
}

// GeminiFinishReason maps Gemini finish reasons to OpenAI's vocabulary.
func GeminiFinishReason(reason string, hasToolCalls bool) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "MALFORMED_FUNCTION_CALL":
		return "content_filter"
	case "STOP":
		if hasToolCalls {
			return "tool_calls"
		}
		return "stop"
	case "":
		if hasToolCalls {
			return "tool_calls"
		}
		return "stop"
	default:
		return "stop"
	}
}

// ToGeminiEmbedding converts an OpenAI embedding input to Gemini embedContent.
func ToGeminiEmbedding(input string) GeminiEmbeddingRequest {
	return GeminiEmbeddingRequest{
		Content: GeminiContent{
			Parts: []GeminiPart{{Text: input}},
		},
	}
}

// ToGeminiEmbeddingWithOptions builds an embedContent request carrying the
// model and output dimensionality required by batchEmbedContents.
func ToGeminiEmbeddingWithOptions(input, model string, dimensions *int) GeminiEmbeddingRequest {
	request := ToGeminiEmbedding(input)
	if model != "" {
		request.Model = "models/" + model
	}
	request.OutputDimensionality = dimensions
	return request
}

// FromGeminiEmbedding converts a Gemini embedContent response to OpenAI embedding data.
func FromGeminiEmbedding(resp GeminiEmbeddingResponse, model string, index int) types.EmbeddingData {
	return types.EmbeddingData{
		Object:    "embedding",
		Index:     index,
		Embedding: resp.Embedding.Values,
	}
}
