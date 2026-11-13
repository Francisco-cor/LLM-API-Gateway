package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server        ServerConfig              `yaml:"server"`
	Providers     map[string]ProviderConfig `yaml:"providers"`
	FallbackChain []string                  `yaml:"fallback_chain"`
	RateLimit     RateLimitConfig           `yaml:"rate_limit"`
	Logging       LoggingConfig             `yaml:"logging"`
	CORS          CORSConfig                `yaml:"cors"`
	Auth          AuthConfig                `yaml:"auth"`
	Resilience    ResilienceConfig          `yaml:"resilience"`
	ModelAliases  map[string][]string       `yaml:"model_aliases"`
	Cache         CacheConfig               `yaml:"cache"`
	Health        HealthConfig              `yaml:"health"`
	Routing       RoutingConfig             `yaml:"routing"`
	Admin         AdminConfig               `yaml:"admin"`

	// unresolved lists environment variables referenced by ${VAR} that were
	// not set while loading. Validate uses it to fail on critical secrets
	// instead of silently running with the literal placeholder as a value.
	unresolved []string
}

type AdminConfig struct {
	Port          int    `yaml:"port"`
	BindAddress   string `yaml:"bind_address"`
	APIKey        string `yaml:"api_key"`
	AllowInsecure bool   `yaml:"allow_insecure"`
	PprofEnabled  bool   `yaml:"pprof_enabled"`
}

type RoutingConfig struct {
	// Weighted routing per model: model -> list of provider weights.
	// Example: gpt-4o: [{provider: openai, weight: 90}, {provider: anthropic, weight: 10}]
	Weighted map[string][]WeightedEntry `yaml:"weighted"`
}

type WeightedEntry struct {
	Provider string `yaml:"provider"`
	Name     string `yaml:"name"` // alias for provider (compat with PLAN example)
	Weight   int    `yaml:"weight"`
}

func (w WeightedEntry) ProviderName() string {
	if w.Provider != "" {
		return w.Provider
	}
	return w.Name
}

type ServerConfig struct {
	Port         int           `yaml:"port"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
}

type ProviderConfig struct {
	APIKey  string        `yaml:"api_key"`
	BaseURL string        `yaml:"base_url"`
	Timeout time.Duration `yaml:"timeout"`
	Models  []string      `yaml:"models"`
	// AllowPrivate permits a private/loopback/link-local base_url. It exists so
	// local gateways (Ollama, vLLM) remain usable while strict production
	// validation rejects SSRF-shaped provider endpoints by default.
	AllowPrivate bool `yaml:"allow_private"`
}

type RateLimitConfig struct {
	Enabled           bool                `yaml:"enabled"`
	RequestsPerMinute int                 `yaml:"requests_per_minute"`
	Burst             int                 `yaml:"burst"`
	RedisURL          string              `yaml:"redis_url"`
	TokenAware        bool                `yaml:"token_aware"`
	TokensPerMinute   int                 `yaml:"tokens_per_minute"`
	TokenBurst        int                 `yaml:"token_burst"`
	Overrides         []RateLimitOverride `yaml:"overrides"`
	Budget            *BudgetConfig       `yaml:"budget"`
}

type RateLimitOverride struct {
	Tenant       string `yaml:"tenant"`
	ModelPattern string `yaml:"model_pattern"`
	RPM          int    `yaml:"rpm"`
	Burst        int    `yaml:"burst"`
}

type BudgetConfig struct {
	Enabled         bool          `yaml:"enabled"`
	MonthlyTokens   int           `yaml:"monthly_tokens"`
	MonthlyUSD      float64       `yaml:"monthly_usd"`
	CostPerTokenUSD float64       `yaml:"cost_per_token_usd"`
	Pricing         PricingConfig `yaml:"pricing"`
	// RequireSharedStore fails closed when Redis is configured but unreachable,
	// instead of falling back to per-process accounting (which multiplies the
	// quota by the replica count).
	RequireSharedStore bool `yaml:"require_shared_store"`
	// ReservationTTL bounds how long an uncommitted budget reservation holds
	// quota. Zero uses the 10 minute default.
	ReservationTTL time.Duration `yaml:"reservation_ttl"`
}

type PricingConfig struct {
	Currency           string        `yaml:"currency"`
	Version            string        `yaml:"version"`
	UnknownModelPolicy string        `yaml:"unknown_model_policy"`
	Catalog            []PricingRule `yaml:"catalog"`
}

type PricingRule struct {
	Provider                      string  `yaml:"provider"`
	Model                         string  `yaml:"model"`
	InputPerMillionTokensUSD      float64 `yaml:"input_per_million_tokens_usd"`
	OutputPerMillionTokensUSD     float64 `yaml:"output_per_million_tokens_usd"`
	EmbeddingPerMillionTokensUSD  float64 `yaml:"embedding_per_million_tokens_usd"`
	CacheReadPerMillionTokensUSD  float64 `yaml:"cache_read_per_million_tokens_usd"`
	CacheWritePerMillionTokensUSD float64 `yaml:"cache_write_per_million_tokens_usd"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type CORSConfig struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
}

type AuthConfig struct {
	Enabled bool           `yaml:"enabled"`
	Keys    []APIKeyConfig `yaml:"keys"`
	// RequireScopes makes an empty scope list a validation error instead of
	// silently granting full access. Defaults to true; set false only for a
	// deliberate deny-all key.
	RequireScopes *bool `yaml:"require_scopes"`
	// KeyPepper optionally keys the on-disk key hashes with HMAC-SHA256. An
	// empty pepper keeps the historical plain SHA-256 (still not reversible,
	// but vulnerable to precomputation if the hash database leaks).
	KeyPepper string `yaml:"key_pepper"`
}

// ScopesRequired reports whether auth.keys[].scopes must be non-empty. The
// default is fail-closed.
func (a AuthConfig) ScopesRequired() bool {
	return a.RequireScopes == nil || *a.RequireScopes
}

type APIKeyConfig struct {
	Key       string     `yaml:"key"`
	Tenant    string     `yaml:"tenant"`
	Scopes    []string   `yaml:"scopes"`
	ExpiresAt *time.Time `yaml:"expires_at"`
}

type ResilienceConfig struct {
	Retry   RetryConfig   `yaml:"retry"`
	Circuit CircuitConfig `yaml:"circuit"`
	Hedge   HedgeConfig   `yaml:"hedge"`
}

type RetryConfig struct {
	MaxAttempts int           `yaml:"max_attempts"`
	BaseDelay   time.Duration `yaml:"base_delay"`
	MaxDelay    time.Duration `yaml:"max_delay"`
}

type CircuitConfig struct {
	FailureThreshold int           `yaml:"failure_threshold"`
	OpenTimeout      time.Duration `yaml:"open_timeout"`
}

type HedgeConfig struct {
	Enabled bool          `yaml:"enabled"`
	Delay   time.Duration `yaml:"delay"`
}

type CacheConfig struct {
	Enabled                bool          `yaml:"enabled"`
	TTL                    time.Duration `yaml:"ttl"`
	MaxSize                int           `yaml:"max_size"`
	MaxBytes               int64         `yaml:"max_bytes"`
	MaxTTL                 time.Duration `yaml:"max_ttl"`
	SemanticEnabled        bool          `yaml:"semantic_enabled"`
	SemanticThreshold      float64       `yaml:"semantic_threshold"`
	SemanticEmbeddingModel string        `yaml:"semantic_embedding_model"`
	SemanticTopK           int           `yaml:"semantic_top_k"`
	SemanticMaxEntries     int           `yaml:"semantic_max_entries"`
}

type HealthConfig struct {
	ReadinessCacheTTL   time.Duration `yaml:"readiness_cache_ttl"`
	CheckTimeout        time.Duration `yaml:"check_timeout"`
	ProviderTimeout     time.Duration `yaml:"provider_timeout"`
	SkipExpensiveChecks bool          `yaml:"skip_expensive_checks"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	expanded, missing := expandEnvTracked(data)
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parsing config: multiple YAML documents are not supported")
		}
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.unresolved = missing
	setDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// expandEnvTracked expands ${VAR} and $VAR while recording variables that were
// not set. Missing values stay as ${VAR} so validation can point at the exact
// field instead of treating the placeholder as a credential. Substituting an
// empty string would silently turn a missing secret into an empty value.
func expandEnvTracked(data []byte) (string, []string) {
	var missing []string
	seen := make(map[string]bool)
	out := os.Expand(string(data), func(key string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		if !seen[key] {
			seen[key] = true
			missing = append(missing, key)
		}
		return "${" + key + "}"
	})
	return out, missing
}

// UnresolvedEnv returns the environment variables that were referenced by the
// config file but were not set during Load. The slice is a copy.
func (c *Config) UnresolvedEnv() []string {
	return append([]string(nil), c.unresolved...)
}

func (c *Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", c.Server.Port)
	}
	if c.Server.ReadTimeout <= 0 {
		return fmt.Errorf("server.read_timeout must be > 0")
	}
	if c.Server.WriteTimeout <= 0 {
		return fmt.Errorf("server.write_timeout must be > 0")
	}
	allowedLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if c.Logging.Level != "" && !allowedLevels[c.Logging.Level] {
		return fmt.Errorf("logging.level must be one of debug/info/warn/error, got %q", c.Logging.Level)
	}
	if c.Logging.Format != "" && c.Logging.Format != "json" && c.Logging.Format != "text" {
		return fmt.Errorf("logging.format must be json or text, got %q", c.Logging.Format)
	}
	if c.RateLimit.RequestsPerMinute < 0 {
		return fmt.Errorf("rate_limit.requests_per_minute must be >= 0")
	}
	if c.RateLimit.Burst < 0 {
		return fmt.Errorf("rate_limit.burst must be >= 0")
	}
	if c.RateLimit.Enabled && c.RateLimit.RequestsPerMinute == 0 {
		return fmt.Errorf("rate_limit.requests_per_minute must be > 0 when enabled")
	}
	if c.RateLimit.Enabled && c.RateLimit.Burst == 0 {
		return fmt.Errorf("rate_limit.burst must be > 0 when enabled")
	}
	if c.RateLimit.Enabled && c.RateLimit.TokenAware {
		if c.RateLimit.TokensPerMinute <= 0 {
			return fmt.Errorf("rate_limit.tokens_per_minute must be > 0 when token_aware is enabled")
		}
		// A token bucket whose capacity is smaller than a normal prompt rejects
		// every request forever. Require a floor large enough for real traffic.
		if c.RateLimit.TokenBurst < 1024 {
			return fmt.Errorf("rate_limit.token_burst must be >= 1024 when token_aware is enabled, got %d", c.RateLimit.TokenBurst)
		}
	}
	for i, ov := range c.RateLimit.Overrides {
		if ov.RPM < 0 || ov.Burst < 0 {
			return fmt.Errorf("rate_limit.overrides[%d] rpm/burst must be >=0", i)
		}
		if ov.RPM > maxRequestsPerMinute || ov.Burst > maxBurst {
			return fmt.Errorf("rate_limit.overrides[%d] rpm/burst exceeds the supported maximum (%d rpm, %d burst)", i, maxRequestsPerMinute, maxBurst)
		}
	}
	if c.RateLimit.RequestsPerMinute > maxRequestsPerMinute {
		return fmt.Errorf("rate_limit.requests_per_minute must be <= %d, got %d", maxRequestsPerMinute, c.RateLimit.RequestsPerMinute)
	}
	if c.RateLimit.Burst > maxBurst {
		return fmt.Errorf("rate_limit.burst must be <= %d, got %d", maxBurst, c.RateLimit.Burst)
	}
	if c.RateLimit.TokensPerMinute > maxTokensPerMinute {
		return fmt.Errorf("rate_limit.tokens_per_minute must be <= %d, got %d", maxTokensPerMinute, c.RateLimit.TokensPerMinute)
	}
	if c.RateLimit.TokenBurst > maxTokensPerMinute {
		return fmt.Errorf("rate_limit.token_burst must be <= %d, got %d", maxTokensPerMinute, c.RateLimit.TokenBurst)
	}
	if c.RateLimit.Budget != nil && c.RateLimit.Budget.Enabled {
		if c.RateLimit.Budget.MonthlyTokens < 0 {
			return fmt.Errorf("rate_limit.budget.monthly_tokens must be >=0")
		}
		if c.RateLimit.Budget.MonthlyUSD < 0 {
			return fmt.Errorf("rate_limit.budget.monthly_usd must be >=0")
		}
		if c.RateLimit.Budget.CostPerTokenUSD < 0 {
			return fmt.Errorf("rate_limit.budget.cost_per_token_usd must be >=0")
		}
	}
	if c.RateLimit.Budget != nil {
		if c.RateLimit.Budget.ReservationTTL < 0 {
			return fmt.Errorf("rate_limit.budget.reservation_ttl must be >=0")
		}
		p := c.RateLimit.Budget.Pricing
		if p.Currency != "" && p.Currency != "USD" {
			return fmt.Errorf("rate_limit.budget.pricing.currency must be USD, got %q", p.Currency)
		}
		if p.UnknownModelPolicy != "" && p.UnknownModelPolicy != "fallback" && p.UnknownModelPolicy != "reject" {
			return fmt.Errorf("rate_limit.budget.pricing.unknown_model_policy must be fallback or reject")
		}
		for i, rule := range p.Catalog {
			if rule.Provider == "" || rule.Model == "" {
				return fmt.Errorf("rate_limit.budget.pricing.catalog[%d] provider and model are required", i)
			}
			if rule.InputPerMillionTokensUSD < 0 || rule.OutputPerMillionTokensUSD < 0 ||
				rule.EmbeddingPerMillionTokensUSD < 0 || rule.CacheReadPerMillionTokensUSD < 0 ||
				rule.CacheWritePerMillionTokensUSD < 0 {
				return fmt.Errorf("rate_limit.budget.pricing.catalog[%d] rates must be >=0", i)
			}
			if rule.InputPerMillionTokensUSD == 0 && rule.OutputPerMillionTokensUSD == 0 &&
				rule.EmbeddingPerMillionTokensUSD == 0 && rule.CacheReadPerMillionTokensUSD == 0 &&
				rule.CacheWritePerMillionTokensUSD == 0 {
				return fmt.Errorf("rate_limit.budget.pricing.catalog[%d] needs at least one positive rate", i)
			}
		}
	}
	// Providers validation — APIKey may be empty or unexpanded "${...}" (treated as disabled, not an error)
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider must be configured")
	}
	for name, p := range c.Providers {
		isDisabled := p.APIKey == "" || (len(p.APIKey) > 3 && p.APIKey[:2] == "${")
		if p.BaseURL == "" && !isDisabled {
			return fmt.Errorf("providers.%s.base_url is required", name)
		}
		if p.Timeout <= 0 {
			return fmt.Errorf("providers.%s.timeout must be > 0", name)
		}
		// models may be empty -> auto-discovery (Fase 8) will fetch upstream
		if len(p.Models) == 0 && !isDisabled {
			// allow empty for auto-discovery; no error
		}
	}
	// fallback_chain must reference known providers
	for _, name := range c.FallbackChain {
		if _, ok := c.Providers[name]; !ok {
			return fmt.Errorf("fallback_chain references unknown provider %q", name)
		}
	}
	// A provider in the fallback chain with an unresolved key would be skipped
	// at runtime, silently shrinking the failover path. Local development mode
	// (allow_insecure) keeps the historical lenient behavior.
	if !c.Admin.AllowInsecure {
		for _, name := range c.FallbackChain {
			if p := c.Providers[name]; isUnresolvedPlaceholder(p.APIKey) {
				return fmt.Errorf("providers.%s.api_key is unresolved but the provider is in fallback_chain", name)
			}
		}
	}
	// auth validation
	if c.Auth.Enabled && len(c.Auth.Keys) == 0 {
		return fmt.Errorf("auth.enabled is true but no keys configured")
	}
	seenKeys := make(map[string]int, len(c.Auth.Keys))
	usableKeys := 0
	for i, k := range c.Auth.Keys {
		if k.Key == "" || isUnresolvedPlaceholder(k.Key) {
			// unexpanded env var — treat as missing but error if auth enabled and key empty
			if c.Auth.Enabled {
				return fmt.Errorf("auth.keys[%d].key is required (set the referenced environment variable)", i)
			}
			continue
		}
		usableKeys++
		if prev, dup := seenKeys[k.Key]; dup {
			return fmt.Errorf("auth.keys[%d].key duplicates auth.keys[%d].key", i, prev)
		}
		seenKeys[k.Key] = i
		if k.Tenant == "" {
			return fmt.Errorf("auth.keys[%d].tenant is required", i)
		}
		// Short keys are guessable and do not carry enough entropy for a
		// bearer credential. Keep local development usable via allow_insecure.
		if !c.Admin.AllowInsecure && len(k.Key) < 32 {
			return fmt.Errorf("auth.keys[%d].key must be at least 32 characters when auth is enabled (set admin.allow_insecure=true for local development)", i)
		}
		if c.Auth.Enabled && c.Auth.ScopesRequired() && len(k.Scopes) == 0 {
			return fmt.Errorf("auth.keys[%d].scopes is required when auth.enabled (set auth.require_scopes: false to opt out explicitly)", i)
		}
		for _, scope := range k.Scopes {
			if !allowedScope(scope) {
				return fmt.Errorf("auth.keys[%d].scopes contains unknown scope %q (allowed: chat:write, embeddings:write, models:read, admin:write, resource:<name>, *)", i, scope)
			}
		}
		if k.ExpiresAt != nil && k.ExpiresAt.IsZero() {
			return fmt.Errorf("auth.keys[%d].expires_at is invalid", i)
		}
	}
	if c.Auth.Enabled && usableKeys == 0 {
		return fmt.Errorf("auth.enabled is true but no usable keys remain (all keys are empty or unresolved placeholders)")
	}
	// resilience validation
	if c.Resilience.Retry.MaxAttempts < 0 {
		return fmt.Errorf("resilience.retry.max_attempts must be >= 0 (0 means the built-in default)")
	}
	if c.Resilience.Retry.MaxAttempts > maxRetryAttempts {
		return fmt.Errorf("resilience.retry.max_attempts must be <= %d, got %d", maxRetryAttempts, c.Resilience.Retry.MaxAttempts)
	}
	if c.Resilience.Retry.BaseDelay < 0 || c.Resilience.Retry.MaxDelay < 0 {
		return fmt.Errorf("resilience.retry delays must be >= 0")
	}
	if c.Resilience.Retry.BaseDelay > maxRetryDelay || c.Resilience.Retry.MaxDelay > maxRetryDelay {
		return fmt.Errorf("resilience.retry delays must be <= %s", maxRetryDelay)
	}
	if c.Resilience.Circuit.FailureThreshold < 0 {
		return fmt.Errorf("resilience.circuit.failure_threshold must be >= 0 (0 means the built-in default)")
	}
	if c.Resilience.Circuit.FailureThreshold > maxCircuitThreshold {
		return fmt.Errorf("resilience.circuit.failure_threshold must be <= %d, got %d", maxCircuitThreshold, c.Resilience.Circuit.FailureThreshold)
	}
	if c.Resilience.Circuit.OpenTimeout < 0 {
		return fmt.Errorf("resilience.circuit.open_timeout must be >= 0")
	}
	if c.Resilience.Circuit.OpenTimeout > maxRetryDelay {
		return fmt.Errorf("resilience.circuit.open_timeout must be <= %s", maxRetryDelay)
	}
	if c.Resilience.Hedge.Delay < 0 {
		return fmt.Errorf("resilience.hedge.delay must be >= 0")
	}
	if c.Resilience.Hedge.Delay > maxRetryDelay {
		return fmt.Errorf("resilience.hedge.delay must be <= %s", maxRetryDelay)
	}
	// model_aliases validation: aliases must point to existing providers via models map
	for alias, targets := range c.ModelAliases {
		if alias == "" {
			return fmt.Errorf("model_aliases key must be non-empty")
		}
		if len(targets) == 0 {
			return fmt.Errorf("model_aliases[%q] must be non-empty", alias)
		}
	}
	if c.Cache.TTL < 0 {
		return fmt.Errorf("cache.ttl must be >=0")
	}
	if c.Cache.MaxSize < 0 {
		return fmt.Errorf("cache.max_size must be >=0")
	}
	if c.Cache.MaxSize > maxCacheEntries {
		return fmt.Errorf("cache.max_size must be <= %d, got %d", maxCacheEntries, c.Cache.MaxSize)
	}
	if c.Cache.MaxBytes < 0 {
		return fmt.Errorf("cache.max_bytes must be >=0")
	}
	if c.Cache.MaxBytes > maxCacheBytes {
		return fmt.Errorf("cache.max_bytes must be <= %d, got %d", maxCacheBytes, c.Cache.MaxBytes)
	}
	if c.Cache.MaxTTL < 0 {
		return fmt.Errorf("cache.max_ttl must be >=0")
	}
	if c.Cache.MaxTTL > maxCacheTTL {
		return fmt.Errorf("cache.max_ttl must be <= %s, got %s", maxCacheTTL, c.Cache.MaxTTL)
	}
	if c.Cache.SemanticThreshold < 0 || c.Cache.SemanticThreshold > 1 {
		return fmt.Errorf("cache.semantic_threshold must be between 0 and 1")
	}
	if c.Cache.SemanticEnabled && c.Cache.SemanticEmbeddingModel == "" {
		return fmt.Errorf("cache.semantic_embedding_model is required when semantic cache is enabled")
	}
	if c.Cache.SemanticTopK < 0 {
		return fmt.Errorf("cache.semantic_top_k must be >=0")
	}
	if c.Cache.SemanticMaxEntries < 0 {
		return fmt.Errorf("cache.semantic_max_entries must be >=0")
	}
	if c.Health.ReadinessCacheTTL < 0 || c.Health.CheckTimeout < 0 || c.Health.ProviderTimeout < 0 {
		return fmt.Errorf("health timeouts must be >=0")
	}
	for model, entries := range c.Routing.Weighted {
		if model == "" {
			return fmt.Errorf("routing.weighted key must be non-empty")
		}
		if len(entries) == 0 {
			return fmt.Errorf("routing.weighted[%q] must be non-empty", model)
		}
		total := 0
		for i, e := range entries {
			name := e.ProviderName()
			if name == "" {
				return fmt.Errorf("routing.weighted[%q][%d] provider/name required", model, i)
			}
			if _, ok := c.Providers[name]; !ok {
				return fmt.Errorf("routing.weighted[%q][%d] references unknown provider %q", model, i, name)
			}
			if e.Weight <= 0 {
				return fmt.Errorf("routing.weighted[%q][%d] weight must be >0", model, i)
			}
			total += e.Weight
		}
		if total == 0 {
			return fmt.Errorf("routing.weighted[%q] total weight must be >0", model)
		}
	}
	if c.Admin.Port != 0 && (c.Admin.Port < 1 || c.Admin.Port > 65535) {
		return fmt.Errorf("admin.port must be between 1 and 65535, got %d", c.Admin.Port)
	}
	if c.Admin.Port != 0 && c.Admin.Port == c.Server.Port {
		return fmt.Errorf("admin.port must differ from server.port (both %d)", c.Admin.Port)
	}
	// The control plane can rewrite configuration and expose pprof. It must be
	// fail-closed unless the operator explicitly opted into local development
	// mode. Unexpanded ${VAR} placeholders count as missing secrets.
	if !c.Admin.AllowInsecure {
		if c.Admin.APIKey == "" || isUnresolvedPlaceholder(c.Admin.APIKey) {
			return fmt.Errorf("admin.api_key is required unless admin.allow_insecure is true: set ADMIN_API_KEY or enable insecure local mode")
		}
	}
	return nil
}

// isUnresolvedPlaceholder reports whether os.Expand left a ${VAR} reference,
// which happens when the environment variable is not set.
func isUnresolvedPlaceholder(v string) bool {
	return len(v) > 3 && v[:2] == "${" && v[len(v)-1] == '}'
}

// Supported upper bounds. They keep a typo (or an admin API patch) from
// turning a limit into an effective denial of service or an unbounded cache.
const (
	maxRequestsPerMinute = 1_000_000
	maxBurst             = 1_000_000
	maxTokensPerMinute   = 1_000_000_000
	maxRetryAttempts     = 10
	maxRetryDelay        = 5 * time.Minute
	maxCircuitThreshold  = 10_000
	maxCacheEntries      = 1_000_000
	maxCacheBytes        = int64(1) << 34 // 16 GiB
	maxCacheTTL          = 24 * time.Hour
)

// allowedScope implements the scope allowlist. Unknown scopes are rejected so
// a typo cannot be mistaken for a meaningful permission.
func allowedScope(scope string) bool {
	switch scope {
	case "chat:write", "embeddings:write", "models:read", "admin:write", "*":
		return true
	}
	return strings.HasPrefix(scope, "resource:") && len(scope) > len("resource:")
}

// ValidateReload rejects fields that cannot be applied to a running process.
// applyConfig calls it before touching live state so the operator gets a clear
// error instead of a silent partial reload.
func (c *Config) ValidateReload(prev *Config) error {
	if prev == nil {
		return nil
	}
	if c.Server.Port != prev.Server.Port {
		return fmt.Errorf("server.port requires restart (got %d, running %d)", c.Server.Port, prev.Server.Port)
	}
	if c.Server.ReadTimeout != prev.Server.ReadTimeout || c.Server.WriteTimeout != prev.Server.WriteTimeout {
		return fmt.Errorf("server.read_timeout/write_timeout require restart")
	}
	if c.Admin.Port != prev.Admin.Port || c.Admin.BindAddress != prev.Admin.BindAddress {
		return fmt.Errorf("admin.bind_address/port require restart")
	}
	if c.Admin.PprofEnabled != prev.Admin.PprofEnabled {
		return fmt.Errorf("admin.pprof_enabled requires restart")
	}
	return nil
}

// ValidateStrict applies the production profile checks on top of Validate.
// main runs it when --strict is passed or GATEWAY_ENV=prod.
func (c *Config) ValidateStrict() error {
	if err := c.Validate(); err != nil {
		return err
	}
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	if !c.Auth.Enabled {
		add("auth.enabled must be true in strict mode")
	}
	if c.Admin.APIKey == "" || isUnresolvedPlaceholder(c.Admin.APIKey) {
		add("admin.api_key is required in strict mode")
	}
	if c.Admin.AllowInsecure {
		add("admin.allow_insecure must be false in strict mode")
	}
	if c.Admin.PprofEnabled {
		add("admin.pprof_enabled must be false in strict mode")
	}
	for _, origin := range c.CORS.AllowedOrigins {
		if origin == "*" {
			add("cors.allowed_origins must not contain \"*\" in strict mode")
		}
	}
	for name, p := range c.Providers {
		if err := validateProviderBaseURL(name, p); err != nil {
			errs = append(errs, err)
		}
	}
	if c.RateLimit.Enabled && (c.RateLimit.RedisURL == "" || isUnresolvedPlaceholder(c.RateLimit.RedisURL)) {
		add("rate_limit.redis_url is required in strict mode: rate limits must be shared across replicas")
	}
	if c.RateLimit.Budget != nil && c.RateLimit.Budget.Enabled {
		if c.RateLimit.RedisURL == "" || isUnresolvedPlaceholder(c.RateLimit.RedisURL) {
			add("rate_limit.redis_url is required when budget is enabled in strict mode")
		}
		if !c.RateLimit.Budget.RequireSharedStore {
			add("rate_limit.budget.require_shared_store must be true in strict mode (multi-replica quota safety)")
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("strict validation failed:\n%w", errors.Join(errs...))
	}
	return nil
}

func validateProviderBaseURL(name string, p ProviderConfig) error {
	if p.BaseURL == "" || isUnresolvedPlaceholder(p.BaseURL) {
		return nil // a disabled provider is allowed; Validate covers enabled ones
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("providers.%s.base_url is not a valid URL", name)
	}
	allowedScheme := u.Scheme == "https" || (p.AllowPrivate && u.Scheme == "http")
	if !allowedScheme {
		return fmt.Errorf("providers.%s.base_url must use https in strict mode (allow_private permits http only for a trusted local backend)", name)
	}
	host := strings.ToLower(u.Hostname())
	// Metadata endpoints stay blocked even for trusted private backends: they
	// hand out cloud credentials, not model completions.
	for _, meta := range []string{"metadata", "metadata.google.internal", "instance-data", "instance-data.ec2.internal"} {
		if host == meta {
			return fmt.Errorf("providers.%s.base_url points at a cloud metadata host %q; this is never allowed", name, host)
		}
	}
	if ip := net.ParseIP(host); ip != nil && isMetadataIP(ip) {
		return fmt.Errorf("providers.%s.base_url points at a cloud metadata address %q; this is never allowed", name, host)
	}
	if p.AllowPrivate {
		return nil
	}
	if host == "localhost" {
		return fmt.Errorf("providers.%s.base_url points at a local/metadata host %q; set allow_private: true only for a trusted private backend", name, host)
	}
	if strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("providers.%s.base_url points at a local host %q; set allow_private: true only for a trusted private backend", name, host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("providers.%s.base_url points at a private/loopback/link-local address %q; set allow_private: true only for a trusted private backend", name, host)
		}
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return isMetadataIP(ip)
}

// isMetadataIP matches cloud instance-metadata addresses that are not
// link-local on every provider.
func isMetadataIP(ip net.IP) bool {
	metadata := []string{"169.254.169.254", "100.100.100.200", "fd00:ec2::254"}
	for _, m := range metadata {
		if ip.Equal(net.ParseIP(m)) {
			return true
		}
	}
	return false
}

// setDefaults fills zero values with the built-in default. Because YAML scalars
// cannot distinguish "absent" from an explicit 0, 0 always means "use the
// default" for the fields handled here (including retry.max_attempts and
// rate_limit.burst). Negative values are rejected by Validate.
func setDefaults(cfg *Config) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.ReadTimeout == 0 {
		cfg.Server.ReadTimeout = 30 * time.Second
	}
	if cfg.Server.WriteTimeout == 0 {
		cfg.Server.WriteTimeout = 60 * time.Second
	}
	for name, p := range cfg.Providers {
		if p.Timeout == 0 {
			p.Timeout = 30 * time.Second
			cfg.Providers[name] = p
		}
	}
	if cfg.RateLimit.RequestsPerMinute == 0 {
		cfg.RateLimit.RequestsPerMinute = 60
	}
	if cfg.RateLimit.Burst == 0 {
		cfg.RateLimit.Burst = 10
	}
	if cfg.RateLimit.TokenAware {
		if cfg.RateLimit.TokensPerMinute == 0 {
			// Assume an average request of 1000 tokens so the default token
			// budget stays proportional to the request limit.
			cfg.RateLimit.TokensPerMinute = cfg.RateLimit.RequestsPerMinute * 1000
			if cfg.RateLimit.TokensPerMinute < 10000 {
				cfg.RateLimit.TokensPerMinute = 10000
			}
		}
		if cfg.RateLimit.TokenBurst == 0 {
			cfg.RateLimit.TokenBurst = cfg.RateLimit.TokensPerMinute / 10
			if cfg.RateLimit.TokenBurst < 1024 {
				cfg.RateLimit.TokenBurst = 1024
			}
		}
	}
	if cfg.RateLimit.Budget != nil && cfg.RateLimit.Budget.CostPerTokenUSD == 0 {
		cfg.RateLimit.Budget.CostPerTokenUSD = 0.00001
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}
	if cfg.Resilience.Retry.MaxAttempts == 0 {
		cfg.Resilience.Retry.MaxAttempts = 3
	}
	if cfg.Resilience.Retry.BaseDelay == 0 {
		cfg.Resilience.Retry.BaseDelay = 200 * time.Millisecond
	}
	if cfg.Resilience.Retry.MaxDelay == 0 {
		cfg.Resilience.Retry.MaxDelay = 2 * time.Second
	}
	if cfg.Resilience.Circuit.FailureThreshold == 0 {
		cfg.Resilience.Circuit.FailureThreshold = 5
	}
	if cfg.Resilience.Circuit.OpenTimeout == 0 {
		cfg.Resilience.Circuit.OpenTimeout = 30 * time.Second
	}
	if cfg.Resilience.Hedge.Delay == 0 {
		cfg.Resilience.Hedge.Delay = 300 * time.Millisecond
	}
	if cfg.Cache.TTL == 0 {
		cfg.Cache.TTL = 5 * time.Minute
	}
	if cfg.Cache.MaxSize == 0 {
		cfg.Cache.MaxSize = 1000
	}
	if cfg.Cache.SemanticThreshold == 0 {
		cfg.Cache.SemanticThreshold = 0.97
	}
	if cfg.Cache.SemanticTopK == 0 {
		cfg.Cache.SemanticTopK = 3
	}
	if cfg.Cache.SemanticMaxEntries == 0 {
		cfg.Cache.SemanticMaxEntries = 1000
	}
	if cfg.Health.ReadinessCacheTTL == 0 {
		cfg.Health.ReadinessCacheTTL = 15 * time.Second
	}
	if cfg.Health.CheckTimeout == 0 {
		cfg.Health.CheckTimeout = 10 * time.Second
	}
	if cfg.Health.ProviderTimeout == 0 {
		cfg.Health.ProviderTimeout = 3 * time.Second
	}
	if cfg.Admin.Port == 0 {
		cfg.Admin.Port = 8081
	}
	if cfg.Admin.BindAddress == "" {
		// The admin surface must not be reachable from other hosts by default.
		cfg.Admin.BindAddress = "127.0.0.1"
	}
	if cfg.Admin.APIKey == "" {
		// allow env var ADMIN_API_KEY as fallback
		if v := os.Getenv("ADMIN_API_KEY"); v != "" {
			cfg.Admin.APIKey = v
		}
	}
	if cfg.Auth.KeyPepper == "" {
		// Optional HMAC pepper for API key hashes at rest. Changing it
		// invalidates existing hashes; keys are re-hashed on reload, so no
		// on-disk migration is needed.
		cfg.Auth.KeyPepper = os.Getenv("AUTH_KEY_PEPPER")
	}
}

// Clone returns a deep copy via YAML round-trip (used for atomic swap + rollback).
func (c *Config) Clone() *Config {
	data, _ := yaml.Marshal(c)
	var out Config
	_ = yaml.Unmarshal(data, &out)
	return &out
}

// Watch polls the config file for changes and calls onChange with the new
// validated config. It respects context cancellation and polls every interval
// (default 1s) checking mtime.
//
// The mtime is only advanced after onChange succeeds, so a failed apply is
// retried with backoff instead of dropping the change. Writes are debounced
// without blocking the poll ticker.
func Watch(ctx context.Context, path string, interval time.Duration, onChange func(*Config) error) error {
	return WatchWithErrorHandler(ctx, path, interval, onChange, nil)
}

// WatchWithErrorHandler is Watch plus an error hook for load/apply failures.
// The watcher keeps retrying; onError is for logging by the caller.
func WatchWithErrorHandler(ctx context.Context, path string, interval time.Duration, onChange func(*Config) error, onError func(error)) error {
	if interval <= 0 {
		interval = time.Second
	}
	var lastMod time.Time
	if st, err := os.Stat(path); err == nil {
		lastMod = st.ModTime()
	}
	report := func(err error) {
		if err != nil && onError != nil {
			onError(err)
		}
	}

	const debounce = 100 * time.Millisecond
	var (
		candidate time.Time
		timer     *time.Timer
		timerC    <-chan time.Time
		backoff   time.Duration
	)
	schedule := func(d time.Duration) {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer = nil
		}
		timer = time.NewTimer(d)
		timerC = timer.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			mod := st.ModTime()
			if !mod.After(lastMod) {
				continue
			}
			if !candidate.IsZero() && !mod.After(candidate) {
				continue
			}
			// New content: debounce briefly so a partially written file is
			// not parsed, and reset the retry backoff.
			candidate = mod
			backoff = 0
			schedule(debounce)
		case <-timerC:
			timerC = nil
			cfg, err := Load(path)
			if err != nil {
				report(fmt.Errorf("watcher: load %s: %w", path, err))
				backoff = nextBackoff(backoff, interval)
				schedule(backoff)
				continue
			}
			if err := onChange(cfg); err != nil {
				report(fmt.Errorf("watcher: apply %s: %w", path, err))
				backoff = nextBackoff(backoff, interval)
				schedule(backoff)
				continue
			}
			lastMod = candidate
			candidate = time.Time{}
			backoff = 0
		}
	}
}

func nextBackoff(current, interval time.Duration) time.Duration {
	const maxBackoff = 10 * time.Second
	if current <= 0 {
		if interval > maxBackoff {
			return maxBackoff
		}
		return interval
	}
	next := current * 2
	if next > maxBackoff {
		next = maxBackoff
	}
	return next
}
