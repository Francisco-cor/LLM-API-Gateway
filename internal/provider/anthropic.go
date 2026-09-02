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

	"github.com/fcordero/llm-api-gateway/internal/translate"
)

const anthropicVersion = translate.AnthropicVersion

// defaultMaxTokens is used when the request does not specify max_tokens,
// which the Anthropic Messages API requires.
const defaultMaxTokens = translate.DefaultMaxTokens

// Keep local type aliases for backward compat and JSON compatibility;
// actual logic delegated to internal/translate.
type anthropicRequest = translate.AnthropicRequest
type anthropicMessage = translate.AnthropicMessage
type anthropicResponse = translate.AnthropicResponse

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
		client:  newHTTPClient(timeout),
	}
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) Models() []string { return a.models }

func (a *Anthropic) SetModels(models []string) { a.models = models }

// anthropicErrorResponse is the native Anthropic error body.
type anthropicErrorResponse struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *Anthropic) Send(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body, err := marshalJSON(translate.ToAnthropic(req))
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
			RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var native anthropicResponse
	if err := json.Unmarshal(data, &native); err != nil {
		return ChatResponse{}, fmt.Errorf("parse response: %w", err)
	}
	return translate.FromAnthropic(native), nil
}

func (a *Anthropic) SendStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, <-chan error) {
	ch := make(chan StreamChunk, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		native := translate.ToAnthropic(req)
		// enable streaming via native field
		type streamReq struct {
			anthropicRequest
			Stream bool `json:"stream"`
		}
		sr := streamReq{anthropicRequest: native, Stream: true}
		body, err := marshalJSON(sr)
		if err != nil {
			errCh <- fmt.Errorf("marshal request: %w", err)
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			errCh <- fmt.Errorf("build request: %w", err)
			return
		}
		httpReq.Header.Set("x-api-key", a.apiKey)
		httpReq.Header.Set("anthropic-version", anthropicVersion)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := a.client.Do(httpReq)
		if err != nil {
			errCh <- &ProviderError{ProviderName: a.Name(), Message: err.Error(), Retryable: true}
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			errCh <- &ProviderError{
				ProviderName: a.Name(),
				StatusCode:   resp.StatusCode,
				Message:      anthropicErrorMessage(data),
				Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
				RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
			}
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 4096)
		scanner.Buffer(buf, 1<<20)
		var currentEvent string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				continue
			}
			switch currentEvent {
			case "content_block_delta":
				var evt struct {
					Delta struct {
						Text string `json:"text"`
					} `json:"delta"`
					Index int `json:"index"`
				}
				if err := json.Unmarshal([]byte(payload), &evt); err != nil {
					continue
				}
				delta := evt.Delta.Text
				if delta == "" {
					continue
				}
				chunk := StreamChunk{
					ID:      "chatcmpl-anthropic",
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   req.Model,
					Choices: []StreamChoice{{
						Index: 0,
						Delta: ChatMessage{Role: "assistant", Content: delta},
					}},
				}
				select {
				case ch <- chunk:
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}
			case "message_stop":
				return
			}
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			errCh <- err
		}
	}()

	return ch, errCh
}

func (a *Anthropic) HealthCheck(ctx context.Context) error {
	// Anthropic has no models-list endpoint; send the smallest possible
	// request and treat anything other than 401/403 as healthy.
	body, _ := marshalJSON(anthropicRequest{
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

// Embed is not supported by Anthropic (no embedding models); returns 404.
func (a *Anthropic) Embed(_ context.Context, _ EmbeddingRequest) (EmbeddingResponse, error) {
	return EmbeddingResponse{}, &ProviderError{
		ProviderName: a.Name(),
		StatusCode:   404,
		Message:      "embeddings not supported for anthropic",
		Retryable:    false,
	}
}

// translateToAnthropic retained for backward compat (delegates to translate package).
func translateToAnthropic(req ChatRequest) anthropicRequest { return translate.ToAnthropic(req) }

// translateFromAnthropic retained for backward compat (delegates to translate package).
func translateFromAnthropic(resp anthropicResponse) ChatResponse { return translate.FromAnthropic(resp) }

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
