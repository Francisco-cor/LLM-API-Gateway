package ratelimit

import (
	"strings"

	"github.com/fcordero/llm-api-gateway/internal/config"
)

// OverrideStore resolves per-tenant/model RPM overrides.
type OverrideStore struct {
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

// Resolve returns effective RPM/Burst for tenant+model.
func (o *OverrideStore) Resolve(tenant, model string) (rpm, burst int) {
	rpm = o.defaultRPM
	burst = o.defaultBurst
	for _, ov := range o.overrides {
		tenantMatch := ov.Tenant == "" || ov.Tenant == tenant || ov.Tenant == "*"
		modelMatch := ov.ModelPattern == "" || ov.ModelPattern == "*" || matchPattern(ov.ModelPattern, model)
		if tenantMatch && modelMatch {
			if ov.RPM > 0 {
				rpm = ov.RPM
			}
			if ov.Burst > 0 {
				burst = ov.Burst
			}
		}
	}
	return
}

func matchPattern(pattern, s string) bool {
	if pattern == s {
		return true
	}
	if strings.Contains(pattern, "*") {
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			return strings.HasPrefix(s, prefix)
		}
		if strings.HasPrefix(pattern, "*") {
			suffix := strings.TrimPrefix(pattern, "*")
			return strings.HasSuffix(s, suffix)
		}
	}
	// fallback to exact
	return false
}
