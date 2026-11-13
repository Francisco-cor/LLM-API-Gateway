package translate

import (
	"errors"
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/types"
)

func TestAnthropicStrictMessagesAndParameters(t *testing.T) {
	temp := 1.5
	topP := 0.7
	topK := 40
	cases := []struct {
		name       string
		req        types.ChatRequest
		wantSystem string
		wantStops  []string
		wantTemp   float64
		wantError  bool
	}{
		{
			name: "all system/developer messages are concatenated",
			req: types.ChatRequest{Messages: []types.ChatMessage{
				{Role: "system", Content: "first"},
				{Role: "developer", Content: "second"},
				{Role: "system", Content: "third"},
				{Role: "user", Content: "hi"},
			}},
			wantSystem: "first\n\nsecond\n\nthird",
		},
		{
			name: "string stop and clamped temperature",
			req: types.ChatRequest{
				Messages:    []types.ChatMessage{{Role: "user", Content: "hi"}},
				Stop:        "END",
				Temperature: &temp,
				TopP:        &topP,
				TopK:        &topK,
			},
			wantStops: []string{"END"},
			wantTemp:  1,
		},
		{
			name: "array stop",
			req: types.ChatRequest{
				Messages: []types.ChatMessage{{Role: "user", Content: "hi"}},
				Stop:     []any{"a", "b"},
			},
			wantStops: []string{"a", "b"},
		},
		{
			name: "unknown role is rejected",
			req: types.ChatRequest{Messages: []types.ChatMessage{
				{Role: "critic", Content: "nope"},
			}},
			wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			native, err := ToAnthropicStrict(tc.req)
			if tc.wantError {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if native.System != tc.wantSystem {
				t.Errorf("system = %q, want %q", native.System, tc.wantSystem)
			}
			if len(tc.wantStops) > 0 {
				if len(native.StopSequences) != len(tc.wantStops) {
					t.Fatalf("stop_sequences = %v, want %v", native.StopSequences, tc.wantStops)
				}
				for i := range tc.wantStops {
					if native.StopSequences[i] != tc.wantStops[i] {
						t.Errorf("stop_sequences[%d] = %q, want %q", i, native.StopSequences[i], tc.wantStops[i])
					}
				}
			}
			if tc.wantTemp != 0 && (native.Temperature == nil || *native.Temperature != tc.wantTemp) {
				t.Errorf("temperature = %v, want %v", native.Temperature, tc.wantTemp)
			}
		})
	}
}

func TestAnthropicStrictUnsupportedParameters(t *testing.T) {
	one := 2
	seed := 7
	penalty := 0.5
	cases := []struct {
		name string
		req  types.ChatRequest
	}{
		{name: "n greater than one", req: types.ChatRequest{N: &one}},
		{name: "seed", req: types.ChatRequest{Seed: &seed}},
		{name: "presence penalty", req: types.ChatRequest{PresencePenalty: &penalty}},
		{name: "frequency penalty", req: types.ChatRequest{FrequencyPenalty: &penalty}},
		{name: "json schema", req: types.ChatRequest{ResponseFormat: &types.ResponseFormat{Type: "json_schema"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Messages = []types.ChatMessage{{Role: "user", Content: "hi"}}
			_, err := ToAnthropicStrict(tc.req)
			var translationErr *Error
			if !errors.As(err, &translationErr) {
				t.Fatalf("error = %v, want *translate.Error", err)
			}
			if translationErr.Code != "unsupported_parameter" {
				t.Errorf("code = %q, want unsupported_parameter", translationErr.Code)
			}
		})
	}
}

func TestAnthropicStrictMultimodal(t *testing.T) {
	req := types.ChatRequest{Messages: []types.ChatMessage{{
		Role: "user",
		ContentParts: []types.ContentPart{
			{Type: "text", Text: "describe"},
			{Type: "image_url", ImageURL: &types.ImageURL{URL: "data:image/png;base64,AAAA"}},
			{Type: "image_url", ImageURL: &types.ImageURL{URL: "https://example.test/cat.png"}},
		},
	}}}
	native, err := ToAnthropicStrict(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	blocks, ok := native.Messages[0].Content.([]AnthropicContentBlock)
	if !ok || len(blocks) != 3 {
		t.Fatalf("content = %#v", native.Messages[0].Content)
	}
	if blocks[1].Source == nil || blocks[1].Source.Type != "base64" || blocks[1].Source.MediaType != "image/png" || blocks[1].Source.Data != "AAAA" {
		t.Errorf("base64 image source = %+v", blocks[1].Source)
	}
	if blocks[2].Source == nil || blocks[2].Source.Type != "url" || blocks[2].Source.URL != "https://example.test/cat.png" {
		t.Errorf("url image source = %+v", blocks[2].Source)
	}

	audio := types.ChatRequest{Messages: []types.ChatMessage{{
		Role: "user",
		ContentParts: []types.ContentPart{
			{Type: "input_audio", InputAudio: &types.InputAudio{Data: "AAAA", Format: "wav"}},
		},
	}}}
	if _, err := ToAnthropicStrict(audio); err == nil {
		t.Fatal("expected anthropic audio input to be rejected")
	}
}

func TestAnthropicToolsDefaultSchemaAndInvalidArguments(t *testing.T) {
	native, err := ToAnthropicStrict(types.ChatRequest{
		Tools: []types.Tool{{Type: "function", Function: types.ToolFunc{Name: "lookup"}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	schema, ok := native.Tools[0].InputSchema.(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("input_schema = %#v", native.Tools[0].InputSchema)
	}
	if _, ok := schema["properties"]; !ok {
		t.Fatalf("input_schema properties missing: %#v", schema)
	}

	invalid := types.ChatRequest{Messages: []types.ChatMessage{{
		Role: "assistant",
		ToolCalls: []types.ToolCall{{
			ID: "call_1", Type: "function",
			Function: types.ToolCallFunction{Name: "lookup", Arguments: "{not json"},
		}},
	}}}
	_, err = ToAnthropicStrict(invalid)
	var translationErr *Error
	if !errors.As(err, &translationErr) || translationErr.Code != "invalid_request" {
		t.Fatalf("error = %v, want invalid_request", err)
	}
}

func TestGeminiStrictMessagesParametersAndTools(t *testing.T) {
	temp := 3.0
	n := 2
	req := types.ChatRequest{
		Temperature: &temp,
		Stop:        []string{"END"},
		N:           &n,
		Messages: []types.ChatMessage{
			{Role: "system", Content: "one"},
			{Role: "developer", Content: "two"},
			{Role: "user", ContentParts: []types.ContentPart{
				{Type: "image_url", ImageURL: &types.ImageURL{URL: "data:image/jpeg;base64,BBBB"}},
			}},
			{Role: "assistant", ToolCalls: []types.ToolCall{{
				ID: "call_0", Type: "function",
				Function: types.ToolCallFunction{Name: "weather", Arguments: `{"city":"Paris"}`},
			}}},
			{Role: "tool", ToolCallID: "call_0", Content: `{"temperature":21}`},
		},
	}
	native, err := ToGeminiStrict(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if native.SystemInstruction == nil || native.SystemInstruction.Parts[0].Text != "one\n\ntwo" {
		t.Errorf("systemInstruction = %+v", native.SystemInstruction)
	}
	if native.GenerationConfig.Temperature == nil || *native.GenerationConfig.Temperature != 2 {
		t.Errorf("temperature = %v, want clamp to 2", native.GenerationConfig.Temperature)
	}
	if len(native.GenerationConfig.StopSequences) != 1 || native.GenerationConfig.StopSequences[0] != "END" {
		t.Errorf("stopSequences = %v", native.GenerationConfig.StopSequences)
	}
	if native.GenerationConfig.CandidateCount == nil || *native.GenerationConfig.CandidateCount != 2 {
		t.Errorf("candidateCount = %v", native.GenerationConfig.CandidateCount)
	}
	image := native.Contents[0].Parts[0].InlineData
	if image == nil || image.MimeType != "image/jpeg" || image.Data != "BBBB" {
		t.Errorf("inline image = %+v", image)
	}
	if native.Contents[1].Role != "model" || native.Contents[1].Parts[0].FunctionCall == nil {
		t.Errorf("assistant function call = %+v", native.Contents[1])
	}
	response := native.Contents[2].Parts[0].FunctionResponse
	if response == nil || response.Name != "weather" {
		t.Fatalf("functionResponse = %+v", response)
	}
	object, ok := response.Response.(map[string]any)
	if !ok || object["temperature"] != float64(21) {
		t.Errorf("functionResponse.response = %#v", response.Response)
	}

	textResult := types.ChatRequest{Messages: []types.ChatMessage{
		{Role: "assistant", ToolCalls: []types.ToolCall{{ID: "call_0", Function: types.ToolCallFunction{Name: "weather", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call_0", Content: "sunny"},
	}}
	native, err = ToGeminiStrict(textResult)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wrapped, ok := native.Contents[1].Parts[0].FunctionResponse.Response.(map[string]any)
	if !ok || wrapped["content"] != "sunny" {
		t.Errorf("wrapped functionResponse = %#v", native.Contents[1].Parts[0].FunctionResponse.Response)
	}
}

func TestGeminiStrictRejectsUnsupportedInput(t *testing.T) {
	remoteImage := types.ChatRequest{Messages: []types.ChatMessage{{
		Role: "user",
		ContentParts: []types.ContentPart{
			{Type: "image_url", ImageURL: &types.ImageURL{URL: "https://example.test/cat.png"}},
		},
	}}}
	if _, err := ToGeminiStrict(remoteImage); err == nil {
		t.Fatal("expected remote image URL to be rejected for gemini")
	}

	n := 9
	if _, err := ToGeminiStrict(types.ChatRequest{N: &n}); err == nil {
		t.Fatal("expected n>8 to be rejected for gemini")
	}
	if _, err := ToGeminiStrict(types.ChatRequest{ResponseFormat: &types.ResponseFormat{Type: "json_schema"}}); err == nil {
		t.Fatal("expected json_schema to be rejected for gemini")
	}
	if _, err := ToGeminiStrict(types.ChatRequest{Messages: []types.ChatMessage{{Role: "critic"}}}); err == nil {
		t.Fatal("expected unknown role to be rejected for gemini")
	}
	if _, err := ToGeminiStrict(types.ChatRequest{Tools: []types.Tool{{Function: types.ToolFunc{Name: "x"}}}, Messages: []types.ChatMessage{
		{Role: "tool", ToolCallID: "missing", Content: "x"},
	}}); err == nil {
		t.Fatal("expected unknown tool_call_id to be rejected for gemini")
	}
}

func TestGeminiJSONObjectResponseFormat(t *testing.T) {
	native, err := ToGeminiStrict(types.ChatRequest{
		ResponseFormat: &types.ResponseFormat{Type: "json_object"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if native.GenerationConfig.ResponseMimeType != "application/json" {
		t.Errorf("responseMimeType = %q", native.GenerationConfig.ResponseMimeType)
	}
}

func TestAnthropicResponseStopReasonsAndCacheUsage(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{reason: "end_turn", want: "stop"},
		{reason: "stop_sequence", want: "stop"},
		{reason: "tool_use", want: "tool_calls"},
		{reason: "max_tokens", want: "length"},
		{reason: "refusal", want: "content_filter"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			resp := AnthropicResponse{
				Content:    []AnthropicContentBlock{{Type: "text", Text: "hello"}},
				StopReason: tc.reason,
			}
			resp.Usage.InputTokens = 10
			resp.Usage.CacheCreationInputTokens = 2
			resp.Usage.CacheReadInputTokens = 3
			resp.Usage.OutputTokens = 4
			got := FromAnthropic(resp)
			if got.Choices[0].FinishReason != tc.want {
				t.Errorf("finish_reason = %q, want %q", got.Choices[0].FinishReason, tc.want)
			}
			if got.Usage.PromptTokens != 15 || got.Usage.CompletionTokens != 4 || got.Usage.TotalTokens != 19 {
				t.Errorf("usage = %+v, want prompt 15 completion 4 total 19", got.Usage)
			}
			if got.Usage.PromptTokensDetails == nil || got.Usage.PromptTokensDetails.CachedTokens != 3 {
				t.Errorf("cached tokens = %+v", got.Usage.PromptTokensDetails)
			}
		})
	}
}

func TestGeminiResponseReasoningUsageAndFinishReasons(t *testing.T) {
	resp := GeminiResponse{}
	resp.Candidates = append(resp.Candidates, struct {
		Content      GeminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	}{
		Content: GeminiContent{Parts: []GeminiPart{
			{Text: "answer"},
			{FunctionCall: &GeminiFunctionCall{Name: "lookup"}},
		}},
		FinishReason: "STOP",
	})
	resp.UsageMetadata = GeminiUsageMetadata{
		PromptTokenCount:     10,
		CandidatesTokenCount: 5,
		ThoughtsTokenCount:   3,
		TotalTokenCount:      18,
	}
	got := FromGemini(resp, "gemini-test")
	if got.Usage.CompletionTokens != 8 || got.Usage.TotalTokens != 18 {
		t.Errorf("usage = %+v, want completion 8 total 18", got.Usage)
	}
	if got.Usage.CompletionTokensDetails == nil || got.Usage.CompletionTokensDetails.ReasoningTokens != 3 {
		t.Errorf("reasoning tokens = %+v", got.Usage.CompletionTokensDetails)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
	calls := got.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_0" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if calls[0].Function.Arguments != "{}" {
		t.Errorf("arguments = %q, want {}", calls[0].Function.Arguments)
	}

	filters := []string{"SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "MALFORMED_FUNCTION_CALL"}
	for _, reason := range filters {
		if got := GeminiFinishReason(reason, false); got != "content_filter" {
			t.Errorf("GeminiFinishReason(%q) = %q, want content_filter", reason, got)
		}
	}
}
