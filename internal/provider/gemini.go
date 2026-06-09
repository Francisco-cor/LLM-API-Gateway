package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Gemini implements Provider for the Google Gemini generateContent API,
// translating to and from the gateway's OpenAI-compatible format.
type Gemini struct {
	apiKey  string
	baseURL string
	models  []string
	client  *http.Client
}

func NewGemini(apiKey, baseURL string, timeout time.Duration, models []string) *Gemini {
	return &Gemini{
		apiKey:  apiKey,
		baseURL: baseURL,
		models:  models,
		client:  &http.Client{Timeout: timeout},
	}
}

func (g *Gemini) Name() string { return "gemini" }

func (g *Gemini) Models() []string { return g.models }

// geminiRequest is the native Gemini generateContent request body.
type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
}

// geminiResponse is the native Gemini generateContent response body.
type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// geminiErrorResponse is the native Gemini error body.
type geminiErrorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (g *Gemini) Send(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body, err := json.Marshal(translateToGemini(req))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/models/%s:generateContent?key=%s", g.baseURL, req.Model, url.QueryEscape(g.apiKey))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, &ProviderError{
			ProviderName: g.Name(),
			Message:      err.Error(),
			Retryable:    true,
		}
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return ChatResponse{}, &ProviderError{
			ProviderName: g.Name(),
			StatusCode:   resp.StatusCode,
			Message:      geminiErrorMessage(data),
			Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
		}
	}

	var native geminiResponse
	if err := json.Unmarshal(data, &native); err != nil {
		return ChatResponse{}, fmt.Errorf("parse response: %w", err)
	}
	return translateFromGemini(native, req.Model), nil
}

func (g *Gemini) HealthCheck(ctx context.Context) error {
	endpoint := fmt.Sprintf("%s/models?key=%s", g.baseURL, url.QueryEscape(g.apiKey))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// translateToGemini converts the gateway's OpenAI-compatible request into the
// Gemini generateContent format. System messages become "systemInstruction"
// and the assistant role is renamed to "model" as required by the Gemini API.
func translateToGemini(req ChatRequest) geminiRequest {
	native := geminiRequest{
		GenerationConfig: &geminiGenConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: req.MaxTokens,
		},
	}

	for _, msg := range req.Messages {
		if msg.Role == "system" {
			native.SystemInstruction = &geminiContent{
				Parts: []geminiPart{{Text: msg.Content}},
			}
			continue
		}
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		native.Contents = append(native.Contents, geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: msg.Content}},
		})
	}
	return native
}

// translateFromGemini converts a Gemini generateContent response back into
// the gateway's OpenAI-compatible format.
func translateFromGemini(resp geminiResponse, model string) ChatResponse {
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

	return ChatResponse{
		Object: "chat.completion",
		Model:  model,
		Choices: []Choice{{
			Index:        0,
			Message:      ChatMessage{Role: "assistant", Content: text},
			FinishReason: finishReason,
		}},
		Usage: Usage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		},
	}
}

func geminiErrorMessage(body []byte) string {
	var errResp geminiErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
		return errResp.Error.Message
	}
	return string(body)
}
