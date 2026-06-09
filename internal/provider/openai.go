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

// OpenAI implements Provider for the OpenAI Chat Completions API. Since the
// gateway's unified format is already OpenAI-compatible, no translation is
// required for this provider.
type OpenAI struct {
	apiKey  string
	baseURL string
	models  []string
	client  *http.Client
}

func NewOpenAI(apiKey, baseURL string, timeout time.Duration, models []string) *OpenAI {
	return &OpenAI{
		apiKey:  apiKey,
		baseURL: baseURL,
		models:  models,
		client:  &http.Client{Timeout: timeout},
	}
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) Models() []string { return o.models }

func (o *OpenAI) Send(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, &ProviderError{
			ProviderName: o.Name(),
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
			ProviderName: o.Name(),
			StatusCode:   resp.StatusCode,
			Message:      string(data),
			Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
		}
	}

	var result ChatResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return ChatResponse{}, fmt.Errorf("parse response: %w", err)
	}
	return result, nil
}

func (o *OpenAI) HealthCheck(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/models", nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}
