package proxy

import (
	"fmt"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// Registry resolves model names to the Provider that serves them and looks
// up providers by name for fallback.
type Registry struct {
	byName  map[string]provider.Provider
	byModel map[string]provider.Provider
	order   []provider.Provider
}

// NewRegistry builds a Registry from the given providers, indexing each by
// name and by every model it declares.
func NewRegistry(providers []provider.Provider) *Registry {
	r := &Registry{
		byName:  make(map[string]provider.Provider),
		byModel: make(map[string]provider.Provider),
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
		return nil, fmt.Errorf("no provider configured for model %q", model)
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
