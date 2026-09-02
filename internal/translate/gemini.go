package translate

import (
	"github.com/fcordero/llm-api-gateway/internal/types"
)

// GeminiRequest is the native Gemini generateContent request body.
type GeminiRequest struct {
	Contents          []GeminiContent  `json:"contents"`
	SystemInstruction *GeminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *GeminiGenConfig `json:"generationConfig,omitempty"`
}

type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts"`
}

type GeminiPart struct {
	Text string `json:"text"`
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
			MaxOutputTokens: req.MaxTokens,
		},
	}
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			native.SystemInstruction = &GeminiContent{
				Parts: []GeminiPart{{Text: msg.Content}},
			}
			continue
		}
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		native.Contents = append(native.Contents, GeminiContent{
			Role:  role,
			Parts: []GeminiPart{{Text: msg.Content}},
		})
	}
	return native
}

// FromGemini converts a Gemini generateContent response back into the
// gateway's OpenAI-compatible format.
func FromGemini(resp GeminiResponse, model string) types.ChatResponse {
	text := ""
	finishReason := "stop"
	if len(resp.Candidates) > 0 {
		candidate := resp.Candidates[0]
		if len(candidate.Content.Parts) > 0 {
			text = candidate.Content.Parts[0].Text
		}
		if candidate.FinishReason == "MAX_TOKENS" {
			finishReason = "length"
		}
	}
	return types.ChatResponse{
		Object: "chat.completion",
		Model:  model,
		Choices: []types.Choice{{
			Index:        0,
			Message:      types.ChatMessage{Role: "assistant", Content: text},
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
