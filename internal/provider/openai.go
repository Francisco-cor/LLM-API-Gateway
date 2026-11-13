package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	apiKey       string
	baseURL      string
	baseErr      error
	models       []string
	client       *http.Client
	streamClient *http.Client
}

func NewOpenAI(apiKey, baseURL string, timeout time.Duration, models []string) *OpenAI {
	base, baseErr := normalizeBaseURL(baseURL)
	return &OpenAI{
		apiKey:       apiKey,
		baseURL:      base,
		baseErr:      baseErr,
		models:       models,
		client:       newHTTPClient(timeout),
		streamClient: newStreamHTTPClient(),
	}
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) Models() []string { return o.models }

func (o *OpenAI) SetModels(models []string) { o.models = models }

func (o *OpenAI) Send(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if o.baseErr != nil {
		return ChatResponse{}, o.baseErr
	}
	body, err := marshalJSON(req)
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

	data, err := readProviderResponse(resp.Body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return ChatResponse{}, &ProviderError{
			ProviderName: o.Name(),
			StatusCode:   resp.StatusCode,
			Message:      providerErrorMessage(data),
			Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
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

		if o.baseErr != nil {
			errCh <- o.baseErr
			return
		}
		req.Stream = true
		body, err := marshalJSON(req)
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

		resp, err := o.streamClient.Do(httpReq)
		if err != nil {
			errCh <- &ProviderError{ProviderName: o.Name(), Message: err.Error(), Retryable: true}
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			data, _ := readProviderResponse(resp.Body)
			errCh <- &ProviderError{
				ProviderName: o.Name(),
				StatusCode:   resp.StatusCode,
				Message:      providerErrorMessage(data),
				Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
				RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
			}
			return
		}

		reader := newSSEReader(resp.Body)
		sawDone := false
		for {
			event, err := reader.Next()
			if err != nil {
				if ctx.Err() != nil {
					errCh <- ctx.Err()
					return
				}
				if errors.Is(err, io.EOF) {
					break
				}
				if errors.Is(err, errSSEEventTooLarge) {
					errCh <- sseEventTooLargeError(o.Name())
					return
				}
				errCh <- &ProviderError{ProviderName: o.Name(), Message: err.Error(), Retryable: false}
				return
			}
			payload := strings.TrimSpace(event.Data)
			if payload == "" {
				continue
			}
			if payload == "[DONE]" {
				sawDone = true
				break
			}
			if providerErr := providerStreamError(o.Name(), []byte(payload)); providerErr != nil {
				errCh <- providerErr
				return
			}
			var chunk StreamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				errCh <- &ProviderError{
					ProviderName: o.Name(),
					Message:      "invalid upstream stream chunk: " + err.Error(),
					Retryable:    false,
				}
				return
			}
			if chunk.Choices == nil {
				chunk.Choices = []StreamChoice{}
			}
			select {
			case ch <- chunk:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
		if !sawDone {
			errCh <- streamTruncatedError(o.Name())
		}
	}()

	return ch, errCh
}

func (o *OpenAI) HealthCheck(ctx context.Context) error {
	if o.baseErr != nil {
		return o.baseErr
	}
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

func (o *OpenAI) Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error) {
	if o.baseErr != nil {
		return EmbeddingResponse{}, o.baseErr
	}
	body, err := marshalJSON(req)
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return EmbeddingResponse{}, &ProviderError{ProviderName: o.Name(), Message: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	data, err := readProviderResponse(resp.Body)
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return EmbeddingResponse{}, &ProviderError{
			ProviderName: o.Name(),
			StatusCode:   resp.StatusCode,
			Message:      providerErrorMessage(data),
			Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	var result EmbeddingResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return EmbeddingResponse{}, fmt.Errorf("parse response: %w", err)
	}
	return result, nil
}

// DiscoverModels fetches available models from OpenAI /v1/models.
func (o *OpenAI) DiscoverModels(ctx context.Context) ([]string, error) {
	if o.baseErr != nil {
		return nil, o.baseErr
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var models []string
	for _, m := range body.Data {
		models = append(models, m.ID)
	}
	return models, nil
}
