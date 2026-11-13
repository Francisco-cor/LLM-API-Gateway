package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type capturedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

func captureProviderRequest(t *testing.T, respond func(http.ResponseWriter, *http.Request)) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	requests := make(chan capturedRequest, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- capturedRequest{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			header: r.Header.Clone(),
			body:   body,
		}
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, requests
}

func collectStream(t *testing.T, chunks <-chan StreamChunk, errs <-chan error) ([]StreamChunk, error) {
	t.Helper()
	var got []StreamChunk
	for chunk := range chunks {
		got = append(got, chunk)
	}
	var streamErr error
	for err := range errs {
		if err != nil && streamErr == nil {
			streamErr = err
		}
	}
	return got, streamErr
}

func writeSSE(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(body))
}

func TestOpenAIStreamTerminalEvents(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantChunks int
		wantErr    string
	}{
		{
			name:       "done terminates cleanly",
			body:       "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			wantChunks: 1,
		},
		{
			name:       "eof without done is truncated",
			body:       "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
			wantChunks: 1,
			wantErr:    ErrStreamTruncated.Error(),
		},
		{
			name:    "inline error is surfaced",
			body:    "data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\",\"code\":500}}\n\n",
			wantErr: "boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, requests := captureProviderRequest(t, func(w http.ResponseWriter, _ *http.Request) {
				writeSSE(w, tc.body)
			})
			p := NewOpenAI("secret", srv.URL, time.Second, nil)
			chunks, errs := p.SendStream(context.Background(), ChatRequest{
				Model:    "gpt-test",
				Messages: []ChatMessage{{Role: "user", Content: "hi"}},
			})
			got, err := collectStream(t, chunks, errs)
			if len(got) != tc.wantChunks {
				t.Fatalf("chunks = %d, want %d", len(got), tc.wantChunks)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if tc.wantErr == ErrStreamTruncated.Error() && IsRetryable(err) {
					t.Fatal("truncated stream must not be retryable")
				}
			}

			captured := <-requests
			if captured.method != http.MethodPost || captured.path != "/chat/completions" {
				t.Errorf("request = %s %s, want POST /chat/completions", captured.method, captured.path)
			}
			if captured.header.Get("Authorization") != "Bearer secret" {
				t.Errorf("Authorization = %q", captured.header.Get("Authorization"))
			}
			if captured.header.Get("Accept") != "text/event-stream" {
				t.Errorf("Accept = %q", captured.header.Get("Accept"))
			}
			var body map[string]any
			if err := json.Unmarshal(captured.body, &body); err != nil {
				t.Fatalf("request body: %v", err)
			}
			if body["stream"] != true || body["model"] != "gpt-test" {
				t.Errorf("request body = %s", captured.body)
			}
		})
	}
}

func TestSSEReaderBoundsEventsWithoutBreakingLargePayloads(t *testing.T) {
	large := strings.Repeat("x", 2<<20)
	reader := newSSEReader(strings.NewReader("data: " + large + "\n\n"))
	event, err := reader.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(event.Data) != len(large) {
		t.Fatalf("data length = %d, want %d", len(event.Data), len(large))
	}

	oversized := strings.Repeat("y", maxSSEEventSize+1)
	reader = newSSEReader(strings.NewReader("data: " + oversized + "\n\n"))
	_, err = reader.Next()
	if !errors.Is(err, errSSEEventTooLarge) {
		t.Fatalf("error = %v, want errSSEEventTooLarge", err)
	}
}

func TestAnthropicStreamFixtures(t *testing.T) {
	fixture := strings.Join([]string{
		"event: ping\ndata: {\"type\":\"ping\"}\n\n",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":10,\"cache_creation_input_tokens\":5,\"cache_read_input_tokens\":3}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"x\\\"}\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, "")

	srv, requests := captureProviderRequest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w, fixture)
	})
	p := NewAnthropic("secret", srv.URL, time.Second, nil)
	chunks, errs := p.SendStream(context.Background(), ChatRequest{
		Model: "claude-test",
		Messages: []ChatMessage{
			{Role: "system", Content: "first"},
			{Role: "developer", Content: "second"},
			{Role: "user", Content: "find x"},
		},
		StreamOptions: &StreamOptions{IncludeUsage: true},
	})
	got, err := collectStream(t, chunks, errs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("chunks = %d, want 5 (tool start, two args, finish, usage)", len(got))
	}
	start := got[0].Choices[0].Delta.ToolCalls[0]
	if start.Index != 1 || start.ID != "toolu_1" || start.Function.Name != "lookup" {
		t.Errorf("tool start = %+v", start)
	}
	for _, index := range []int{1, 2} {
		call := got[index].Choices[0].Delta.ToolCalls[0]
		if call.Index != 1 || call.ID != "" || call.Function.Name != "" {
			t.Errorf("tool delta %d = %+v, want only index/arguments", index, call)
		}
	}
	if got[1].Choices[0].Delta.ToolCalls[0].Function.Arguments != `{"q":` {
		t.Errorf("first argument delta = %q", got[1].Choices[0].Delta.ToolCalls[0].Function.Arguments)
	}
	if got[3].Choices[0].FinishReason == nil || *got[3].Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish reason = %v, want tool_calls", got[3].Choices[0].FinishReason)
	}
	usage := got[4].Usage
	if usage == nil || usage.PromptTokens != 18 || usage.CompletionTokens != 7 || usage.TotalTokens != 25 {
		t.Fatalf("usage = %+v, want prompt 18 completion 7 total 25", usage)
	}
	if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 3 {
		t.Errorf("cached tokens = %+v", usage.PromptTokensDetails)
	}
	if got[4].Choices == nil || len(got[4].Choices) != 0 {
		t.Errorf("usage chunk choices = %#v, want empty slice", got[4].Choices)
	}
	if got[4].Model != "claude-test" || got[4].Created == 0 {
		t.Errorf("usage chunk metadata = %+v", got[4])
	}

	captured := <-requests
	if captured.method != http.MethodPost || captured.path != "/v1/messages" {
		t.Errorf("request = %s %s", captured.method, captured.path)
	}
	if captured.header.Get("x-api-key") != "secret" || captured.header.Get("anthropic-version") == "" {
		t.Errorf("headers = %v", captured.header)
	}
	var body struct {
		Stream   bool   `json:"stream"`
		System   string `json:"system"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured.body, &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if !body.Stream || body.System != "first\n\nsecond" || len(body.Messages) != 1 {
		t.Errorf("request body = %s", captured.body)
	}
}

func TestAnthropicStreamErrorsAndTruncation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "truncated without message_stop",
			body:    "event: content_block_delta\ndata: {\"delta\":{\"text\":\"hi\"}}\n\n",
			wantErr: ErrStreamTruncated.Error(),
		},
		{
			name:    "error event",
			body:    "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n",
			wantErr: "busy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := captureProviderRequest(t, func(w http.ResponseWriter, _ *http.Request) {
				writeSSE(w, tc.body)
			})
			p := NewAnthropic("secret", srv.URL, time.Second, nil)
			chunks, errs := p.SendStream(context.Background(), ChatRequest{
				Model:    "claude-test",
				Messages: []ChatMessage{{Role: "user", Content: "hi"}},
			})
			_, err := collectStream(t, chunks, errs)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestAnthropicStreamStopReasonMapping(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{reason: "end_turn", want: "stop"},
		{reason: "stop_sequence", want: "stop"},
		{reason: "max_tokens", want: "length"},
		{reason: "tool_use", want: "tool_calls"},
		{reason: "refusal", want: "content_filter"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			body := "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"" + tc.reason + "\"}}\n\n" +
				"event: message_stop\ndata: {}\n\n"
			srv, _ := captureProviderRequest(t, func(w http.ResponseWriter, _ *http.Request) {
				writeSSE(w, body)
			})
			p := NewAnthropic("secret", srv.URL, time.Second, nil)
			chunks, errs := p.SendStream(context.Background(), ChatRequest{
				Model:    "claude-test",
				Messages: []ChatMessage{{Role: "user", Content: "hi"}},
			})
			got, err := collectStream(t, chunks, errs)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 || got[0].Choices[0].FinishReason == nil || *got[0].Choices[0].FinishReason != tc.want {
				t.Fatalf("chunks = %+v, want finish %q", got, tc.want)
			}
		})
	}
}

func TestGeminiStreamFixtures(t *testing.T) {
	fixture := "" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\"}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"thoughtsTokenCount\":4,\"totalTokenCount\":9}}\n\n"

	srv, requests := captureProviderRequest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w, fixture)
	})
	p := NewGemini("secret", srv.URL, time.Second, nil)
	chunks, errs := p.SendStream(context.Background(), ChatRequest{
		Model:         "gemini-test",
		Messages:      []ChatMessage{{Role: "user", Content: "hi"}},
		StreamOptions: &StreamOptions{IncludeUsage: true},
	})
	got, err := collectStream(t, chunks, errs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("chunks = %d, want content, finish, usage", len(got))
	}
	if got[0].Choices[0].Delta.Content != "hi" {
		t.Errorf("content = %q", got[0].Choices[0].Delta.Content)
	}
	call := got[1].Choices[0].Delta.ToolCalls[0]
	if call.ID != "call_0" || call.Index != 0 || call.Function.Name != "lookup" || call.Function.Arguments != "{}" {
		t.Errorf("tool call = %+v", call)
	}
	if got[1].Choices[0].FinishReason == nil || *got[1].Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish reason = %v", got[1].Choices[0].FinishReason)
	}
	usage := got[2].Usage
	if usage == nil || usage.PromptTokens != 3 || usage.CompletionTokens != 6 || usage.TotalTokens != 9 {
		t.Fatalf("usage = %+v, want prompt 3 completion 6 total 9", usage)
	}
	if usage.CompletionTokensDetails == nil || usage.CompletionTokensDetails.ReasoningTokens != 4 {
		t.Errorf("reasoning tokens = %+v", usage.CompletionTokensDetails)
	}
	if got[2].Choices == nil {
		t.Error("usage chunk choices must be an empty slice, not null")
	}

	captured := <-requests
	if captured.method != http.MethodPost || captured.path != "/models/gemini-test:streamGenerateContent" {
		t.Errorf("request = %s %s", captured.method, captured.path)
	}
	if captured.query != "alt=sse" {
		t.Errorf("query = %q, want alt=sse", captured.query)
	}
	if captured.header.Get("x-goog-api-key") != "secret" {
		t.Errorf("x-goog-api-key = %q", captured.header.Get("x-goog-api-key"))
	}
}

func TestGeminiStreamSafetyErrorsAndTruncation(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantErr    string
		wantFinish string
	}{
		{
			name:       "safety finish reason maps to content_filter",
			body:       "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"blocked\"}]},\"finishReason\":\"SAFETY\"}]}\n\n",
			wantFinish: "content_filter",
		},
		{
			name:    "inline error payload",
			body:    "data: {\"error\":{\"code\":500,\"message\":\"backend down\",\"status\":\"INTERNAL\"}}\n\n",
			wantErr: "backend down",
		},
		{
			name:    "prompt feedback block",
			body:    "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n",
			wantErr: "SAFETY",
		},
		{
			name:    "truncated without finishReason",
			body:    "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n",
			wantErr: ErrStreamTruncated.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := captureProviderRequest(t, func(w http.ResponseWriter, _ *http.Request) {
				writeSSE(w, tc.body)
			})
			p := NewGemini("secret", srv.URL, time.Second, nil)
			chunks, errs := p.SendStream(context.Background(), ChatRequest{
				Model:    "gemini-test",
				Messages: []ChatMessage{{Role: "user", Content: "hi"}},
			})
			got, err := collectStream(t, chunks, errs)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 || got[0].Choices[0].FinishReason == nil || *got[0].Choices[0].FinishReason != tc.wantFinish {
				t.Fatalf("chunks = %+v, want finish %q", got, tc.wantFinish)
			}
		})
	}
}

func TestGeminiRejectsUnsafeModelPath(t *testing.T) {
	requests := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests <- struct{}{}
	}))
	defer srv.Close()

	p := NewGemini("secret", srv.URL, time.Second, nil)
	for _, model := range []string{"models/gemini", "gemini?x=1", "gemini#frag", "gemini:alternate"} {
		_, err := p.Send(context.Background(), ChatRequest{Model: model, Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
		if err == nil || !strings.Contains(err.Error(), "invalid gemini model") {
			t.Errorf("model %q error = %v", model, err)
		}
	}
	select {
	case <-requests:
		t.Fatal("unsafe model names must not reach the network")
	default:
	}
}

func TestAnthropicHealthCheckDoesNotPostMessages(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantErr    bool
		wantMethod string
	}{
		{name: "models ok", status: http.StatusOK, wantMethod: http.MethodGet},
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: true, wantMethod: http.MethodGet},
		{name: "rate limited", status: http.StatusTooManyRequests, wantErr: true, wantMethod: http.MethodGet},
		{name: "server error", status: http.StatusServiceUnavailable, wantErr: true, wantMethod: http.MethodGet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, requests := captureProviderRequest(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})
			p := NewAnthropic("secret", srv.URL, time.Second, nil)
			err := p.HealthCheck(context.Background())
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			captured := <-requests
			if captured.method != tc.wantMethod || captured.path != "/v1/models" {
				t.Errorf("request = %s %s, want GET /v1/models", captured.method, captured.path)
			}
			select {
			case <-requests:
				t.Fatal("health check must not send a second (billable) request")
			default:
			}
		})
	}
}

func TestAnthropicHealthCheckFallsBackToHead(t *testing.T) {
	srv, requests := captureProviderRequest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	p := NewAnthropic("secret", srv.URL, time.Second, nil)
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	first := <-requests
	second := <-requests
	if first.path != "/v1/models" || second.method != http.MethodHead || second.path != "/v1/messages" {
		t.Errorf("requests = %+v %+v", first, second)
	}
}

func TestGeminiEmbeddingsUseBatchAndDimensions(t *testing.T) {
	srv, requests := captureProviderRequest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]}]}`))
	})
	dimensions := 256
	p := NewGemini("secret", srv.URL, time.Second, nil)
	resp, err := p.Embed(context.Background(), EmbeddingRequest{
		Model:      "text-embedding-004",
		Input:      []string{"alpha", "beta"},
		Dimensions: &dimensions,
	})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if len(resp.Data) != 2 || resp.Data[1].Index != 1 || len(resp.Data[1].Embedding) != 2 {
		t.Fatalf("data = %+v", resp.Data)
	}
	if !resp.Usage.Estimated {
		t.Error("gemini embedding usage must be marked as estimated")
	}

	captured := <-requests
	if captured.method != http.MethodPost || captured.path != "/models/text-embedding-004:batchEmbedContents" {
		t.Errorf("request = %s %s", captured.method, captured.path)
	}
	var body struct {
		Requests []struct {
			Model                string `json:"model"`
			OutputDimensionality *int   `json:"outputDimensionality"`
			Content              struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(captured.body, &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if len(body.Requests) != 2 {
		t.Fatalf("batch requests = %d, want 2", len(body.Requests))
	}
	for _, request := range body.Requests {
		if request.Model != "models/text-embedding-004" || request.OutputDimensionality == nil || *request.OutputDimensionality != 256 {
			t.Errorf("batch request = %+v", request)
		}
	}
}

func TestGeminiDiscoverModelsPaginates(t *testing.T) {
	srv, requests := captureProviderRequest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-a"}],"nextPageToken":"page-2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-b"}]}`))
	})
	p := NewGemini("secret", srv.URL, time.Second, nil)
	models, err := p.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("DiscoverModels failed: %v", err)
	}
	if len(models) != 2 || models[0] != "gemini-a" || models[1] != "gemini-b" {
		t.Fatalf("models = %v", models)
	}
	first := <-requests
	second := <-requests
	if first.query == "" || !strings.Contains(second.query, "pageToken=page-2") {
		t.Errorf("queries = %q %q", first.query, second.query)
	}
}
