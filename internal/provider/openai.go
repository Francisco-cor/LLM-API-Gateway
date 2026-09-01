package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

func (o *OpenAI) SendStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, <-chan error) {
	ch := make(chan StreamChunk, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		req.Stream = true
		body, err := json.Marshal(req)
		if err != nil {
			errCh <- fmt.Errorf("marshal request: %w", err)
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			errCh <- fmt.Errorf("build request: %w", err)
			return
		}
		httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := o.client.Do(httpReq)
		if err != nil {
			errCh <- &ProviderError{ProviderName: o.Name(), Message: err.Error(), Retryable: true}
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			errCh <- &ProviderError{
				ProviderName: o.Name(),
				StatusCode:   resp.StatusCode,
				Message:      string(data),
				Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			}
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		// increase buffer for large chunks
		buf := make([]byte, 0, 4096)
		scanner.Buffer(buf, 1<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				return
			}
			var chunk StreamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				// skip malformed chunk, log via errCh but continue
				continue
			}
			select {
			case ch <- chunk:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			errCh <- err
		}
	}()

	return ch, errCh
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
