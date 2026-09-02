package proxy

import (
	"fmt"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// Registry resolves model names to the Provider that serves them and looks
// up providers by name for fallback. It also supports model_aliases
// (e.g. "gpt-4o" -> ["claude-sonnet-4-6"]) for fallback remapping.
type Registry struct {
	byName   map[string]provider.Provider
	byModel  map[string]provider.Provider
	aliases  map[string][]string
	order    []provider.Provider
}

// NewRegistry builds a Registry from the given providers, indexing each by
// name and by every model it declares.
func NewRegistry(providers []provider.Provider) *Registry {
	return NewRegistryWithAliases(providers, nil)
}

// NewRegistryWithAliases builds a Registry with model alias support.
func NewRegistryWithAliases(providers []provider.Provider, aliases map[string][]string) *Registry {
	r := &Registry{
		byName:  make(map[string]provider.Provider),
		byModel: make(map[string]provider.Provider),
		aliases: aliases,
	}
	for _, p := range providers {
		r.byName[p.Name()] = p
		r.order = append(r.order, p)
		for _, model := range p.Models() {
			r.byModel[model] = p
		}
	}
	return r
}

// Resolve returns the Provider configured to serve model.
func (r *Registry) Resolve(model string) (provider.Provider, error) {
	p, ok := r.byModel[model]
	if !ok {
		return nil, fmt.Errorf("%w: model %q", provider.ErrNoProvider, model)
	}
	return p, nil
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
			if m == t {
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
