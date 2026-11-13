package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/translate"
)

// Keep local type aliases for JSON compatibility; logic delegated to translate.
type geminiRequest = translate.GeminiRequest
type geminiContent = translate.GeminiContent
type geminiPart = translate.GeminiPart
type geminiGenConfig = translate.GeminiGenConfig
type geminiResponse = translate.GeminiResponse

// Gemini implements Provider for the Google Gemini generateContent API,
// translating to and from the gateway's OpenAI-compatible format.
type Gemini struct {
	apiKey       string
	baseURL      string
	baseErr      error
	models       []string
	client       *http.Client
	streamClient *http.Client
}

func NewGemini(apiKey, baseURL string, timeout time.Duration, models []string) *Gemini {
	base, baseErr := normalizeBaseURL(baseURL)
	return &Gemini{
		apiKey:       apiKey,
		baseURL:      base,
		baseErr:      baseErr,
		models:       models,
		client:       newHTTPClient(timeout),
		streamClient: newStreamHTTPClient(),
	}
}

func (g *Gemini) Name() string { return "gemini" }

func (g *Gemini) Models() []string { return g.models }

func (g *Gemini) SetModels(models []string) { g.models = models }

// geminiErrorResponse is the native Gemini error body.
type geminiErrorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// geminiModelPath rejects model names that would alter the request path.
func geminiModelPath(model string) (string, error) {
	if model == "" {
		return "", fmt.Errorf("model is required")
	}
	if strings.ContainsAny(model, "/?#:") {
		return "", fmt.Errorf("invalid gemini model %q", model)
	}
	return url.PathEscape(model), nil
}

func (g *Gemini) Send(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if g.baseErr != nil {
		return ChatResponse{}, g.baseErr
	}
	modelPath, err := geminiModelPath(req.Model)
	if err != nil {
		return ChatResponse{}, err
	}
	nativeRequest, err := translate.ToGeminiStrict(req)
	if err != nil {
		return ChatResponse{}, err
	}
	body, err := marshalJSON(nativeRequest)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/models/%s:generateContent", g.baseURL, modelPath)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, &ProviderError{
			ProviderName: g.Name(),
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
			ProviderName: g.Name(),
			StatusCode:   resp.StatusCode,
			Message:      geminiErrorMessage(data),
			Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var native geminiResponse
	if err := json.Unmarshal(data, &native); err != nil {
		return ChatResponse{}, fmt.Errorf("parse response: %w", err)
	}
	if len(native.Candidates) == 0 && native.PromptFeedback.BlockReason != "" {
		return ChatResponse{}, &ProviderError{
			ProviderName: g.Name(),
			Message:      "prompt blocked by gemini: " + native.PromptFeedback.BlockReason,
			Retryable:    false,
		}
	}
	if len(native.Candidates) > 0 && native.Candidates[0].FinishReason == "MALFORMED_FUNCTION_CALL" {
		return ChatResponse{}, &ProviderError{
			ProviderName: g.Name(),
			Message:      "gemini returned a malformed function call",
			Retryable:    false,
		}
	}
	result := translate.FromGemini(native, req.Model)
	result.Model = req.Model
	return result, nil
}

func (g *Gemini) SendStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, <-chan error) {
	ch := make(chan StreamChunk, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		if g.baseErr != nil {
			errCh <- g.baseErr
			return
		}
		modelPath, err := geminiModelPath(req.Model)
		if err != nil {
			errCh <- err
			return
		}
		nativeRequest, err := translate.ToGeminiStrict(req)
		if err != nil {
			errCh <- err
			return
		}
		body, err := marshalJSON(nativeRequest)
		if err != nil {
			errCh <- fmt.Errorf("marshal request: %w", err)
			return
		}
		endpoint := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", g.baseURL, modelPath)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			errCh <- fmt.Errorf("build request: %w", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("x-goog-api-key", g.apiKey)

		resp, err := g.streamClient.Do(httpReq)
		if err != nil {
			errCh <- &ProviderError{ProviderName: g.Name(), Message: err.Error(), Retryable: true}
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			data, _ := readProviderResponse(resp.Body)
			errCh <- &ProviderError{
				ProviderName: g.Name(),
				StatusCode:   resp.StatusCode,
				Message:      geminiErrorMessage(data),
				Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
				RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
			}
			return
		}

		created := time.Now().Unix()
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		var lastUsage *Usage
		toolCallCount := 0
		sawToolCalls := false
		sawFinish := false

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
			if !includeUsage || lastUsage == nil {
				return true
			}
			usage := *lastUsage
			lastUsage = nil
			return emit(StreamChunk{
				ID:      "chatcmpl-gemini",
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   req.Model,
				Choices: []StreamChoice{},
				Usage:   &usage,
			})
		}

		reader := newSSEReader(resp.Body)
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
					errCh <- sseEventTooLargeError(g.Name())
					return
				}
				errCh <- &ProviderError{ProviderName: g.Name(), Message: err.Error(), Retryable: false}
				return
			}
			payload := strings.TrimSpace(event.Data)
			if payload == "" {
				continue
			}
			if providerErr := providerStreamError(g.Name(), []byte(payload)); providerErr != nil {
				errCh <- providerErr
				return
			}
			var native geminiResponse
			if err := json.Unmarshal([]byte(payload), &native); err != nil {
				errCh <- &ProviderError{
					ProviderName: g.Name(),
					Message:      "invalid upstream stream chunk: " + err.Error(),
					Retryable:    false,
				}
				return
			}
			if usage := geminiUsage(native.UsageMetadata); usage != nil {
				lastUsage = usage
			}
			if len(native.Candidates) == 0 {
				if native.PromptFeedback.BlockReason != "" {
					errCh <- &ProviderError{
						ProviderName: g.Name(),
						Message:      "prompt blocked by gemini: " + native.PromptFeedback.BlockReason,
						Retryable:    false,
					}
					return
				}
				continue
			}
			candidate := native.Candidates[0]
			if candidate.FinishReason == "MALFORMED_FUNCTION_CALL" {
				errCh <- &ProviderError{
					ProviderName: g.Name(),
					Message:      "gemini returned a malformed function call",
					Retryable:    false,
				}
				return
			}
			if sawFinish {
				continue
			}

			text := ""
			var toolCalls []ToolCall
			for _, part := range candidate.Content.Parts {
				text += part.Text
				if part.FunctionCall != nil {
					arguments := "{}"
					if part.FunctionCall.Args != nil {
						data, err := json.Marshal(part.FunctionCall.Args)
						if err != nil {
							errCh <- &ProviderError{ProviderName: g.Name(), Message: "marshal tool arguments: " + err.Error(), Retryable: false}
							return
						}
						arguments = string(data)
					}
					toolCalls = append(toolCalls, ToolCall{
						Index: toolCallCount,
						ID:    fmt.Sprintf("call_%d", toolCallCount),
						Type:  "function",
						Function: ToolCallFunction{
							Name:      part.FunctionCall.Name,
							Arguments: arguments,
						},
					})
					toolCallCount++
					sawToolCalls = true
				}
			}

			if text == "" && len(toolCalls) == 0 && candidate.FinishReason == "" {
				continue
			}
			chunk := StreamChunk{
				ID:      "chatcmpl-gemini",
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   req.Model,
				Choices: []StreamChoice{{
					Index: 0,
					Delta: ChatMessage{Role: "assistant", Content: text, ToolCalls: toolCalls},
				}},
			}
			if candidate.FinishReason != "" {
				finishReason := translate.GeminiFinishReason(candidate.FinishReason, sawToolCalls)
				chunk.Choices[0].FinishReason = &finishReason
				sawFinish = true
			}
			if !emit(chunk) {
				return
			}
		}

		if !sawFinish {
			errCh <- streamTruncatedError(g.Name())
			return
		}
		_ = emitUsage()
	}()

	return ch, errCh
}

func geminiUsage(metadata translate.GeminiUsageMetadata) *Usage {
	promptTokens := metadata.PromptTokenCount
	completionTokens := metadata.CandidatesTokenCount + metadata.ThoughtsTokenCount
	total := promptTokens + completionTokens
	if total == 0 {
		total = metadata.TotalTokenCount
	}
	if total == 0 {
		return nil
	}
	usage := &Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      total,
	}
	if metadata.ThoughtsTokenCount > 0 {
		usage.CompletionTokensDetails = &CompletionTokensDetails{
			ReasoningTokens: metadata.ThoughtsTokenCount,
		}
	}
	return usage
}

func (g *Gemini) HealthCheck(ctx context.Context) error {
	if g.baseErr != nil {
		return g.baseErr
	}
	values := url.Values{}
	values.Set("pageSize", "1")
	endpoint := fmt.Sprintf("%s/models?%s", g.baseURL, values.Encode())

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("unauthorized: invalid API key")
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("rate limited (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
}

func (g *Gemini) Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error) {
	if g.baseErr != nil {
		return EmbeddingResponse{}, g.baseErr
	}
	modelPath, err := geminiModelPath(req.Model)
	if err != nil {
		return EmbeddingResponse{}, err
	}
	inputs := normalizeEmbeddingInput(req.Input)
	if len(inputs) == 0 {
		return EmbeddingResponse{}, fmt.Errorf("input is required")
	}

	var data []EmbeddingData
	if len(inputs) > 1 {
		batched, err := g.embedBatch(ctx, modelPath, req.Model, inputs, req.Dimensions)
		if err != nil {
			return EmbeddingResponse{}, err
		}
		data = batched
	}
	if data == nil {
		data = make([]EmbeddingData, 0, len(inputs))
		for i, input := range inputs {
			embedding, err := g.embedOne(ctx, modelPath, input, req.Dimensions)
			if err != nil {
				return EmbeddingResponse{}, err
			}
			data = append(data, translate.FromGeminiEmbedding(embedding, req.Model, i))
		}
	}

	tokens := estimateEmbeddingTokens(inputs)
	return EmbeddingResponse{
		Object: "list",
		Data:   data,
		Model:  req.Model,
		Usage: EmbeddingUsage{
			PromptTokens: tokens,
			TotalTokens:  tokens,
			Estimated:    true,
		},
	}, nil
}

func (g *Gemini) embedBatch(ctx context.Context, modelPath, model string, inputs []string, dimensions *int) ([]EmbeddingData, error) {
	batch := translate.GeminiBatchEmbeddingRequest{
		Requests: make([]translate.GeminiEmbeddingRequest, 0, len(inputs)),
	}
	for _, input := range inputs {
		batch.Requests = append(batch.Requests, translate.ToGeminiEmbeddingWithOptions(input, model, dimensions))
	}
	body, err := marshalJSON(batch)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/models/%s:batchEmbedContents", g.baseURL, modelPath)
	resp, err := g.postEmbedding(ctx, endpoint, body)
	if err != nil {
		return nil, err
	}
	var parsed translate.GeminiBatchEmbeddingResponse
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Embeddings) != len(inputs) {
		// Older Gemini-compatible endpoints only implement embedContent, and a
		// partial batch response is equally unusable.
		return nil, nil
	}
	data := make([]EmbeddingData, 0, len(parsed.Embeddings))
	for i, embedding := range parsed.Embeddings {
		data = append(data, EmbeddingData{Object: "embedding", Index: i, Embedding: embedding.Values})
	}
	return data, nil
}

func (g *Gemini) embedOne(ctx context.Context, modelPath, input string, dimensions *int) (translate.GeminiEmbeddingResponse, error) {
	request := translate.ToGeminiEmbedding(input)
	request.OutputDimensionality = dimensions
	body, err := marshalJSON(request)
	if err != nil {
		return translate.GeminiEmbeddingResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/models/%s:embedContent", g.baseURL, modelPath)
	resp, err := g.postEmbedding(ctx, endpoint, body)
	if err != nil {
		return translate.GeminiEmbeddingResponse{}, err
	}
	var parsed translate.GeminiEmbeddingResponse
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return translate.GeminiEmbeddingResponse{}, fmt.Errorf("parse response: %w", err)
	}
	return parsed, nil
}

func (g *Gemini) postEmbedding(ctx context.Context, endpoint string, body []byte) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", g.apiKey)
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, &ProviderError{ProviderName: g.Name(), Message: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	data, err := readProviderResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &ProviderError{
			ProviderName: g.Name(),
			StatusCode:   resp.StatusCode,
			Message:      geminiErrorMessage(data),
			Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	return data, nil
}

func estimateEmbeddingTokens(inputs []string) int {
	tokens := 0
	for _, input := range inputs {
		tokens += len(input) / 4
	}
	if tokens == 0 {
		return len(inputs)
	}
	return tokens
}

// DiscoverModels fetches available models from Gemini /v1beta/models, following
// nextPageToken so large catalogs are not truncated.
func (g *Gemini) DiscoverModels(ctx context.Context) ([]string, error) {
	if g.baseErr != nil {
		return nil, g.baseErr
	}
	var models []string
	pageToken := ""
	for page := 0; page < 100; page++ {
		values := url.Values{}
		values.Set("pageSize", "1000")
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}
		endpoint := fmt.Sprintf("%s/models?%s", g.baseURL, values.Encode())
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("x-goog-api-key", g.apiKey)
		resp, err := g.client.Do(httpReq)
		if err != nil {
			return nil, err
		}
		var body struct {
			Models []struct {
				Name string `json:"name"` // e.g. "models/gemini-2.5-flash"
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
		if decodeErr != nil {
			return nil, decodeErr
		}
		for _, m := range body.Models {
			name := strings.TrimPrefix(m.Name, "models/")
			models = append(models, name)
		}
		if body.NextPageToken == "" {
			break
		}
		pageToken = body.NextPageToken
	}
	return models, nil
}

func normalizeEmbeddingInput(input any) []string {
	switch v := input.(type) {
	case string:
		return []string{v}
	case []string:
		return v
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// translateToGemini retained for backward compat (delegates to translate package).
func translateToGemini(req ChatRequest) geminiRequest { return translate.ToGemini(req) }

// translateFromGemini retained for backward compat (delegates to translate package).
func translateFromGemini(resp geminiResponse, model string) ChatResponse {
	return translate.FromGemini(resp, model)
}

func geminiErrorMessage(body []byte) string {
	var errResp geminiErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
		return truncateProviderError(errResp.Error.Message)
	}
	return providerErrorMessage(body)
}
