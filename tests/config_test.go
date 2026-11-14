package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/config"
)

func TestConfig_LoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  port: 8080
  read_timeout: 1s
  write_timeout: 1s
providers:
  openai:
    api_key: test-key
    base_url: http://localhost
    timeout: 1s
    models: [gpt-4o]
unknown_setting: true
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("config with unknown field was accepted")
	}
}

const strongKey = "0123456789abcdef0123456789abcdef"

func loadConfig(t *testing.T, content string) (*config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return config.Load(path)
}

// baseConfigYAML is a minimal valid config with an explicit admin key so it
// passes the fail-closed admin validation unless a test overrides that.
func baseConfigYAML(extra string) string {
	return `
server:
  port: 8080
  read_timeout: 5s
  write_timeout: 5s
providers:
  openai:
    api_key: provider-test-key
    base_url: https://api.openai.com/v1
    timeout: 5s
    models: [gpt-4o]
fallback_chain: [openai]
admin:
  port: 8081
  api_key: admin-test-key
  allow_insecure: false
logging:
  level: info
  format: json
` + extra
}

func TestConfig_ProviderCapabilities(t *testing.T) {
	content := strings.Replace(baseConfigYAML(""), "    models: [gpt-4o]", `    models: [gpt-4o, text-embedding-3-small]
    capabilities:
      default:
        vision: false
      "gpt-4o":
        tools: true
      "text-embedding-*":
        chat: false
        embeddings: true
      "gpt-4o-mini":
        context_window: 128000`, 1)
	cfg, err := loadConfig(t, content)
	if err != nil {
		t.Fatalf("load config with capabilities: %v", err)
	}
	caps := cfg.Providers["openai"].Capabilities
	if len(caps) != 4 {
		t.Fatalf("capabilities keys = %d, want 4", len(caps))
	}
	if caps["gpt-4o"].Tools == nil || !*caps["gpt-4o"].Tools {
		t.Error("gpt-4o tools override not parsed")
	}
	if caps["default"].Vision == nil || *caps["default"].Vision {
		t.Error("default vision override not parsed")
	}
	if caps["gpt-4o-mini"].ContextWindow == nil || *caps["gpt-4o-mini"].ContextWindow != 128000 {
		t.Error("context_window not parsed")
	}
}

func TestConfig_ProviderCapabilitiesRejectInvalidValues(t *testing.T) {
	for name, block := range map[string]string{
		"empty key":       "\"\": {chat: true}",
		"negative window": "\"gpt-4o\": {context_window: -1}",
		"negative output": "\"gpt-4o\": {max_output: -5}",
	} {
		t.Run(name, func(t *testing.T) {
			content := strings.Replace(baseConfigYAML(""), "    models: [gpt-4o]", "    models: [gpt-4o]\n    capabilities:\n      "+block, 1)
			if _, err := loadConfig(t, content); err == nil {
				t.Fatalf("invalid capabilities accepted: %s", block)
			}
		})
	}
}

func TestConfig_IdempotencyDefaultsAndOverrides(t *testing.T) {
	cfg, err := loadConfig(t, baseConfigYAML(`
idempotency:
  enabled: true
  ttl: 30s
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Idempotency.Enabled || cfg.Idempotency.TTL != 30*time.Second {
		t.Fatalf("idempotency = %+v", cfg.Idempotency)
	}
	if cfg.Idempotency.MaxEntries == 0 || cfg.Idempotency.MaxBytes == 0 || cfg.Idempotency.EntryMaxBytes == 0 {
		t.Fatalf("idempotency defaults not applied: %+v", cfg.Idempotency)
	}

	invalid := strings.Replace(baseConfigYAML(""), "logging:", `idempotency:
  max_bytes: 10
  entry_max_bytes: 100
logging:`, 1)
	if _, err := loadConfig(t, invalid); err == nil {
		t.Fatal("entry_max_bytes > max_bytes was accepted")
	}
}

func TestConfig_AdminKeyRequiredUnlessInsecure(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "")
	content := strings.Replace(baseConfigYAML(""), "  api_key: admin-test-key\n", "", 1)
	if _, err := loadConfig(t, content); err == nil {
		t.Fatal("missing admin.api_key was accepted with allow_insecure=false")
	}

	insecure := `
server:
  port: 8080
  read_timeout: 5s
  write_timeout: 5s
providers:
  openai:
    api_key: provider-test-key
    base_url: https://api.openai.com/v1
    timeout: 5s
    models: [gpt-4o]
fallback_chain: [openai]
admin:
  port: 8081
  allow_insecure: true
`
	if _, err := loadConfig(t, insecure); err != nil {
		t.Fatalf("local development config with allow_insecure was rejected: %v", err)
	}
}

func TestConfig_UnresolvedCriticalSecretsFail(t *testing.T) {
	admin := strings.Replace(baseConfigYAML(""),
		"api_key: admin-test-key", "api_key: ${GATEWAY_TEST_UNRESOLVED_ADMIN_9f3}", 1)
	if _, err := loadConfig(t, admin); err == nil {
		t.Fatal("unresolved admin api_key was accepted")
	}

	auth := baseConfigYAML(`
auth:
  enabled: true
  keys:
    - key: ${GATEWAY_TEST_UNRESOLVED_AUTH_9f3}
      tenant: default
      scopes: [chat:write]
`)
	if _, err := loadConfig(t, auth); err == nil {
		t.Fatal("unresolved auth key was accepted with auth enabled")
	}

	provider := strings.Replace(baseConfigYAML(""),
		"api_key: provider-test-key", "api_key: ${GATEWAY_TEST_UNRESOLVED_PROVIDER_9f3}", 1)
	if _, err := loadConfig(t, provider); err == nil {
		t.Fatal("unresolved provider key in fallback_chain was accepted")
	}

	// allow_insecure keeps local development lenient, but the unresolved
	// variable is still tracked for observability.
	dev := strings.Replace(provider, "allow_insecure: false", "allow_insecure: true", 1)
	cfg, err := loadConfig(t, dev)
	if err != nil {
		t.Fatalf("dev config with unresolved provider key rejected: %v", err)
	}
	found := false
	for _, name := range cfg.UnresolvedEnv() {
		if name == "GATEWAY_TEST_UNRESOLVED_PROVIDER_9f3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("UnresolvedEnv() = %v, want the provider variable", cfg.UnresolvedEnv())
	}
}

func TestConfig_EnvExpansion(t *testing.T) {
	t.Setenv("GATEWAY_TEST_PROVIDER_KEY_9f3", "expanded-provider-key")
	content := strings.Replace(baseConfigYAML(""),
		"api_key: provider-test-key", "api_key: ${GATEWAY_TEST_PROVIDER_KEY_9f3}", 1)
	cfg, err := loadConfig(t, content)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Providers["openai"].APIKey; got != "expanded-provider-key" {
		t.Fatalf("expanded provider key = %q", got)
	}
	if len(cfg.UnresolvedEnv()) != 0 {
		t.Fatalf("UnresolvedEnv() = %v, want none", cfg.UnresolvedEnv())
	}
}

func TestConfig_AuthKeyValidation(t *testing.T) {
	authWith := func(keys string) string {
		return baseConfigYAML(`
auth:
  enabled: true
  keys:
` + keys)
	}

	cases := []struct {
		name string
		keys string
		want string
	}{
		{
			name: "duplicate",
			keys: "    - key: " + strongKey + "\n      tenant: a\n      scopes: [chat:write]\n" +
				"    - key: " + strongKey + "\n      tenant: b\n      scopes: [chat:write]\n",
			want: "duplicates",
		},
		{
			name: "empty tenant",
			keys: "    - key: " + strongKey + "\n      scopes: [chat:write]\n",
			want: "tenant is required",
		},
		{
			name: "short key",
			keys: "    - key: short-key\n      tenant: a\n      scopes: [chat:write]\n",
			want: "at least 32 characters",
		},
		{
			name: "missing scopes",
			keys: "    - key: " + strongKey + "\n      tenant: a\n",
			want: "scopes is required",
		},
		{
			name: "unknown scope",
			keys: "    - key: " + strongKey + "\n      tenant: a\n      scopes: [chat:read]\n",
			want: "unknown scope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadConfig(t, authWith(tc.keys)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}

	valid := authWith("    - key: " + strongKey + "\n      tenant: a\n      scopes: [chat:write, models:read, resource:team-a]\n")
	if _, err := loadConfig(t, valid); err != nil {
		t.Fatalf("valid scopes rejected: %v", err)
	}
}

func TestConfig_RequireScopesCanBeDisabled(t *testing.T) {
	keys := "    - key: " + strongKey + "\n      tenant: a\n"
	if _, err := loadConfig(t, baseConfigYAML(`
auth:
  enabled: true
  keys:
`+keys)); err == nil {
		t.Fatal("empty scopes accepted with default require_scopes")
	}
	if _, err := loadConfig(t, baseConfigYAML(`
auth:
  enabled: true
  require_scopes: false
  keys:
`+keys)); err != nil {
		t.Fatalf("require_scopes: false was rejected: %v", err)
	}
}

func TestConfig_ValidateStrictRejectsInsecureProductionSettings(t *testing.T) {
	cfg, err := loadConfig(t, baseConfigYAML(`
rate_limit:
  enabled: true
  requests_per_minute: 60
  burst: 10
auth:
  enabled: true
  keys:
    - key: `+strongKey+`
      tenant: default
      scopes: [chat:write]
`))
	if err != nil {
		t.Fatalf("load base: %v", err)
	}
	// Add the remaining strict requirements so we can mutate one at a time.
	cfg.RateLimit.RedisURL = "redis://redis:6379/0"
	if err := cfg.ValidateStrict(); err != nil {
		t.Fatalf("strict validation of a valid production config failed: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"auth disabled", func(c *config.Config) { c.Auth.Enabled = false }, "auth.enabled"},
		{"admin key missing", func(c *config.Config) { c.Admin.APIKey = "" }, "admin.api_key"},
		{"allow insecure", func(c *config.Config) { c.Admin.AllowInsecure = true }, "allow_insecure"},
		{"pprof", func(c *config.Config) { c.Admin.PprofEnabled = true }, "pprof_enabled"},
		{"cors wildcard", func(c *config.Config) { c.CORS.AllowedOrigins = []string{"*"} }, "cors.allowed_origins"},
		{"redis missing", func(c *config.Config) { c.RateLimit.RedisURL = "" }, "redis_url"},
		{"budget shared store", func(c *config.Config) {
			c.RateLimit.Budget = &config.BudgetConfig{Enabled: true, MonthlyTokens: 1, RequireSharedStore: false}
		}, "require_shared_store"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := cfg.Clone()
			tc.mutate(mutated)
			err := mutated.ValidateStrict()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("strict error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestConfig_ValidateStrictProviderURLs(t *testing.T) {
	cfg, err := loadConfig(t, baseConfigYAML(""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cases := []struct {
		name         string
		baseURL      string
		allowPrivate bool
		wantURLErr   bool
	}{
		{"https public", "https://api.example.com/v1", false, false},
		{"http public", "http://api.example.com/v1", false, true},
		{"http private allowed", "http://127.0.0.1:11434", true, false},
		{"private loopback", "https://127.0.0.1:11434", false, true},
		{"private loopback allowed", "https://127.0.0.1:11434", true, false},
		{"localhost", "https://localhost:11434", false, true},
		{"metadata always blocked", "http://169.254.169.254/latest", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := cfg.Clone()
			p := mutated.Providers["openai"]
			p.BaseURL = tc.baseURL
			p.AllowPrivate = tc.allowPrivate
			mutated.Providers["openai"] = p
			err := mutated.ValidateStrict()
			// Other strict requirements (auth, redis) are intentionally absent
			// here; only the base_url judgement is under test.
			hasURLErr := err != nil && strings.Contains(err.Error(), "base_url")
			if tc.wantURLErr != hasURLErr {
				t.Fatalf("base_url=%s allow_private=%v: strict error = %v, want URL error %v", tc.baseURL, tc.allowPrivate, err, tc.wantURLErr)
			}
		})
	}
}

func TestConfig_ValidateReloadRejectsRestartOnlyFields(t *testing.T) {
	prev, err := loadConfig(t, baseConfigYAML(""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	canReload := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"auth enabled", func(c *config.Config) { c.Auth.Enabled = true }},
		{"cors", func(c *config.Config) { c.CORS.AllowedOrigins = []string{"https://app.example"} }},
		{"logging level", func(c *config.Config) { c.Logging.Level = "debug" }},
		{"admin api key", func(c *config.Config) { c.Admin.APIKey = "rotated-admin-key" }},
		{"rate limit", func(c *config.Config) { c.RateLimit.RequestsPerMinute = 120 }},
	}
	for _, tc := range canReload {
		t.Run("reloadable/"+tc.name, func(t *testing.T) {
			next := prev.Clone()
			tc.mutate(next)
			if err := next.ValidateReload(prev); err != nil {
				t.Fatalf("%s should be reloadable: %v", tc.name, err)
			}
		})
	}

	restart := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"server port", func(c *config.Config) { c.Server.Port = 9090 }},
		{"admin port", func(c *config.Config) { c.Admin.Port = 9091 }},
		{"admin bind", func(c *config.Config) { c.Admin.BindAddress = "0.0.0.0" }},
		{"pprof", func(c *config.Config) { c.Admin.PprofEnabled = true }},
	}
	for _, tc := range restart {
		t.Run("restart/"+tc.name, func(t *testing.T) {
			next := prev.Clone()
			tc.mutate(next)
			err := next.ValidateReload(prev)
			if err == nil || !strings.Contains(err.Error(), "restart") {
				t.Fatalf("%s error = %v, want a clear restart error", tc.name, err)
			}
		})
	}
}

func TestConfig_ZeroMeansBuiltInDefault(t *testing.T) {
	cfg, err := loadConfig(t, baseConfigYAML(`
resilience:
  retry:
    max_attempts: 0
    base_delay: 0s
    max_delay: 0s
  circuit:
    failure_threshold: 0
    open_timeout: 0s
cache:
  ttl: 0s
  max_size: 0
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Resilience.Retry.MaxAttempts != 3 {
		t.Errorf("max_attempts = %d, want default 3", cfg.Resilience.Retry.MaxAttempts)
	}
	if cfg.Resilience.Retry.BaseDelay != 200*time.Millisecond {
		t.Errorf("base_delay = %v, want default 200ms", cfg.Resilience.Retry.BaseDelay)
	}
	if cfg.Resilience.Circuit.FailureThreshold != 5 {
		t.Errorf("failure_threshold = %d, want default 5", cfg.Resilience.Circuit.FailureThreshold)
	}
	if cfg.Cache.TTL != 5*time.Minute {
		t.Errorf("cache.ttl = %v, want default 5m", cfg.Cache.TTL)
	}
	if cfg.Cache.MaxSize != 1000 {
		t.Errorf("cache.max_size = %d, want default 1000", cfg.Cache.MaxSize)
	}
}

func TestConfig_WatchRetriesApplyFailure(t *testing.T) {
	path := writeTempConfig(t, baseConfigYAML(""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	var reported atomic.Int32
	applied := make(chan struct{})
	go func() {
		_ = config.WatchWithErrorHandler(ctx, path, 50*time.Millisecond, func(*config.Config) error {
			n := calls.Add(1)
			if n < 3 {
				return errors.New("apply rejected")
			}
			select {
			case <-applied:
			default:
				close(applied)
			}
			return nil
		}, func(error) {
			reported.Add(1)
		})
	}()

	// Touch the file after the watcher captured the initial mtime.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(path, []byte(baseConfigYAML("\n# changed\n")), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatalf("apply was not retried after failures (calls=%d)", calls.Load())
	}
	if calls.Load() < 3 {
		t.Fatalf("apply attempts = %d, want at least 3", calls.Load())
	}
	if reported.Load() < 2 {
		t.Fatalf("reported errors = %d, want at least 2", reported.Load())
	}
}

func TestConfig_WatchRecoversFromInvalidFile(t *testing.T) {
	path := writeTempConfig(t, baseConfigYAML(""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	applied := make(chan *config.Config, 1)
	go func() {
		_ = config.Watch(ctx, path, 50*time.Millisecond, func(cfg *config.Config) error {
			select {
			case applied <- cfg:
			default:
			}
			return nil
		})
	}()

	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(path, []byte("not: yaml: [bad"), 0o600); err != nil {
		t.Fatalf("write invalid: %v", err)
	}
	select {
	case <-applied:
		t.Fatal("watcher applied an invalid file")
	case <-time.After(300 * time.Millisecond):
	}

	// lastMod was not advanced by the failed load, so the fixed file is
	// detected even though it is newer than the invalid write.
	if err := os.WriteFile(path, []byte(baseConfigYAML("\n# fixed\n")), 0o600); err != nil {
		t.Fatalf("write fixed: %v", err)
	}
	select {
	case cfg := <-applied:
		if cfg == nil {
			t.Fatal("nil config applied")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not recover after the file became valid")
	}
}

func TestConfig_KeyPepperFromEnv(t *testing.T) {
	t.Setenv("AUTH_KEY_PEPPER", "env-pepper")
	cfg, err := loadConfig(t, baseConfigYAML(""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Auth.KeyPepper != "env-pepper" {
		t.Fatalf("key_pepper = %q, want env-pepper", cfg.Auth.KeyPepper)
	}

	explicit, err := loadConfig(t, baseConfigYAML("auth:\n  key_pepper: yaml-pepper\n"))
	if err != nil {
		t.Fatalf("load explicit: %v", err)
	}
	if explicit.Auth.KeyPepper != "yaml-pepper" {
		t.Fatalf("explicit key_pepper = %q, want yaml-pepper", explicit.Auth.KeyPepper)
	}
}

// TestConfig_ProductionProfilesLoad keeps the shipped profiles honest: the
// production profile must pass strict validation with the documented env vars,
// and the development profile must not silently qualify as production.
func TestConfig_ProductionProfilesLoad(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")
	t.Setenv("GEMINI_API_KEY", "test-gemini")
	t.Setenv("GATEWAY_API_KEY", "test-gateway-key-at-least-32-characters")
	t.Setenv("ADMIN_API_KEY", "test-admin-key-at-least-32-characters")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")

	prod, err := config.Load("../config.prod.yaml")
	if err != nil {
		t.Fatalf("load config.prod.yaml: %v", err)
	}
	if err := prod.ValidateStrict(); err != nil {
		t.Fatalf("config.prod.yaml must satisfy strict validation: %v", err)
	}

	dev, err := config.Load("../config.dev.yaml")
	if err != nil {
		t.Fatalf("load config.dev.yaml: %v", err)
	}
	if err := dev.ValidateStrict(); err == nil {
		t.Fatal("config.dev.yaml must not satisfy strict production validation")
	}
}
