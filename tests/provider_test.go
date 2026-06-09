package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// TestAnthropicTranslation verifies that an OpenAI-format ChatRequest is
// translated into Anthropic's Messages API shape: system messages move to
// the top-level "system" field and are excluded from "messages".
func TestAnthropicTranslation(t *testing.T) {
	cases := []struct {
		name         string
		messages     []provider.ChatMessage
		wantSystem   string
		wantMsgCount int
		wantFirstMsg string
	}{
		{
			name: "system message is extracted",
			messages: []provider.ChatMessage{
				{Role: "system", Content: "You are a helpful assistant"},
				{Role: "user", Content: "Hello"},
			},
			wantSystem:   "You are a helpful assistant",
			wantMsgCount: 1,
			wantFirstMsg: "user",
		},
		{
			name: "no system message leaves system field empty",
			messages: []provider.ChatMessage{
				{Role: "user", Content: "Hello"},
				{Role: "assistant", Content: "Hi there"},
				{Role: "user", Content: "How are you?"},
			},
			wantSystem:   "",
			wantMsgCount: 3,
			wantFirstMsg: "user",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured struct {
				MaxTokens int    `json:"max_tokens"`
				System    string `json:"system"`
				Messages  []struct {
					Role string `json:"role"`
				} `json:"messages"`
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&captured)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":          "msg_test",
					"type":        "message",
					"role":        "assistant",
					"model":       "claude-sonnet-4-6",
					"content":     []map[string]string{{"type": "text", "text": "ok"}},
					"stop_reason": "end_turn",
					"usage":       map[string]int{"input_tokens": 5, "output_tokens": 2},
				})
			}))
			defer srv.Close()

			p := provider.NewAnthropic("test-key", srv.URL, 5*time.Second, []string{"claude-sonnet-4-6"})
			_, err := p.Send(context.Background(), provider.ChatRequest{
				Model:    "claude-sonnet-4-6",
				Messages: tc.messages,
			})
			if err != nil {
				t.Fatalf("Send failed: %v", err)
			}

			if captured.System != tc.wantSystem {
				t.Errorf("system field: got %q, want %q", captured.System, tc.wantSystem)
			}
			if len(captured.Messages) != tc.wantMsgCount {
				t.Errorf("message count: got %d, want %d", len(captured.Messages), tc.wantMsgCount)
			}
			if captured.MaxTokens == 0 {
				t.Error("max_tokens should default to a non-zero value")
			}
			if len(captured.Messages) > 0 && captured.Messages[0].Role != tc.wantFirstMsg {
				t.Errorf("first message role: got %q, want %q", captured.Messages[0].Role, tc.wantFirstMsg)
			}
		})
	}
}

// TestAnthropicResponseTranslation verifies that an Anthropic Messages API
// response is translated back into the OpenAI-compatible ChatResponse shape.
func TestAnthropicResponseTranslation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_123",
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-sonnet-4-6",
			"content":     []map[string]string{{"type": "text", "text": "Hello back"}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 10, "output_tokens": 4},
		})
	}))
	defer srv.Close()

	p := provider.NewAnthropic("test-key", srv.URL, 5*time.Second, []string{"claude-sonnet-4-6"})
	resp, err := p.Send(context.Background(), provider.ChatRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []provider.ChatMessage{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if len(resp.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hello back" {
		t.Errorf("got content %q, want %q", resp.Choices[0].Message.Content, "Hello back")
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Errorf("got role %q, want %q", resp.Choices[0].Message.Role, "assistant")
	}
	if resp.Usage.TotalTokens != 14 {
		t.Errorf("got total_tokens %d, want 14", resp.Usage.TotalTokens)
	}
}

// TestGeminiTranslation verifies that an OpenAI-format ChatRequest is
// translated into Gemini's contents/systemInstruction shape, with the
// "assistant" role remapped to "model".
func TestGeminiTranslation(t *testing.T) {
	var captured struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		SystemInstruction *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{
					"content":      map[string]any{"role": "model", "parts": []map[string]string{{"text": "ok"}}},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]int{
				"promptTokenCount":     3,
				"candidatesTokenCount": 1,
				"totalTokenCount":      4,
			},
		})
	}))
	defer srv.Close()

	p := provider.NewGemini("test-key", srv.URL, 5*time.Second, []string{"gemini-2.5-flash"})
	resp, err := p.Send(context.Background(), provider.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []provider.ChatMessage{
			{Role: "system", Content: "Be concise"},
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi"},
			{Role: "user", Content: "How are you?"},
		},
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if captured.SystemInstruction == nil {
		t.Fatal("expected systemInstruction to be set")
	}
	if captured.SystemInstruction.Parts[0].Text != "Be concise" {
		t.Errorf("systemInstruction text: got %q, want %q", captured.SystemInstruction.Parts[0].Text, "Be concise")
	}
	if len(captured.Contents) != 3 {
		t.Fatalf("got %d contents, want 3 (system message excluded)", len(captured.Contents))
	}
	if captured.Contents[1].Role != "model" {
		t.Errorf("assistant role: got %q, want %q", captured.Contents[1].Role, "model")
	}
	if resp.Usage.TotalTokens != 4 {
		t.Errorf("got total_tokens %d, want 4", resp.Usage.TotalTokens)
	}
}

// TestProviderError_Retryable verifies that 429 and 5xx responses are marked
// retryable so the handler can fall back to another provider.
func TestProviderError_Retryable(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		retryable bool
	}{
		{name: "429 is retryable", status: http.StatusTooManyRequests, retryable: true},
		{name: "500 is retryable", status: http.StatusInternalServerError, retryable: true},
		{name: "503 is retryable", status: http.StatusServiceUnavailable, retryable: true},
		{name: "400 is not retryable", status: http.StatusBadRequest, retryable: false},
		{name: "401 is not retryable", status: http.StatusUnauthorized, retryable: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			}))
			defer srv.Close()

			p := provider.NewOpenAI("test-key", srv.URL, 5*time.Second, []string{"gpt-4o"})
			_, err := p.Send(context.Background(), provider.ChatRequest{
				Model:    "gpt-4o",
				Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if got := provider.IsRetryable(err); got != tc.retryable {
				t.Errorf("IsRetryable() = %v, want %v", got, tc.retryable)
			}
		})
	}
}
