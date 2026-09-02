package tests

import (
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/translate"
	"github.com/fcordero/llm-api-gateway/internal/types"
)

func TestTranslate_ToAnthropic(t *testing.T) {
	temp := 0.7
	cases := []struct {
		name       string
		req        types.ChatRequest
		wantSystem string
		wantCount  int
	}{
		{
			name: "system extracted",
			req: types.ChatRequest{
				Model:       "claude-sonnet-4-6",
				Messages:    []types.ChatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}},
				Temperature: &temp,
			},
			wantSystem: "sys",
			wantCount:  1,
		},
		{
			name: "no system",
			req: types.ChatRequest{
				Model:    "claude-sonnet-4-6",
				Messages: []types.ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
			},
			wantSystem: "",
			wantCount:  2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translate.ToAnthropic(tc.req)
			if got.System != tc.wantSystem {
				t.Errorf("system %q want %q", got.System, tc.wantSystem)
			}
			if len(got.Messages) != tc.wantCount {
				t.Errorf("messages %d want %d", len(got.Messages), tc.wantCount)
			}
			if tc.req.Temperature != nil && got.Temperature == nil {
				t.Error("temperature lost")
			}
			// default max tokens when not set
			if tc.req.MaxTokens == nil && got.MaxTokens != translate.DefaultMaxTokens {
				t.Errorf("max_tokens %d want %d (default)", got.MaxTokens, translate.DefaultMaxTokens)
			}
		})
	}
}

func TestTranslate_FromAnthropic(t *testing.T) {
	resp := translate.AnthropicResponse{
		ID:    "msg_1",
		Model: "claude-sonnet-4-6",
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: "hello"}},
		StopReason: "end_turn",
	}
	resp.Usage.InputTokens = 10
	resp.Usage.OutputTokens = 5
	got := translate.FromAnthropic(resp)
	if got.Choices[0].Message.Content != "hello" {
		t.Errorf("content %q want hello", got.Choices[0].Message.Content)
	}
	if got.Usage.TotalTokens != 15 {
		t.Errorf("total %d want 15", got.Usage.TotalTokens)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish %q want stop", got.Choices[0].FinishReason)
	}
	// max_tokens stop reason maps to length
	resp.StopReason = "max_tokens"
	got = translate.FromAnthropic(resp)
	if got.Choices[0].FinishReason != "length" {
		t.Errorf("finish %q want length for max_tokens", got.Choices[0].FinishReason)
	}
	// empty content
	resp.Content = nil
	got = translate.FromAnthropic(resp)
	if got.Choices[0].Message.Content != "" {
		t.Errorf("expected empty content for no content")
	}
}

func TestTranslate_ToGemini(t *testing.T) {
	temp := 0.9
	max := 100
	req := types.ChatRequest{
		Model:       "gemini-2.5-flash",
		Messages:    []types.ChatMessage{{Role: "system", Content: "be concise"}, {Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
		Temperature: &temp,
		MaxTokens:   &max,
	}
	got := translate.ToGemini(req)
	if got.SystemInstruction == nil || got.SystemInstruction.Parts[0].Text != "be concise" {
		t.Fatalf("systemInstruction not set correctly %+v", got.SystemInstruction)
	}
	if len(got.Contents) != 2 {
		t.Fatalf("contents %d want 2 (system excluded)", len(got.Contents))
	}
	if got.Contents[1].Role != "model" {
		t.Errorf("assistant role %q want model", got.Contents[1].Role)
	}
	if got.GenerationConfig.Temperature == nil || *got.GenerationConfig.Temperature != 0.9 {
		t.Error("temperature not preserved")
	}
	if got.GenerationConfig.MaxOutputTokens == nil || *got.GenerationConfig.MaxOutputTokens != 100 {
		t.Error("max_tokens not preserved")
	}
}

func TestTranslate_FromGemini(t *testing.T) {
	resp := translate.GeminiResponse{
		Candidates: []struct {
			Content      translate.GeminiContent `json:"content"`
			FinishReason string                  `json:"finishReason"`
		}{
			{Content: translate.GeminiContent{Parts: []translate.GeminiPart{{Text: "yo"}}}, FinishReason: "STOP"},
		},
	}
	resp.UsageMetadata.PromptTokenCount = 3
	resp.UsageMetadata.CandidatesTokenCount = 2
	resp.UsageMetadata.TotalTokenCount = 5
	got := translate.FromGemini(resp, "gemini-2.5-flash")
	if got.Choices[0].Message.Content != "yo" {
		t.Errorf("content %q want yo", got.Choices[0].Message.Content)
	}
	if got.Usage.TotalTokens != 5 {
		t.Errorf("total %d want 5", got.Usage.TotalTokens)
	}
	// MAX_TOKENS maps to length
	resp.Candidates[0].FinishReason = "MAX_TOKENS"
	got = translate.FromGemini(resp, "gemini-2.5-flash")
	if got.Choices[0].FinishReason != "length" {
		t.Errorf("finish %q want length", got.Choices[0].FinishReason)
	}
	// empty candidates
	empty := translate.GeminiResponse{}
	got = translate.FromGemini(empty, "gemini-2.5-flash")
	if got.Choices[0].Message.Content != "" {
		t.Error("expected empty content for empty candidates")
	}
}

func TestTranslate_GeminiEmbedding(t *testing.T) {
	req := translate.ToGeminiEmbedding("hello world")
	if len(req.Content.Parts) != 1 || req.Content.Parts[0].Text != "hello world" {
		t.Errorf("embedding request parts %+v", req.Content.Parts)
	}
	resp := translate.GeminiEmbeddingResponse{}
	resp.Embedding.Values = []float32{0.1, 0.2}
	data := translate.FromGeminiEmbedding(resp, "text-embedding-004", 0)
	if len(data.Embedding) != 2 || data.Index != 0 || data.Object != "embedding" {
		t.Errorf("embedding data %+v", data)
	}
}

func TestTranslate_NormalizeChat(t *testing.T) {
	req := types.ChatRequest{Model: "gpt-4o", Messages: []types.ChatMessage{{Role: "user", Content: "hi"}}}
	got := translate.NormalizeChatRequest(req)
	if got.Model != req.Model {
		t.Error("normalize changed model")
	}
}
