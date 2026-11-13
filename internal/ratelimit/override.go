package ratelimit

import (
	"path"
	"strings"
	"sync"

	"github.com/fcordero/llm-api-gateway/internal/config"
)

// OverrideStore resolves per-tenant/model RPM overrides.
type OverrideStore struct {
	mu           sync.RWMutex
	defaultRPM   int
	defaultBurst int
	overrides    []config.RateLimitOverride
}

func NewOverrideStore(cfg config.RateLimitConfig) *OverrideStore {
	return &OverrideStore{
		defaultRPM:   cfg.RequestsPerMinute,
		defaultBurst: cfg.Burst,
		overrides:    cfg.Overrides,
	}
}

// Reload atomically updates overrides from new config (for hot-reload).
func (o *OverrideStore) Reload(cfg config.RateLimitConfig) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.defaultRPM = cfg.RequestsPerMinute
	o.defaultBurst = cfg.Burst
	o.overrides = cfg.Overrides
}

// Resolve returns effective RPM/Burst for tenant+model. The most specific
// rule wins deterministically: tenant specificity outweighs model
// specificity, and exact/pattern matches outweigh wildcards. Rules do not
// inherit individual fields from lower-priority matches: an RPM-only rule
// keeps the default burst.
func (o *OverrideStore) Resolve(tenant, model string) (rpm, burst int) {
	rpm, burst, _ = o.ResolveScoped(tenant, model)
	return rpm, burst
}

// ResolveScoped is Resolve plus whether an override rule actually matched.
// Callers use matched to isolate buckets per override scope; a global limit
// remains global across models.
func (o *OverrideStore) ResolveScoped(tenant, model string) (rpm, burst int, matched bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	rpm = o.defaultRPM
	burst = o.defaultBurst
	bestScore := -1
	bestSpec := -1
	var best *config.RateLimitOverride
	for i := range o.overrides {
		ov := &o.overrides[i]
		tenantMatch := ov.Tenant == "" || ov.Tenant == tenant || ov.Tenant == "*"
		modelMatch := ov.ModelPattern == "" || ov.ModelPattern == "*" || matchPattern(ov.ModelPattern, model)
		if !tenantMatch || !modelMatch {
			continue
		}
		score := matchSpecificity(ov.Tenant, tenant)*10 + matchSpecificity(ov.ModelPattern, model)
		spec := len(ov.Tenant) + len(ov.ModelPattern)
		if score > bestScore || (score == bestScore && spec > bestSpec) {
			bestScore = score
			bestSpec = spec
			best = ov
		}
	}
	if best == nil {
		return rpm, burst, false
	}
	if best.RPM > 0 {
		rpm = best.RPM
	}
	if best.Burst > 0 {
		burst = best.Burst
	}
	return rpm, burst, true
}

func matchPattern(pattern, s string) bool {
	if pattern == s {
		return true
	}
	if strings.Contains(pattern, "*") {
		matched, err := path.Match(pattern, s)
		return err == nil && matched
	}
	// fallback to exact
	return false
}

func matchSpecificity(pattern, value string) int {
	if pattern == "" || pattern == "*" {
		return 0
	}
	if pattern == value {
		return 2
	}
	return 1
}
