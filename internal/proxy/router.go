package proxy

import (
	"fmt"
	"math/rand"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// Registry resolves model names to the Provider that serves them and looks
// up providers by name for fallback. It supports:
// - exact model mapping
// - wildcard/regex patterns in provider models (e.g. "gpt-4*", "claude-*")
// - weighted routing per model (routing.weighted in config) for canary/blue-green
// - model_aliases (e.g. "gpt-4o" -> ["claude-sonnet-4-6"]) for fallback remapping.
type Registry struct {
	byName  map[string]provider.Provider
	byModel map[string]provider.Provider // legacy exact map (single provider per model) for backward compat
	order   []provider.Provider
	aliases map[string][]string

	// extended routing
	weighted  map[string][]weightedEntry
	patterns  []patternEntry
	weightedMu sync.RWMutex
	randMu     sync.Mutex
	rnd        *rand.Rand
}

type weightedEntry struct {
	provider provider.Provider
	weight   int
}

type patternEntry struct {
	provider provider.Provider
	pattern  string
	regex    *regexp.Regexp
	isRegex  bool
}

// NewRegistry builds a Registry from the given providers, indexing each by
// name and by every model it declares.
func NewRegistry(providers []provider.Provider) *Registry {
	return NewRegistryWithAliases(providers, nil)
}

// NewRegistryWithAliases builds a Registry with model alias support.
func NewRegistryWithAliases(providers []provider.Provider, aliases map[string][]string) *Registry {
	return NewRegistryWithWeighted(providers, aliases, nil)
}

// NewRegistryWithWeighted builds a Registry with alias and weighted routing support.
// weighted is map model -> []{provider name, weight}. Provider names are resolved
// against the passed providers list.
func NewRegistryWithWeighted(providers []provider.Provider, aliases map[string][]string, weighted map[string][]WeightedConfig) *Registry {
	r := &Registry{
		byName:   make(map[string]provider.Provider),
		byModel:  make(map[string]provider.Provider),
		aliases:  aliases,
		weighted: make(map[string][]weightedEntry),
		rnd:      rand.New(rand.NewSource(42)), // deterministic seed for tests; overwritten with time-based in production via SetRandSeed
	}
	for _, p := range providers {
		r.byName[p.Name()] = p
		r.order = append(r.order, p)
		for _, model := range p.Models() {
			// detect pattern vs exact
			if isPattern(model) {
				re, isRegex := compilePattern(model)
				r.patterns = append(r.patterns, patternEntry{
					provider: p,
					pattern:  model,
					regex:    re,
					isRegex:  isRegex,
				})
			} else {
				// keep last provider for exact map (backward compat)
				r.byModel[model] = p
			}
		}
	}
	// build weighted entries
	for model, entries := range weighted {
		var we []weightedEntry
		for _, e := range entries {
			name := e.ProviderName()
			if prov, ok := r.byName[name]; ok {
				we = append(we, weightedEntry{provider: prov, weight: e.Weight})
			}
		}
		if len(we) > 0 {
			r.weighted[model] = we
		}
	}
	return r
}

// WeightedConfig mirrors config.WeightedEntry but avoids import cycle.
// Defined here so router can be configured without importing config.
type WeightedConfig struct {
	Provider string `yaml:"provider"`
	Name     string `yaml:"name"`
	Weight   int    `yaml:"weight"`
}

func (w WeightedConfig) ProviderName() string {
	if w.Provider != "" {
		return w.Provider
	}
	return w.Name
}

// SetRandSeed allows main.go to seed the registry's RNG with time.Now for production.
func (r *Registry) SetRandSeed(seed int64) {
	r.randMu.Lock()
	defer r.randMu.Unlock()
	r.rnd = rand.New(rand.NewSource(seed))
}

// isPattern reports whether model string contains wildcard or regex meta.
func isPattern(s string) bool {
	return strings.Contains(s, "*") || strings.Contains(s, "?") || strings.Contains(s, "[") || looksLikeRegex(s)
}

func looksLikeRegex(s string) bool {
	// heuristic: contains regex anchors or quantifiers that are not simple wildcards
	return strings.Contains(s, ".*") || strings.Contains(s, "^") || strings.Contains(s, "$") || strings.Contains(s, "(") || strings.Contains(s, "|")
}

func compilePattern(pattern string) (*regexp.Regexp, bool) {
	if looksLikeRegex(pattern) {
		// treat as regex directly
		if re, err := regexp.Compile(pattern); err == nil {
			return re, true
		}
	}
	// treat as glob -> convert to regex
	reStr := regexp.QuoteMeta(pattern)
	reStr = strings.ReplaceAll(reStr, `\*`, `.*`)
	reStr = strings.ReplaceAll(reStr, `\?`, `.`)
	reStr = "^" + reStr + "$"
	re, err := regexp.Compile(reStr)
	if err != nil {
		return nil, false
	}
	return re, false
}

func (r *Registry) matchesPattern(entry patternEntry, model string) bool {
	if entry.regex != nil {
		return entry.regex.MatchString(model)
	}
	// fallback to path.Match
	matched, _ := path.Match(entry.pattern, model)
	return matched
}

// Resolve returns the Provider configured to serve model.
// Resolution order:
// 1. weighted routing for exact model name
// 2. exact match
// 3. weighted routing for pattern that matches model (if model key in weighted uses pattern)
// 4. pattern/wildcard match (first match, or random among multiple)
// 5. error
func (r *Registry) Resolve(model string) (provider.Provider, error) {
	// 1. weighted exact
	if we, ok := r.weighted[model]; ok && len(we) > 0 {
		return r.pickWeighted(we), nil
	}
	// 2. exact
	if p, ok := r.byModel[model]; ok {
		return p, nil
	}
	// 3. weighted pattern keys: check if any weighted key is a pattern matching model
	for key, we := range r.weighted {
		if isPattern(key) {
			re, _ := compilePattern(key)
			if re != nil && re.MatchString(model) {
				return r.pickWeighted(we), nil
			}
			if matched, _ := path.Match(key, model); matched {
				return r.pickWeighted(we), nil
			}
		}
	}
	// 4. pattern match
	var candidates []provider.Provider
	for _, pe := range r.patterns {
		if r.matchesPattern(pe, model) {
			candidates = append(candidates, pe.provider)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) > 1 {
		// pick randomly among candidates (equal weight) to distribute
		r.randMu.Lock()
		idx := r.rnd.Intn(len(candidates))
		r.randMu.Unlock()
		return candidates[idx], nil
	}
	return nil, fmt.Errorf("%w: model %q", provider.ErrNoProvider, model)
}

func (r *Registry) pickWeighted(entries []weightedEntry) provider.Provider {
	total := 0
	for _, e := range entries {
		total += e.weight
	}
	if total == 0 {
		return entries[0].provider
	}
	r.randMu.Lock()
	n := r.rnd.Intn(total)
	r.randMu.Unlock()
	acc := 0
	for _, e := range entries {
		acc += e.weight
		if n < acc {
			return e.provider
		}
	}
	return entries[len(entries)-1].provider
}

// Get returns the provider registered under name, if any.
func (r *Registry) Get(name string) (provider.Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// All returns every registered provider in registration order.
func (r *Registry) All() []provider.Provider {
	return r.order
}

// Aliases returns alias targets for model, if any.
func (r *Registry) Aliases(model string) []string {
	return r.aliases[model]
}

// RemapForFallback returns a ChatRequest with Model remapped to fallback's model
// if an alias exists. Otherwise returns req unchanged.
func (r *Registry) RemapForFallback(req provider.ChatRequest, fallback provider.Provider) provider.ChatRequest {
	targets := r.aliases[req.Model]
	if len(targets) == 0 {
		return req
	}
	// If fallback owns any of the alias targets, use the first it owns
	for _, t := range targets {
		for _, m := range fallback.Models() {
			if m == t || matchesModelPattern(m, t) || matchesModelPattern(t, m) {
				req.Model = t
				return req
			}
		}
	}
	// Otherwise map to first available model of fallback (best-effort)
	if len(fallback.Models()) > 0 {
		req.Model = fallback.Models()[0]
	}
	return req
}

// matchesModelPattern checks if either pattern matches the other (for alias remapping tolerance)
func matchesModelPattern(pattern, model string) bool {
	if pattern == model {
		return true
	}
	if isPattern(pattern) {
		re, _ := compilePattern(pattern)
		if re != nil && re.MatchString(model) {
			return true
		}
		matched, _ := path.Match(pattern, model)
		return matched
	}
	return false
}
