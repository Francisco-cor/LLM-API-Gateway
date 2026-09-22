package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatMessageSupportsTextAndMultimodalContent(t *testing.T) {
	var req ChatRequest
	body := `{"model":"gpt-4o","messages":[{"role":"user","name":"Ada","content":[{"type":"text","text":"Describe this"},{"type":"image_url","image_url":{"url":"https://example.test/image.png","detail":"high"}}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}],"max_completion_tokens":32,"parallel_tool_calls":true}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	message := req.Messages[0]
	if message.Content != "" || len(message.ContentParts) != 2 || message.Text() != "Describe this" {
		t.Fatalf("unexpected message: %+v", message)
	}
	if message.ContentParts[1].ImageURL == nil || message.ContentParts[1].ImageURL.Detail != "high" {
		t.Fatalf("image part not preserved: %+v", message.ContentParts[1])
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Arguments != `{"q":"x"}` {
		t.Fatalf("tool call not preserved: %+v", message.ToolCalls)
	}
	if req.EffectiveMaxTokens() == nil || *req.EffectiveMaxTokens() != 32 || req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
		t.Fatalf("request options not preserved: %+v", req)
	}

	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	encodedText := string(encoded)
	if !strings.Contains(encodedText, `"content":[`) || !strings.Contains(encodedText, `"tool_calls"`) {
		t.Fatalf("multimodal request was not preserved: %s", encodedText)
	}
}

func TestChatMessageRejectsUnknownFields(t *testing.T) {
	var message ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hi","unknown":true}`), &message); err == nil {
		t.Fatal("unknown chat message field was accepted")
	}
}
