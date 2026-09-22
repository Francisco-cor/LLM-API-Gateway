package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

func TestAnthropicStreamEmitsUsageWhenRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"text\":\"hello\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {}\n\n"))
	}))
	defer srv.Close()

	p := provider.NewAnthropic("test-key", srv.URL, time.Second, []string{"claude-test"})
	chunks, errs := p.SendStream(context.Background(), provider.ChatRequest{
		Model:         "claude-test",
		Messages:      []provider.ChatMessage{{Role: "user", Content: "hello"}},
		StreamOptions: &provider.StreamOptions{IncludeUsage: true},
	})
	var got []provider.StreamChunk
	for chunk := range chunks {
		got = append(got, chunk)
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 3 {
		t.Fatalf("chunks = %d, want content, finish, usage", len(got))
	}
	if got[2].Usage == nil || got[2].Usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v, want total 5", got[2].Usage)
	}
}

func TestGeminiStreamEmitsUsageWhenRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n"))
	}))
	defer srv.Close()

	p := provider.NewGemini("test-key", srv.URL, time.Second, []string{"gemini-test"})
	chunks, errs := p.SendStream(context.Background(), provider.ChatRequest{
		Model:         "gemini-test",
		Messages:      []provider.ChatMessage{{Role: "user", Content: "hello"}},
		StreamOptions: &provider.StreamOptions{IncludeUsage: true},
	})
	var got []provider.StreamChunk
	for chunk := range chunks {
		got = append(got, chunk)
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 3 {
		t.Fatalf("chunks = %d, want content, finish, usage", len(got))
	}
	if got[2].Usage == nil || got[2].Usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v, want total 5", got[2].Usage)
	}
}

func TestAnthropicStreamPreservesToolCallDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-1\",\"name\":\"lookup\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"x\\\"}\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {}\n\n"))
	}))
	defer srv.Close()

	p := provider.NewAnthropic("test-key", srv.URL, time.Second, []string{"claude-test"})
	chunks, errs := p.SendStream(context.Background(), provider.ChatRequest{
		Model:    "claude-test",
		Messages: []provider.ChatMessage{{Role: "user", Content: "find x"}},
	})
	var got []provider.StreamChunk
	for chunk := range chunks {
		got = append(got, chunk)
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 4 {
		t.Fatalf("chunks = %d, want tool id, two args, finish", len(got))
	}
	if got[0].Choices[0].Delta.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("tool start = %+v", got[0])
	}
	if got[1].Choices[0].Delta.ToolCalls[0].Function.Arguments != `{"q":` || got[2].Choices[0].Delta.ToolCalls[0].Function.Arguments != `x"}` {
		t.Fatalf("tool argument deltas = %+v, %+v", got[1], got[2])
	}
}
