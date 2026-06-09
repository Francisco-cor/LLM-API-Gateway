package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const anthropicVersion = "2023-06-01"

// defaultMaxTokens is used when the request does not specify max_tokens,
// which the Anthropic Messages API requires.
const defaultMaxTokens = 4096

// Anthropic implements Provider for the Anthropic Messages API, translating
// to and from the gateway's OpenAI-compatible format.
type Anthropic struct {
	apiKey  string
	baseURL string
	models  []string
	client  *http.Client
}

func NewAnthropic(apiKey, baseURL string, timeout time.Duration, models []string) *Anthropic {
	return &Anthropic{
		apiKey:  apiKey,
		baseURL: baseURL,
		models:  models,
		client:  &http.Client{Timeout: timeout},
	}
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) Models() []string { return a.models }

// anthropicRequest is the native Anthropic Messages API request body.
type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicResponse is the native Anthropic Messages API response body.
type anthropicResponse struct {
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

// anthropicErrorResponse is the native Anthropic error body.
type anthropicErrorResponse struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *Anthropic) Send(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body, err := json.Marshal(translateToAnthropic(req))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, &ProviderError{
			ProviderName: a.Name(),
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
			ProviderName: a.Name(),
			StatusCode:   resp.StatusCode,
			Message:      anthropicErrorMessage(data),
			Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
		}
	}

	var native anthropicResponse
	if err := json.Unmarshal(data, &native); err != nil {
		return ChatResponse{}, fmt.Errorf("parse response: %w", err)
	}
	return translateFromAnthropic(native), nil
}

func (a *Anthropic) HealthCheck(ctx context.Context) error {
	// Anthropic has no models-list endpoint; send the smallest possible
	// request and treat anything other than 401/403 as healthy.
	body, _ := json.Marshal(anthropicRequest{
		Model:     firstOrDefault(a.models, "claude-haiku-4-5-20251001"),
		MaxTokens: 1,
		Messages:  []anthropicMessage{{Role: "user", Content: "ping"}},
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("unauthorized: invalid API key")
	}
	return nil
}

// translateToAnthropic converts the gateway's OpenAI-compatible request into
// the Anthropic Messages API format. System messages are extracted into the
// top-level "system" field since Anthropic does not accept a "system" role
// inside the messages array.
func translateToAnthropic(req ChatRequest) anthropicRequest {
	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}

	native := anthropicRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
	}

	for _, msg := range req.Messages {
		if msg.Role == "system" {
			native.System = msg.Content
			continue
		}
		native.Messages = append(native.Messages, anthropicMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}
	return native
}

// translateFromAnthropic converts an Anthropic Messages API response back
// into the gateway's OpenAI-compatible format.
func translateFromAnthropic(resp anthropicResponse) ChatResponse {
	text := ""
	if len(resp.Content) > 0 {
		text = resp.Content[0].Text
	}

	finishReason := "stop"
	if resp.StopReason == "max_tokens" {
		finishReason = "length"
	}

	return ChatResponse{
		ID:     resp.ID,
		Object: "chat.completion",
		Model:  resp.Model,
		Choices: []Choice{{
			Index:        0,
			Message:      ChatMessage{Role: "assistant", Content: text},
			FinishReason: finishReason,
		}},
		Usage: Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

func anthropicErrorMessage(body []byte) string {
	var errResp anthropicErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
		return errResp.Error.Message
	}
	return string(body)
}

func firstOrDefault(values []string, def string) string {
	if len(values) > 0 {
		return values[0]
	}
	return def
}
