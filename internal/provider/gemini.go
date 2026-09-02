package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

func (g *Gemini) SetModels(models []string) { g.models = models }

// geminiErrorResponse is the native Gemini error body.
type geminiErrorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (g *Gemini) Send(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body, err := json.Marshal(translate.ToGemini(req))
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
			RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var native geminiResponse
	if err := json.Unmarshal(data, &native); err != nil {
		return ChatResponse{}, fmt.Errorf("parse response: %w", err)
	}
	return translate.FromGemini(native, req.Model), nil
}

func (g *Gemini) SendStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, <-chan error) {
	ch := make(chan StreamChunk, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		body, err := json.Marshal(translate.ToGemini(req))
		if err != nil {
			errCh <- fmt.Errorf("marshal request: %w", err)
			return
		}
		endpoint := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse&key=%s", g.baseURL, req.Model, url.QueryEscape(g.apiKey))
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			errCh <- fmt.Errorf("build request: %w", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := g.client.Do(httpReq)
		if err != nil {
			errCh <- &ProviderError{ProviderName: g.Name(), Message: err.Error(), Retryable: true}
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			errCh <- &ProviderError{
				ProviderName: g.Name(),
				StatusCode:   resp.StatusCode,
				Message:      geminiErrorMessage(data),
				Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
				RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
			}
			return
		}

		scanner := bufio.NewScanner(resp.Body)
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
			if payload == "" {
				continue
			}
			var native geminiResponse
			if err := json.Unmarshal([]byte(payload), &native); err != nil {
				continue
			}
			if len(native.Candidates) == 0 || len(native.Candidates[0].Content.Parts) == 0 {
				continue
			}
			text := native.Candidates[0].Content.Parts[0].Text
			chunk := StreamChunk{
				ID:      "chatcmpl-gemini",
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   req.Model,
				Choices: []StreamChoice{{
					Index: 0,
					Delta: ChatMessage{Role: "assistant", Content: text},
				}},
			}
			// Map finishReason if present
			if native.Candidates[0].FinishReason == "STOP" || native.Candidates[0].FinishReason == "MAX_TOKENS" {
				fr := "stop"
				if native.Candidates[0].FinishReason == "MAX_TOKENS" {
					fr = "length"
				}
				chunk.Choices[0].FinishReason = &fr
			}
			select {
			case ch <- chunk:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
			if chunk.Choices[0].FinishReason != nil {
				return
			}
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			errCh <- err
		}
	}()

	return ch, errCh
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

func (g *Gemini) Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error) {
	inputs := normalizeEmbeddingInput(req.Input)
	if len(inputs) == 0 {
		return EmbeddingResponse{}, fmt.Errorf("input is required")
	}
	var data []EmbeddingData
	for i, input := range inputs {
		gemReq := translate.ToGeminiEmbedding(input)
		body, err := json.Marshal(gemReq)
		if err != nil {
			return EmbeddingResponse{}, fmt.Errorf("marshal request: %w", err)
		}
		endpoint := fmt.Sprintf("%s/models/%s:embedContent?key=%s", g.baseURL, req.Model, url.QueryEscape(g.apiKey))
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return EmbeddingResponse{}, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := g.client.Do(httpReq)
		if err != nil {
			return EmbeddingResponse{}, &ProviderError{ProviderName: g.Name(), Message: err.Error(), Retryable: true}
		}
		dataBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return EmbeddingResponse{}, fmt.Errorf("read response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return EmbeddingResponse{}, &ProviderError{
				ProviderName: g.Name(),
				StatusCode:   resp.StatusCode,
				Message:      geminiErrorMessage(dataBytes),
				Retryable:    resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
				RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After")),
			}
		}
		var gemResp translate.GeminiEmbeddingResponse
		if err := json.Unmarshal(dataBytes, &gemResp); err != nil {
			return EmbeddingResponse{}, fmt.Errorf("parse response: %w", err)
		}
		data = append(data, translate.FromGeminiEmbedding(gemResp, req.Model, i))
	}
	// estimate usage: ~ len(input)/4 tokens
	tokens := 0
	for _, s := range inputs {
		tokens += len(s) / 4
	}
	return EmbeddingResponse{
		Object: "list",
		Data:   data,
		Model:  req.Model,
		Usage: EmbeddingUsage{
			PromptTokens: tokens,
			TotalTokens:  tokens,
		},
	}, nil
}

// DiscoverModels fetches available models from Gemini /v1beta/models
func (g *Gemini) DiscoverModels(ctx context.Context) ([]string, error) {
	endpoint := fmt.Sprintf("%s/models?key=%s", g.baseURL, url.QueryEscape(g.apiKey))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var body struct {
		Models []struct {
			Name string `json:"name"` // e.g. "models/gemini-2.5-flash"
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var models []string
	for _, m := range body.Models {
		name := m.Name
		if strings.HasPrefix(name, "models/") {
			name = strings.TrimPrefix(name, "models/")
		}
		models = append(models, name)
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
		// try to handle json.RawMessage or fallback
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
		return errResp.Error.Message
	}
	return string(body)
}
