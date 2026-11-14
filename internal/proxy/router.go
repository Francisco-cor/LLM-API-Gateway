package proxy

import (
	"fmt"
	"math/rand"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/fcordero/llm-api-gateway/internal/capability"
	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// Registry resolves model names to the Provider that serves them and looks
// up providers by name for fallback. It supports:
// - exact model mapping
// - wildcard/regex patterns in provider models (e.g. "gpt-4*", "claude-*")
// - weighted routing per model (routing.weighted in config) for canary/blue-green
// - model_aliases (e.g. "gpt-4o" -> ["claude-sonnet-4-6"]) for fallback remapping.
type Registry struct {
	mu      sync.RWMutex
	byName  map[string]provider.Provider
	byModel map[string]provider.Provider // legacy exact map (single provider per model) for backward compat
	order   []provider.Provider
	aliases map[string][]string

	// extended routing
	weighted        map[string][]weightedEntry
	weightedPattern []weightedPatternEntry
	patterns        []patternEntry
	// capabilities is keyed by provider name. A missing table means the
	// provider has no capability metadata and requests are not gated.
	capabilities map[string]capability.Table
	randMu       sync.Mutex
	rnd          *rand.Rand
}

type weightedEntry struct {
	provider provider.Provider
	weight   int
}

// weightedPatternEntry is a precompiled weighted key that is a glob or regex.
// Compiling it once at construction keeps Resolve out of regexp.Compile on the
// hot path (previously every request recompiled every weighted pattern key).
type weightedPatternEntry struct {
	key     string
	regex   *regexp.Regexp
	isRegex bool
	entries []weightedEntry
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
	return NewRegistryWithCapabilities(providers, aliases, weighted, nil)
}

// NewRegistryWithCapabilities builds a Registry with capability metadata.
// Tables are keyed by provider name; nil disables capability enforcement.
func NewRegistryWithCapabilities(providers []provider.Provider, aliases map[string][]string, weighted map[string][]WeightedConfig, tables map[string]capability.Table) *Registry {
	r := &Registry{
		byName:       make(map[string]provider.Provider),
		byModel:      make(map[string]provider.Provider),
		aliases:      cloneAliases(aliases),
		weighted:     make(map[string][]weightedEntry),
		capabilities: tables,
		rnd:          rand.New(rand.NewSource(42)), // deterministic seed for tests; overwritten with time-based in production via SetRandSeed
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
	r.weightedPattern = compileWeightedPatterns(r.weighted)
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

// compileWeightedPatterns precompiles weighted keys that are globs or regexes
// and orders them so the most specific pattern is tried first. Ties use lexical
// order to stay deterministic across map iteration.
func compileWeightedPatterns(weighted map[string][]weightedEntry) []weightedPatternEntry {
	var out []weightedPatternEntry
	for key, entries := range weighted {
		if !isPattern(key) {
			continue
		}
		re, isRegex := compilePattern(key)
		out = append(out, weightedPatternEntry{key: key, regex: re, isRegex: isRegex, entries: entries})
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].key) != len(out[j].key) {
			return len(out[i].key) > len(out[j].key)
		}
		return out[i].key < out[j].key
	})
	return out
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	// 1. weighted exact
	if we, ok := r.weighted[model]; ok && len(we) > 0 {
		return r.pickWeighted(we), nil
	}
	// 2. exact
	if p, ok := r.byModel[model]; ok {
		return p, nil
	}
	// 3. weighted pattern keys: check if any weighted key is a pattern matching model
	for _, wp := range r.weightedPattern {
		if wp.regex != nil && wp.regex.MatchString(model) {
			return r.pickWeighted(wp.entries), nil
		}
		if !wp.isRegex {
			if matched, _ := path.Match(wp.key, model); matched {
				return r.pickWeighted(wp.entries), nil
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

// ResolveForChat resolves model and enforces capability requirements before
// any provider call. Unlike Resolve, it filters weighted and pattern candidates
// instead of picking an incompatible provider at random, and returns a
// *capability.MismatchError when candidates exist but none can serve reqs.
func (r *Registry) ResolveForChat(model string, reqs capability.Requirements) (provider.Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var lastMismatch *capability.MismatchError
	if we, ok := r.weighted[model]; ok && len(we) > 0 {
		if p, mismatch := r.pickWeightedCompatible(we, model, reqs); p != nil {
			return p, nil
		} else if mismatch != nil {
			lastMismatch = mismatch
		}
	}
	if p, ok := r.byModel[model]; ok {
		if ok, missing := r.supportsLocked(p.Name(), model, reqs); ok {
			return p, nil
		} else {
			// The exact owner cannot serve the request; a pattern provider may
			// still be able to, so keep looking instead of failing immediately.
			lastMismatch = &capability.MismatchError{Provider: p.Name(), Model: model, Missing: missing}
		}
	}
	for _, wp := range r.weightedPattern {
		if !r.weightedPatternMatches(wp, model) {
			continue
		}
		if p, mismatch := r.pickWeightedCompatible(wp.entries, model, reqs); p != nil {
			return p, nil
		} else if mismatch != nil {
			lastMismatch = mismatch
		}
	}
	var compatible []provider.Provider
	for _, pe := range r.patterns {
		if !r.matchesPattern(pe, model) {
			continue
		}
		if ok, missing := r.supportsLocked(pe.provider.Name(), model, reqs); ok {
			compatible = append(compatible, pe.provider)
		} else if lastMismatch == nil {
			lastMismatch = &capability.MismatchError{Provider: pe.provider.Name(), Model: model, Missing: missing}
		}
	}
	if len(compatible) == 1 {
		return compatible[0], nil
	}
	if len(compatible) > 1 {
		r.randMu.Lock()
		idx := r.rnd.Intn(len(compatible))
		r.randMu.Unlock()
		return compatible[idx], nil
	}
	if lastMismatch != nil {
		return nil, lastMismatch
	}
	return nil, fmt.Errorf("%w: model %q", provider.ErrNoProvider, model)
}

func (r *Registry) weightedPatternMatches(wp weightedPatternEntry, model string) bool {
	if wp.regex != nil {
		return wp.regex.MatchString(model)
	}
	matched, _ := path.Match(wp.key, model)
	return matched
}

func (r *Registry) pickWeightedCompatible(entries []weightedEntry, model string, reqs capability.Requirements) (provider.Provider, *capability.MismatchError) {
	var compatible []weightedEntry
	var mismatch *capability.MismatchError
	for _, e := range entries {
		if ok, missing := r.supportsLocked(e.provider.Name(), model, reqs); ok {
			compatible = append(compatible, e)
		} else if mismatch == nil {
			mismatch = &capability.MismatchError{Provider: e.provider.Name(), Model: model, Missing: missing}
		}
	}
	if len(compatible) > 0 {
		return r.pickWeighted(compatible), nil
	}
	return nil, mismatch
}

// Capabilities returns the effective capability set for a provider model.
// ok is false when the provider has no capability table.
func (r *Registry) Capabilities(providerName, model string) (capability.Set, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.capabilities[providerName]
	if !ok || t.Empty() {
		return capability.Set{}, false
	}
	return t.For(model), true
}

// Supports reports whether providerName/model covers reqs. Providers without a
// capability table are treated as compatible (no enforcement).
func (r *Registry) Supports(providerName, model string, reqs capability.Requirements) (bool, []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.supportsLocked(providerName, model, reqs)
}

func (r *Registry) supportsLocked(providerName, model string, reqs capability.Requirements) (bool, []string) {
	if reqs.Empty() {
		return true, nil
	}
	t, ok := r.capabilities[providerName]
	if !ok || t.Empty() {
		return true, nil
	}
	return t.For(model).Supports(reqs)
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[name]
	return p, ok
}

// All returns every registered provider in registration order.
func (r *Registry) All() []provider.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]provider.Provider, len(r.order))
	copy(out, r.order)
	return out
}

// Aliases returns alias targets for model, if any.
func (r *Registry) Aliases(model string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.aliases[model]...)
}

// Reload atomically replaces registry contents with new providers/aliases/weighted.
// Used for hot-reload without replacing the *Registry pointer (so all handlers see update).
// Capability tables are preserved; use ReloadWithCapabilities to replace them too.
func (r *Registry) Reload(providers []provider.Provider, aliases map[string][]string, weighted map[string][]WeightedConfig) {
	r.mu.RLock()
	tables := r.capabilities
	r.mu.RUnlock()
	r.ReloadWithCapabilities(providers, aliases, weighted, tables)
}

// ReloadWithCapabilities replaces providers, aliases, weighted routing and
// capability metadata atomically.
func (r *Registry) ReloadWithCapabilities(providers []provider.Provider, aliases map[string][]string, weighted map[string][]WeightedConfig, tables map[string]capability.Table) {
	newR := NewRegistryWithCapabilities(providers, aliases, weighted, tables)
	// preserve RNG seed
	r.randMu.Lock()
	newR.rnd = r.rnd
	r.randMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName = newR.byName
	r.byModel = newR.byModel
	r.order = newR.order
	r.aliases = newR.aliases
	r.weighted = newR.weighted
	r.weightedPattern = newR.weightedPattern
	r.patterns = newR.patterns
	r.capabilities = newR.capabilities
}

// UpdateAliases atomically updates aliases (for PATCH runtime).
func (r *Registry) UpdateAliases(aliases map[string][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases = cloneAliases(aliases)
}

// RemapForFallback returns a ChatRequest with Model remapped to fallback's model
// if an alias exists. Otherwise returns req unchanged.
func (r *Registry) RemapForFallback(req provider.ChatRequest, fallback provider.Provider) provider.ChatRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.remapForFallbackLocked(req, fallback)
}

func (r *Registry) remapForFallbackLocked(req provider.ChatRequest, fallback provider.Provider) provider.ChatRequest {
	targets := append([]string(nil), r.aliases[req.Model]...)
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

// RemapForFallbackWithCapabilities picks a fallback model that still satisfies
// reqs. It returns ok=false when the fallback cannot serve the request, so the
// caller can skip it instead of silently degrading tools, vision or structured
// output. Providers without capability metadata keep the historical remap.
func (r *Registry) RemapForFallbackWithCapabilities(req provider.ChatRequest, fallback provider.Provider, reqs capability.Requirements) (provider.ChatRequest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if !r.hasCapabilityTableLocked(fallback.Name()) {
		return r.remapForFallbackLocked(req, fallback), true
	}
	if modelOwnedBy(fallback, req.Model) {
		if ok, _ := r.supportsLocked(fallback.Name(), req.Model, reqs); ok {
			return req, true
		}
	}
	for _, target := range r.aliases[req.Model] {
		if !modelOwnedBy(fallback, target) {
			continue
		}
		if ok, _ := r.supportsLocked(fallback.Name(), target, reqs); ok {
			req.Model = target
			return req, true
		}
	}
	for _, model := range fallback.Models() {
		if isPattern(model) {
			continue
		}
		if ok, _ := r.supportsLocked(fallback.Name(), model, reqs); ok {
			req.Model = model
			return req, true
		}
	}
	return req, false
}

func (r *Registry) hasCapabilityTableLocked(providerName string) bool {
	t, ok := r.capabilities[providerName]
	return ok && !t.Empty()
}

func modelOwnedBy(p provider.Provider, model string) bool {
	for _, m := range p.Models() {
		if m == model || matchesModelPattern(m, model) || matchesModelPattern(model, m) {
			return true
		}
	}
	return false
}

func cloneAliases(aliases map[string][]string) map[string][]string {
	if len(aliases) == 0 {
		return nil
	}
	out := make(map[string][]string, len(aliases))
	for model, targets := range aliases {
		out[model] = append([]string(nil), targets...)
	}
	return out
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
