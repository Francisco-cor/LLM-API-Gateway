package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// healthCheckTimeout bounds how long GET /health/providers waits for all
// providers to respond.
const healthCheckTimeout = 10 * time.Second

// HealthHandler serves GET /health, a liveness probe for the gateway itself.
type HealthHandler struct{}

func NewHealthHandler() *HealthHandler {
	return &HealthHandler{}
}

func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// providerStatus describes the outcome of a single provider's health check.
type providerStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// HealthProvidersHandler serves GET /health/providers, pinging every
// configured provider and reporting per-provider status.
type HealthProvidersHandler struct {
	registry *Registry
}

func NewHealthProvidersHandler(registry *Registry) *HealthProvidersHandler {
	return &HealthProvidersHandler{registry: registry}
}

func (h *HealthProvidersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()

	results := make(map[string]providerStatus)
	for _, p := range h.registry.All() {
		if err := p.HealthCheck(ctx); err != nil {
			results[p.Name()] = providerStatus{Status: "unhealthy", Error: err.Error()}
		} else {
			results[p.Name()] = providerStatus{Status: "healthy"}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(results)
}
