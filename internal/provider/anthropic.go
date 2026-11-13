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
	apiKey       string
	baseURL      string
	baseErr      error
	models       []string
	client       *http.Client
	streamClient *http.Client
}

func NewAnthropic(apiKey, baseURL string, timeout time.Duration, models []string) *Anthropic {
	base, baseErr := normalizeBaseURL(baseURL)
	return &Anthropic{
		apiKey:       apiKey,
		baseURL:      base,
		baseErr:      baseErr,
		models:       models,
		client:       newHTTPClient(timeout),
		streamClient: newStreamHTTPClient(),
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
	if a.baseErr != nil {
		return ChatResponse{}, a.baseErr
	}
	nativeRequest, err := translate.ToAnthropicStrict(req)
	if err != nil {
		return ChatResponse{}, err
	}
	body, err := marshalJSON(nativeRequest)
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

	data, err := readProviderResponse(resp.Body)
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
	result := translate.FromAnthropic(native)
	result.Model = req.Model
	result.Created = time.Now().Unix()
	return result, nil
}

func (a *Anthropic) SendStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, <-chan error) {
	ch := make(chan StreamChunk, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		if a.baseErr != nil {
			errCh <- a.baseErr
			return
		}
		native, err := translate.ToAnthropicStrict(req)
		if err != nil {
			errCh <- err
			return
		}
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

		resp, err := a.streamClient.Do(httpReq)
		if err != nil {
			errCh <- &ProviderError{ProviderName: a.Name(), Message: err.Error(), Retryable: true}
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			data, _ := readProviderResponse(resp.Body)
			errCh <- &ProviderError{
				ProviderName: a.Name(),
				StatusCode:   resp.StatusCode,
				Message:      anthropicErrorMessage(data),
				Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
				RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
			}
			return
		}

		created := time.Now().Unix()
		messageID := "chatcmpl-anthropic"
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		promptTokens, completionTokens, cachedTokens := 0, 0, 0
		toolBlocks := make(map[int]struct {
			id   string
			name string
		})

		emit := func(chunk StreamChunk) bool {
			select {
			case ch <- chunk:
				return true
			case <-ctx.Done():
				errCh <- ctx.Err()
				return false
			}
		}
		emitUsage := func() bool {
			if !includeUsage || (promptTokens == 0 && completionTokens == 0) {
				return true
			}
			usage := &Usage{
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      promptTokens + completionTokens,
			}
			if cachedTokens > 0 {
				usage.PromptTokensDetails = &PromptTokensDetails{CachedTokens: cachedTokens}
			}
			return emit(StreamChunk{
				ID:      messageID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   req.Model,
				Choices: []StreamChoice{},
				Usage:   usage,
			})
		}

		reader := newSSEReader(resp.Body)
		sawStop := false
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
					errCh <- sseEventTooLargeError(a.Name())
					return
				}
				errCh <- &ProviderError{ProviderName: a.Name(), Message: err.Error(), Retryable: false}
				return
			}
			payload := strings.TrimSpace(event.Data)
			if payload == "" {
				continue
			}
			eventName := event.Name
			if eventName == "" {
				var envelope struct {
					Type string `json:"type"`
				}
				if json.Unmarshal([]byte(payload), &envelope) == nil {
					eventName = envelope.Type
				}
			}

			switch eventName {
			case "ping":
				continue
			case "error":
				var errResp anthropicErrorResponse
				if err := json.Unmarshal([]byte(payload), &errResp); err != nil || errResp.Error.Message == "" {
					errCh <- &ProviderError{ProviderName: a.Name(), Message: truncateProviderError(payload), Retryable: false}
					return
				}
				errCh <- &ProviderError{
					ProviderName: a.Name(),
					Message:      truncateProviderError(errResp.Error.Message),
					Retryable:    errResp.Error.Type == "overloaded_error" || errResp.Error.Type == "api_error",
				}
				return
			case "message_start":
				var evt struct {
					Message struct {
						ID    string `json:"id"`
						Usage struct {
							InputTokens              int `json:"input_tokens"`
							CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
							CacheReadInputTokens     int `json:"cache_read_input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				if json.Unmarshal([]byte(payload), &evt) == nil {
					if evt.Message.ID != "" {
						messageID = evt.Message.ID
					}
					promptTokens = evt.Message.Usage.InputTokens +
						evt.Message.Usage.CacheCreationInputTokens +
						evt.Message.Usage.CacheReadInputTokens
					cachedTokens = evt.Message.Usage.CacheReadInputTokens
				}
			case "content_block_start":
				var evt struct {
					Index        int `json:"index"`
					ContentBlock struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content_block"`
				}
				if json.Unmarshal([]byte(payload), &evt) == nil && evt.ContentBlock.Type == "tool_use" {
					toolBlocks[evt.Index] = struct {
						id   string
						name string
					}{id: evt.ContentBlock.ID, name: evt.ContentBlock.Name}
					if !emit(StreamChunk{
						ID:      messageID,
						Object:  "chat.completion.chunk",
						Created: created,
						Model:   req.Model,
						Choices: []StreamChoice{{
							Index: 0,
							Delta: ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{
								Index: evt.Index,
								ID:    evt.ContentBlock.ID,
								Type:  "function",
								Function: ToolCallFunction{
									Name: evt.ContentBlock.Name,
								},
							}}},
						}},
					}) {
						return
					}
				}
			case "content_block_delta":
				var evt struct {
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
					Index int `json:"index"`
				}
				if json.Unmarshal([]byte(payload), &evt) != nil {
					continue
				}
				if evt.Delta.Text != "" {
					if !emit(StreamChunk{
						ID:      messageID,
						Object:  "chat.completion.chunk",
						Created: created,
						Model:   req.Model,
						Choices: []StreamChoice{{
							Index: 0,
							Delta: ChatMessage{Role: "assistant", Content: evt.Delta.Text},
						}},
					}) {
						return
					}
				}
				if evt.Delta.PartialJSON != "" {
					if _, ok := toolBlocks[evt.Index]; ok {
						if !emit(StreamChunk{
							ID:      messageID,
							Object:  "chat.completion.chunk",
							Created: created,
							Model:   req.Model,
							Choices: []StreamChoice{{
								Index: 0,
								Delta: ChatMessage{ToolCalls: []ToolCall{{
									Index:    evt.Index,
									Function: ToolCallFunction{Arguments: evt.Delta.PartialJSON},
								}}},
							}},
						}) {
							return
						}
					}
				}
			case "message_delta":
				var evt struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage struct {
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal([]byte(payload), &evt) != nil {
					continue
				}
				completionTokens = evt.Usage.OutputTokens
				if evt.Delta.StopReason != "" {
					finishReason := translate.AnthropicFinishReason(evt.Delta.StopReason)
					if !emit(StreamChunk{
						ID:      messageID,
						Object:  "chat.completion.chunk",
						Created: created,
						Model:   req.Model,
						Choices: []StreamChoice{{Index: 0, FinishReason: &finishReason}},
					}) {
						return
					}
				}
			case "message_stop":
				sawStop = true
				if !emitUsage() {
					return
				}
				return
			}
		}
		if !sawStop {
			errCh <- streamTruncatedError(a.Name())
		}
	}()

	return ch, errCh
}

func (a *Anthropic) HealthCheck(ctx context.Context) error {
	if a.baseErr != nil {
		return a.baseErr
	}
	// Models listing is cheap and does not consume generation tokens.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/v1/models?limit=1", nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("unauthorized: invalid API key")
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("rate limited (HTTP %d)", resp.StatusCode)
	case resp.StatusCode >= 500:
		return fmt.Errorf("upstream unavailable (HTTP %d)", resp.StatusCode)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return a.probeMessagesEndpoint(ctx)
	default:
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
}

// probeMessagesEndpoint performs a body-less HEAD so old API versions that do
// not expose /v1/models can still be checked without spending tokens.
func (a *Anthropic) probeMessagesEndpoint(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodHead, a.baseURL+"/v1/messages", nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return err
	}
	resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("unauthorized: invalid API key")
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("rate limited (HTTP %d)", resp.StatusCode)
	case resp.StatusCode >= 500:
		return fmt.Errorf("upstream unavailable (HTTP %d)", resp.StatusCode)
	default:
		return nil
	}
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
func translateFromAnthropic(resp anthropicResponse) ChatResponse {
	return translate.FromAnthropic(resp)
}

func anthropicErrorMessage(body []byte) string {
	var errResp anthropicErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
		return truncateProviderError(errResp.Error.Message)
	}
	return providerErrorMessage(body)
}
