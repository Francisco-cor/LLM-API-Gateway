package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	defaultHealthCheckTimeout = 10 * time.Second
	defaultProviderTimeout    = 3 * time.Second
	defaultReadinessCacheTTL  = 15 * time.Second
)

type HealthOptions struct {
	ReadinessCacheTTL   time.Duration
	CheckTimeout        time.Duration
	ProviderTimeout     time.Duration
	SkipExpensiveChecks bool
}

func DefaultHealthOptions() HealthOptions {
	return HealthOptions{
		ReadinessCacheTTL: defaultReadinessCacheTTL,
		CheckTimeout:      defaultHealthCheckTimeout,
		ProviderTimeout:   defaultProviderTimeout,
	}
}

func normalizeHealthOptions(options HealthOptions) HealthOptions {
	defaults := DefaultHealthOptions()
	if options.ReadinessCacheTTL <= 0 {
		options.ReadinessCacheTTL = defaults.ReadinessCacheTTL
	}
	if options.CheckTimeout <= 0 {
		options.CheckTimeout = defaults.CheckTimeout
	}
	if options.ProviderTimeout <= 0 {
		options.ProviderTimeout = defaults.ProviderTimeout
	}
	return options
}

// HealthHandler serves GET /health, a liveness probe for the gateway itself.
// It also tracks readiness for graceful drain (Fase 10): SetReady(false) makes
// /readyz return 503 so K8s stops routing before Shutdown completes.
type HealthHandler struct {
	startTime time.Time
	ready     atomic.Bool
}

func NewHealthHandler() *HealthHandler {
	h := &HealthHandler{startTime: time.Now()}
	h.ready.Store(true)
	return h
}

// SetReady marks the gateway as ready (true) or draining (false).
func (h *HealthHandler) SetReady(v bool) { h.ready.Store(v) }

// IsReady reports whether gateway is ready to serve traffic.
func (h *HealthHandler) IsReady() bool { return h.ready.Load() }

func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"uptime_seconds": time.Since(h.startTime).Seconds(),
		"start_time":     h.startTime.Format(time.RFC3339),
		"ready":          h.ready.Load(),
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
	registry  *Registry
	health    *HealthHandler
	cacheMu   sync.Mutex
	refresh   sync.Mutex
	optionsMu sync.RWMutex
	options   HealthOptions
	last      readinessSnapshot
}

type readinessSnapshot struct {
	at        time.Time
	healthy   int
	total     int
	providers map[string]providerStatus
}

func NewReadinessHandler(registry *Registry) *ReadinessHandler {
	return NewReadinessHandlerWithOptions(registry, nil, DefaultHealthOptions())
}

// NewReadinessHandlerWithHealth links readiness to HealthHandler ready flag (for graceful drain).
func NewReadinessHandlerWithHealth(registry *Registry, health *HealthHandler) *ReadinessHandler {
	return NewReadinessHandlerWithOptions(registry, health, DefaultHealthOptions())
}

func NewReadinessHandlerWithOptions(registry *Registry, health *HealthHandler, options HealthOptions) *ReadinessHandler {
	return &ReadinessHandler{registry: registry, health: health, options: normalizeHealthOptions(options)}
}

func (h *ReadinessHandler) SetOptions(options HealthOptions) {
	h.optionsMu.Lock()
	h.options = normalizeHealthOptions(options)
	h.optionsMu.Unlock()
	h.cacheMu.Lock()
	h.last = readinessSnapshot{}
	h.cacheMu.Unlock()
}

func (h *ReadinessHandler) getOptions() HealthOptions {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	return h.options
}

func (h *ReadinessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Fase 10: if gateway is draining, return 503 immediately so K8s removes endpoint
	if h.health != nil && !h.health.IsReady() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "draining",
			"ready":  false,
		})
		return
	}
	snapshot, ok := h.cachedSnapshot()
	if !ok {
		snapshot = h.refreshSnapshot(r.Context())
	}

	h.writeReadiness(w, snapshot)
}

func (h *ReadinessHandler) cachedSnapshot() (readinessSnapshot, bool) {
	ttl := h.getOptions().ReadinessCacheTTL
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	if h.last.at.IsZero() || time.Since(h.last.at) >= ttl {
		return readinessSnapshot{}, false
	}
	return cloneReadinessSnapshot(h.last), true
}

func (h *ReadinessHandler) refreshSnapshot(parent context.Context) readinessSnapshot {
	// Collapse concurrent Kubernetes probes into one upstream health fan-out.
	h.refresh.Lock()
	defer h.refresh.Unlock()
	if snapshot, ok := h.cachedSnapshot(); ok {
		return snapshot
	}
	options := h.getOptions()
	ctx, cancel := context.WithTimeout(parent, options.CheckTimeout)
	defer cancel()

	var mu sync.Mutex
	healthy := 0
	results := make(map[string]providerStatus)

	g, ctx := errgroup.WithContext(ctx)
	for _, p := range h.registry.All() {
		p := p
		g.Go(func() error {
			pCtx, pCancel := context.WithTimeout(ctx, options.ProviderTimeout)
			defer pCancel()
			if options.SkipExpensiveChecks && p.Name() == "anthropic" {
				mu.Lock()
				results[p.Name()] = providerStatus{Status: "skipped"}
				healthy++
				mu.Unlock()
				return nil
			}
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
	snapshot := readinessSnapshot{at: time.Now(), healthy: healthy, total: len(h.registry.All()), providers: results}
	h.cacheMu.Lock()
	h.last = cloneReadinessSnapshot(snapshot)
	h.cacheMu.Unlock()
	return snapshot
}

func (h *ReadinessHandler) writeReadiness(w http.ResponseWriter, snapshot readinessSnapshot) {
	w.Header().Set("Content-Type", "application/json")
	if snapshot.healthy == 0 && snapshot.total > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "not_ready",
			"healthy":   0,
			"total":     snapshot.total,
			"providers": snapshot.providers,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ready",
		"healthy":   snapshot.healthy,
		"total":     snapshot.total,
		"providers": snapshot.providers,
	})
}

func cloneReadinessSnapshot(in readinessSnapshot) readinessSnapshot {
	out := in
	out.providers = make(map[string]providerStatus, len(in.providers))
	for name, status := range in.providers {
		out.providers[name] = status
	}
	return out
}

// providerStatus describes the outcome of a single provider's health check.
type providerStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// HealthProvidersHandler serves GET /health/providers, pinging every
// configured provider and reporting per-provider status.
type HealthProvidersHandler struct {
	registry  *Registry
	optionsMu sync.RWMutex
	options   HealthOptions
}

func NewHealthProvidersHandler(registry *Registry) *HealthProvidersHandler {
	return NewHealthProvidersHandlerWithOptions(registry, DefaultHealthOptions())
}

func NewHealthProvidersHandlerWithOptions(registry *Registry, options HealthOptions) *HealthProvidersHandler {
	return &HealthProvidersHandler{registry: registry, options: normalizeHealthOptions(options)}
}

func (h *HealthProvidersHandler) SetOptions(options HealthOptions) {
	h.optionsMu.Lock()
	h.options = normalizeHealthOptions(options)
	h.optionsMu.Unlock()
}

func (h *HealthProvidersHandler) getOptions() HealthOptions {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	return h.options
}

func (h *HealthProvidersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	options := h.getOptions()
	ctx, cancel := context.WithTimeout(r.Context(), options.CheckTimeout)
	defer cancel()

	var mu sync.Mutex
	results := make(map[string]providerStatus)

	g, ctx := errgroup.WithContext(ctx)
	for _, p := range h.registry.All() {
		p := p
		g.Go(func() error {
			pCtx, pCancel := context.WithTimeout(ctx, options.ProviderTimeout)
			defer pCancel()
			if options.SkipExpensiveChecks && p.Name() == "anthropic" {
				mu.Lock()
				results[p.Name()] = providerStatus{Status: "skipped"}
				mu.Unlock()
				return nil
			}
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
