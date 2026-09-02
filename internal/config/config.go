package config

import (
	"fmt"
	"os"
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
}

type RateLimitConfig struct {
	Enabled           bool `yaml:"enabled"`
	RequestsPerMinute int  `yaml:"requests_per_minute"`
	Burst             int  `yaml:"burst"`
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
}

type APIKeyConfig struct {
	Key       string     `yaml:"key"`
	Tenant    string     `yaml:"tenant"`
	Scopes    []string   `yaml:"scopes"`
	ExpiresAt *time.Time `yaml:"expires_at"`
}

type ResilienceConfig struct {
	Retry       RetryConfig    `yaml:"retry"`
	Circuit     CircuitConfig  `yaml:"circuit"`
	Hedge       HedgeConfig    `yaml:"hedge"`
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

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	expanded := os.Expand(string(data), func(key string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return "${" + key + "}"
	})
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	setDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
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
		if len(p.Models) == 0 {
			return fmt.Errorf("providers.%s.models must be non-empty", name)
		}
	}
	// fallback_chain must reference known providers
	for _, name := range c.FallbackChain {
		if _, ok := c.Providers[name]; !ok {
			return fmt.Errorf("fallback_chain references unknown provider %q", name)
		}
	}
	// auth validation
	if c.Auth.Enabled && len(c.Auth.Keys) == 0 {
		return fmt.Errorf("auth.enabled is true but no keys configured")
	}
	for i, k := range c.Auth.Keys {
		if k.Key == "" || (len(k.Key) > 3 && k.Key[:2] == "${") {
			// unexpanded env var — treat as missing but error if auth enabled and key empty
			if c.Auth.Enabled {
				return fmt.Errorf("auth.keys[%d].key is required", i)
			}
		}
		if k.ExpiresAt != nil && k.ExpiresAt.IsZero() {
			return fmt.Errorf("auth.keys[%d].expires_at is invalid", i)
		}
	}
	// resilience validation
	if c.Resilience.Retry.MaxAttempts < 0 {
		return fmt.Errorf("resilience.retry.max_attempts must be >= 0")
	}
	if c.Resilience.Circuit.FailureThreshold < 0 {
		return fmt.Errorf("resilience.circuit.failure_threshold must be >= 0")
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
	return nil
}

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
}
