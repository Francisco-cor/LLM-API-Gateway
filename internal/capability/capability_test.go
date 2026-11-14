package capability

import (
	"reflect"
	"testing"

	"github.com/fcordero/llm-api-gateway/internal/types"
)

func boolPtr(v bool) *bool { return &v }

func TestDefaultsForProviderKinds(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		model  string
		assert func(t *testing.T, set Set)
	}{
		{
			name:  "openai chat model",
			kind:  "openai",
			model: "gpt-4o",
			assert: func(t *testing.T, set Set) {
				if !set.Chat || !set.Stream || !set.Tools || !set.Vision || !set.JSONSchema || !set.Audio {
					t.Fatalf("openai defaults missing capabilities: %+v", set)
				}
			},
		},
		{
			name:  "openai embedding model is not chat",
			kind:  "openai",
			model: "text-embedding-3-small",
			assert: func(t *testing.T, set Set) {
				if set.Chat || set.Tools || !set.Embeddings {
					t.Fatalf("embedding defaults wrong: %+v", set)
				}
			},
		},
		{
			name:  "openai audio-only model is not chat",
			kind:  "openai",
			model: "whisper-1",
			assert: func(t *testing.T, set Set) {
				if set.Chat || set.Embeddings {
					t.Fatalf("audio-only defaults wrong: %+v", set)
				}
			},
		},
		{
			name:  "anthropic has no structured output",
			kind:  "anthropic",
			model: "claude-sonnet-4-6",
			assert: func(t *testing.T, set Set) {
				if !set.Tools || !set.Vision || set.JSONSchema || set.JSONObject || set.Embeddings {
					t.Fatalf("anthropic defaults wrong: %+v", set)
				}
			},
		},
		{
			name:  "gemini supports json_object but not json_schema",
			kind:  "gemini",
			model: "gemini-2.5-pro",
			assert: func(t *testing.T, set Set) {
				if !set.JSONObject || set.JSONSchema || !set.Audio || !set.Vision {
					t.Fatalf("gemini defaults wrong: %+v", set)
				}
			},
		},
		{
			name:  "gemini embedding model",
			kind:  "gemini",
			model: "text-embedding-004",
			assert: func(t *testing.T, set Set) {
				if set.Chat || !set.Embeddings {
					t.Fatalf("gemini embedding defaults wrong: %+v", set)
				}
			},
		},
		{
			name:  "unknown kind keeps chat only",
			kind:  "custom",
			model: "whatever",
			assert: func(t *testing.T, set Set) {
				if !set.Chat || set.Tools || set.Vision {
					t.Fatalf("unknown-kind defaults wrong: %+v", set)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.assert(t, DefaultsFor(tc.kind, tc.model))
		})
	}
}

func TestOverridesAndSpecificity(t *testing.T) {
	table := NewTable("openai", map[string]Overrides{
		"default":     {Vision: boolPtr(false)},
		"gpt-4*":      {Audio: boolPtr(false)},
		"gpt-4o-*":    {ParallelTools: boolPtr(false)},
		"gpt-4o":      {Tools: boolPtr(false)},
		"gpt-4o-mini": {JSONSchema: boolPtr(false)},
	})

	mini := table.For("gpt-4o-mini")
	if mini.Vision {
		t.Error("default override did not disable vision")
	}
	if mini.Audio {
		t.Error("glob override did not disable audio")
	}
	if mini.ParallelTools {
		t.Error("more specific glob (gpt-4o-*) did not win over gpt-4*")
	}
	if mini.JSONSchema {
		t.Error("exact rule did not win over globs")
	}
	if !mini.Tools {
		t.Error("gpt-4o exact rule must not leak into gpt-4o-mini")
	}
	if !mini.Chat {
		t.Error("chat default should be preserved")
	}

	fourO := table.For("gpt-4o")
	if fourO.Tools {
		t.Error("exact gpt-4o rule should apply")
	}
	if !fourO.JSONSchema {
		t.Error("gpt-4o JSON schema should stay enabled")
	}

	other := table.For("gpt-4-turbo")
	if !other.Chat || !other.Tools || other.Audio {
		t.Errorf("gpt-4-turbo inherited unexpected capabilities: %+v", other)
	}
}

func TestGlobMatchesSlashes(t *testing.T) {
	table := NewTable("openai", map[string]Overrides{
		"meta-llama/*": {Tools: boolPtr(false)},
	})
	set := table.For("meta-llama/Llama-3-70B")
	if set.Tools {
		t.Error("glob with slash did not match")
	}
}

func TestChatRequirements(t *testing.T) {
	maxTokens := 10
	req := types.ChatRequest{
		Model:               "gpt-4o",
		Stream:              true,
		Tools:               []types.Tool{{Type: "function"}},
		ParallelToolCalls:   boolPtr(true),
		ResponseFormat:      &types.ResponseFormat{Type: "json_schema"},
		MaxCompletionTokens: &maxTokens,
		Messages: []types.ChatMessage{{
			Role: "user",
			ContentParts: []types.ContentPart{
				{Type: "image_url", ImageURL: &types.ImageURL{URL: "https://example.com/a.png"}},
				{Type: "input_audio", InputAudio: &types.InputAudio{Data: "..."}},
			},
		}},
	}
	got := ChatRequirements(req)
	want := Requirements{
		Chat:          true,
		Stream:        true,
		Tools:         true,
		ParallelTools: true,
		Vision:        true,
		Audio:         true,
		JSONSchema:    true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %+v, want %+v", got, want)
	}
}

func TestSupportsAndMissingOrder(t *testing.T) {
	set := Set{Chat: true}
	ok, missing := set.Supports(Requirements{Chat: true, Tools: true, Vision: true, JSONSchema: true})
	if ok {
		t.Fatal("incomplete set reported as supported")
	}
	want := []string{"tools", "vision", "json_schema"}
	if !reflect.DeepEqual(missing, want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	if param := Param(missing); param != "tools" {
		t.Errorf("param = %q, want tools", param)
	}
	if param := Param([]string{"json_schema"}); param != "response_format" {
		t.Errorf("param = %q, want response_format", param)
	}
	if param := Param([]string{"vision"}); param != "messages" {
		t.Errorf("param = %q, want messages", param)
	}
}

func TestEmptyRequirementsNeverMismatch(t *testing.T) {
	if ok, missing := (Set{}).Supports(Requirements{}); !ok || len(missing) != 0 {
		t.Fatalf("empty requirements should always pass, got ok=%v missing=%v", ok, missing)
	}
}
