// Package capability describes what a provider model can do (chat, tools,
// vision, structured output, embeddings) and validates requests against it.
//
// The registry is configuration-driven: provider-kind defaults are a best
// effort starting point, and per-model rules let operators correct them. A
// missing table means "no enforcement" so tests and embedders built without
// capability data keep working.
package capability

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/fcordero/llm-api-gateway/internal/types"
)

// Set is the full capability surface of one model.
type Set struct {
	Chat          bool `yaml:"chat" json:"chat"`
	Stream        bool `yaml:"stream" json:"stream"`
	Tools         bool `yaml:"tools" json:"tools"`
	ParallelTools bool `yaml:"parallel_tools" json:"parallel_tools"`
	Vision        bool `yaml:"vision" json:"vision"`
	Audio         bool `yaml:"audio" json:"audio"`
	JSONObject    bool `yaml:"json_object" json:"json_object"`
	JSONSchema    bool `yaml:"json_schema" json:"json_schema"`
	Embeddings    bool `yaml:"embeddings" json:"embeddings"`

	ContextWindow int `yaml:"context_window" json:"context_window,omitempty"`
	MaxOutput     int `yaml:"max_output" json:"max_output,omitempty"`
}

// Requirements is the subset of capabilities a specific request needs.
type Requirements struct {
	Chat          bool
	Stream        bool
	Tools         bool
	ParallelTools bool
	Vision        bool
	Audio         bool
	JSONObject    bool
	JSONSchema    bool
	Embeddings    bool
}

// Empty reports whether no capability is required. A request with no
// requirements still falls back to the first model, preserving old behaviour.
func (r Requirements) Empty() bool {
	return r == Requirements{}
}

// Overrides is a partial Set: nil means "keep the inherited value".
// It is the YAML shape of providers[].capabilities[<model|glob|default>].
type Overrides struct {
	Chat          *bool `yaml:"chat" json:"chat,omitempty"`
	Stream        *bool `yaml:"stream" json:"stream,omitempty"`
	Tools         *bool `yaml:"tools" json:"tools,omitempty"`
	ParallelTools *bool `yaml:"parallel_tools" json:"parallel_tools,omitempty"`
	Vision        *bool `yaml:"vision" json:"vision,omitempty"`
	Audio         *bool `yaml:"audio" json:"audio,omitempty"`
	JSONObject    *bool `yaml:"json_object" json:"json_object,omitempty"`
	JSONSchema    *bool `yaml:"json_schema" json:"json_schema,omitempty"`
	Embeddings    *bool `yaml:"embeddings" json:"embeddings,omitempty"`

	ContextWindow *int `yaml:"context_window" json:"context_window,omitempty"`
	MaxOutput     *int `yaml:"max_output" json:"max_output,omitempty"`
}

// Apply merges the non-nil fields of o over base.
func (o Overrides) Apply(base Set) Set {
	if o.Chat != nil {
		base.Chat = *o.Chat
	}
	if o.Stream != nil {
		base.Stream = *o.Stream
	}
	if o.Tools != nil {
		base.Tools = *o.Tools
	}
	if o.ParallelTools != nil {
		base.ParallelTools = *o.ParallelTools
	}
	if o.Vision != nil {
		base.Vision = *o.Vision
	}
	if o.Audio != nil {
		base.Audio = *o.Audio
	}
	if o.JSONObject != nil {
		base.JSONObject = *o.JSONObject
	}
	if o.JSONSchema != nil {
		base.JSONSchema = *o.JSONSchema
	}
	if o.Embeddings != nil {
		base.Embeddings = *o.Embeddings
	}
	if o.ContextWindow != nil {
		base.ContextWindow = *o.ContextWindow
	}
	if o.MaxOutput != nil {
		base.MaxOutput = *o.MaxOutput
	}
	return base
}

// Table resolves capabilities for one provider kind plus its configured rules.
// Keys are model names, globs, or "default" for a provider-wide override.
type Table struct {
	Kind    string
	Default *Overrides
	rules   []rule
}

type rule struct {
	pattern  string
	exact    bool
	glob     *regexp.Regexp
	override Overrides
}

// NewTable builds a capability table. The "default" key applies to every
// model before model-specific rules; other keys are exact names or globs.
func NewTable(kind string, overrides map[string]Overrides) Table {
	t := Table{Kind: kind}
	for key, override := range overrides {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if key == "default" {
			o := override
			t.Default = &o
			continue
		}
		o := override
		t.rules = append(t.rules, rule{
			pattern:  key,
			exact:    !isGlob(key),
			glob:     compileGlob(key),
			override: o,
		})
	}
	// Exact rules first, then the longest glob. Ties are lexical so map
	// iteration order cannot change the result.
	sort.Slice(t.rules, func(i, j int) bool {
		a, b := t.rules[i], t.rules[j]
		if a.exact != b.exact {
			return a.exact
		}
		if len(a.pattern) != len(b.pattern) {
			return len(a.pattern) > len(b.pattern)
		}
		return a.pattern < b.pattern
	})
	return t
}

// Empty reports whether the table carries no information.
func (t Table) Empty() bool {
	return t.Kind == "" && t.Default == nil && len(t.rules) == 0
}

// For returns the effective capabilities of model.
func (t Table) For(model string) Set {
	set := DefaultsFor(t.Kind, model)
	if t.Default != nil {
		set = t.Default.Apply(set)
	}
	// rules are sorted most-specific first; apply them broadest-first so a
	// specific rule overrides only the fields it declares.
	for i := len(t.rules) - 1; i >= 0; i-- {
		r := t.rules[i]
		if r.exact {
			if r.pattern == model {
				set = r.override.Apply(set)
			}
			continue
		}
		if r.glob != nil && r.glob.MatchString(model) {
			set = r.override.Apply(set)
		}
	}
	return set
}

// Supports reports whether set covers every requirement and, when it does
// not, which capabilities are missing in stable order.
func (s Set) Supports(req Requirements) (bool, []string) {
	missing := s.Missing(req)
	return len(missing) == 0, missing
}

// Missing lists the capabilities required by req that s does not provide.
func (s Set) Missing(req Requirements) []string {
	var missing []string
	add := func(required, present bool, name string) {
		if required && !present {
			missing = append(missing, name)
		}
	}
	add(req.Chat, s.Chat, "chat")
	add(req.Stream, s.Stream, "stream")
	add(req.Tools, s.Tools, "tools")
	add(req.ParallelTools, s.ParallelTools, "parallel_tools")
	add(req.Vision, s.Vision, "vision")
	add(req.Audio, s.Audio, "audio")
	add(req.JSONObject, s.JSONObject, "json_object")
	add(req.JSONSchema, s.JSONSchema, "json_schema")
	add(req.Embeddings, s.Embeddings, "embeddings")
	return missing
}

// Param maps the first missing capability to the request field that caused
// it, so the client gets an actionable 400 instead of a generic message.
func Param(missing []string) string {
	for _, name := range missing {
		switch name {
		case "tools":
			return "tools"
		case "parallel_tools":
			return "parallel_tool_calls"
		case "vision", "audio":
			return "messages"
		case "json_object", "json_schema":
			return "response_format"
		case "stream":
			return "stream"
		case "embeddings":
			return "input"
		case "chat":
			return "model"
		}
	}
	return "model"
}

// MismatchError is returned before any provider call when the resolved model
// cannot serve the request. It maps to a 400 with code
// model_capability_mismatch.
type MismatchError struct {
	Provider string
	Model    string
	Missing  []string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("model %q on provider %q lacks required capabilities: %s",
		e.Model, e.Provider, strings.Join(e.Missing, ", "))
}

// Param returns the request field responsible for the mismatch.
func (e *MismatchError) Param() string { return Param(e.Missing) }

// DefaultsFor returns best-effort defaults for a provider kind and model.
// Explicit configuration always wins; these values only prevent obviously
// incompatible routing (for example, a tools request to an embedding model).
func DefaultsFor(kind, model string) Set {
	if isEmbeddingModel(model) {
		return Set{Embeddings: true}
	}
	switch strings.ToLower(kind) {
	case "openai":
		if isNonChatOpenAIModel(model) {
			return Set{}
		}
		return Set{
			Chat:          true,
			Stream:        true,
			Tools:         true,
			ParallelTools: true,
			Vision:        true,
			Audio:         true,
			JSONObject:    true,
			JSONSchema:    true,
		}
	case "anthropic":
		return Set{
			Chat:          true,
			Stream:        true,
			Tools:         true,
			ParallelTools: true,
			Vision:        true,
		}
	case "gemini":
		return Set{
			Chat:          true,
			Stream:        true,
			Tools:         true,
			ParallelTools: true,
			Vision:        true,
			Audio:         true,
			JSONObject:    true,
		}
	default:
		// Unknown adapters are opt-in: assume chat/stream rather than rejecting
		// every request, but require explicit rules for tools/vision.
		return Set{Chat: true, Stream: true}
	}
}

// ChatRequirements derives the capabilities needed by req.
func ChatRequirements(req types.ChatRequest) Requirements {
	reqs := Requirements{
		Chat:   true,
		Stream: req.Stream,
		Tools:  len(req.Tools) > 0,
	}
	if reqs.Tools && req.ParallelToolCalls != nil && *req.ParallelToolCalls {
		reqs.ParallelTools = true
	}
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
		case "json_object":
			reqs.JSONObject = true
		case "json_schema":
			reqs.JSONSchema = true
		}
	}
	for _, msg := range req.Messages {
		for _, part := range msg.ContentParts {
			if part.Type == "image_url" || part.ImageURL != nil {
				reqs.Vision = true
			}
			if part.Type == "input_audio" || part.InputAudio != nil {
				reqs.Audio = true
			}
		}
	}
	if req.Audio != nil {
		reqs.Audio = true
	}
	for _, modality := range req.Modalities {
		if strings.EqualFold(modality, "audio") {
			reqs.Audio = true
		}
	}
	return reqs
}

func isEmbeddingModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "embedding") || strings.Contains(m, "embed-") ||
		strings.HasPrefix(m, "embed")
}

func isNonChatOpenAIModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "whisper") || strings.HasPrefix(m, "tts-") ||
		strings.HasPrefix(m, "dall-e") || strings.HasPrefix(m, "omni-moderation")
}

func isGlob(s string) bool {
	return strings.ContainsAny(s, "*?")
}

// compileGlob converts a simple * / ? glob into an anchored regexp that also
// matches slashes. Returns nil for exact patterns.
func compileGlob(pattern string) *regexp.Regexp {
	if !isGlob(pattern) {
		return nil
	}
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}
