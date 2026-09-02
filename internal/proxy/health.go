package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// healthCheckTimeout bounds how long GET /health/providers waits for all
// providers to respond.
const healthCheckTimeout = 10 * time.Second

// perProviderTimeout bounds each individual provider health check.
const perProviderTimeout = 3 * time.Second

// HealthHandler serves GET /health, a liveness probe for the gateway itself.
type HealthHandler struct {
	startTime time.Time
}

func NewHealthHandler() *HealthHandler {
	return &HealthHandler{startTime: time.Now()}
}

func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"uptime_seconds": time.Since(time.Now().Add(-time.Since(h.startTime))).Seconds(),
		"start_time":     h.startTime.Format(time.RFC3339),
	})
}

// LivenessHandler is GET /livez (k8s liveness).
type LivenessHandler struct {
	health *HealthHandler
}

func NewLivenessHandler(health *HealthHandler) *LivenessHandler {
	return &LivenessHandler{health: health}
}

func (h *LivenessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.health.ServeHTTP(w, r)
}

// ReadinessHandler is GET /readyz (k8s readiness) - checks providers are at least one healthy.
type ReadinessHandler struct {
	registry *Registry
}

func NewReadinessHandler(registry *Registry) *ReadinessHandler {
	return &ReadinessHandler{registry: registry}
}

func (h *ReadinessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()

	var mu sync.Mutex
	healthy := 0
	results := make(map[string]providerStatus)

	g, ctx := errgroup.WithContext(ctx)
	for _, p := range h.registry.All() {
		p := p
		g.Go(func() error {
			pCtx, pCancel := context.WithTimeout(ctx, perProviderTimeout)
			defer pCancel()
			err := p.HealthCheck(pCtx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results[p.Name()] = providerStatus{Status: "unhealthy", Error: err.Error()}
			} else {
				results[p.Name()] = providerStatus{Status: "healthy"}
				healthy++
			}
			return nil
		})
	}
	_ = g.Wait()

	w.Header().Set("Content-Type", "application/json")
	if healthy == 0 && len(h.registry.All()) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "not_ready",
			"healthy":   0,
			"total":     len(h.registry.All()),
			"providers": results,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ready",
		"healthy":   healthy,
		"total":     len(h.registry.All()),
		"providers": results,
	})
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

	var mu sync.Mutex
	results := make(map[string]providerStatus)

	g, ctx := errgroup.WithContext(ctx)
	for _, p := range h.registry.All() {
		p := p
		g.Go(func() error {
			pCtx, pCancel := context.WithTimeout(ctx, perProviderTimeout)
			defer pCancel()
			err := p.HealthCheck(pCtx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results[p.Name()] = providerStatus{Status: "unhealthy", Error: err.Error()}
			} else {
				results[p.Name()] = providerStatus{Status: "healthy"}
			}
			return nil
		})
	}
	_ = g.Wait()

	// Determine overall status
	overall := "healthy"
	for _, v := range results {
		if v.Status != "healthy" {
			overall = "degraded"
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	// Backwards compat: top-level map with per-provider keys plus "overall" if degraded
	if overall == "degraded" {
		out := make(map[string]any, len(results)+1)
		for k, v := range results {
			out[k] = v
		}
		out["overall"] = overall
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	_ = json.NewEncoder(w).Encode(results)
}
