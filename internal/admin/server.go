package admin

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/capability"
	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
	"gopkg.in/yaml.v3"
)

// Server implements the Admin API on :8081 (Fase 9).
// Endpoints:
// - GET /admin/config      – current config (redacted)
// - POST /admin/reload     – reload from file (validate + rollback on error)
// - GET /admin/providers   – list providers + models
// - PATCH /admin/config    – runtime knobs (rate_limit, cache, circuit)
type Server struct {
	configPath string
	apiKey     string
	log        *slog.Logger

	mu           sync.RWMutex
	cfg          *config.Config
	registry     *proxy.Registry
	pprofEnabled bool
	requireAuth  bool

	// patchMu serializes PATCH read-modify-apply cycles so an If-Match
	// comparison cannot race another patch.
	patchMu sync.Mutex
	// failures rate limits invalid admin credentials per remote IP.
	failures *adminFailures

	// apply is called after cfg has been validated and swapped under lock.
	// It should apply live state (registry reload, limiter, breaker, etc.)
	// The function receives the new validated config.
	onApply func(*config.Config) error

	// getProviders returns provider info for /admin/providers (optional).
	getProviders func() []ProviderInfo
}

type ProviderInfo struct {
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
	// Capabilities is keyed by concrete model. Omitted when the registry has
	// no capability metadata (for example in lightweight unit tests).
	Capabilities map[string]capability.Set `json:"capabilities,omitempty"`
}

// New creates an admin Server.
// configPath is used for POST /admin/reload (Load from file).
// apiKey empty => auth disabled (for tests). In production set via ADMIN_API_KEY.
// onApply is called after config is swapped; may be nil for standalone tests.
// getProviders may be nil.
func New(log *slog.Logger, apiKey, configPath string, cfg *config.Config, registry *proxy.Registry, onApply func(*config.Config) error, getProviders func() []ProviderInfo) *Server {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return &Server{
		apiKey:       apiKey,
		configPath:   configPath,
		log:          log,
		cfg:          cfg,
		registry:     registry,
		onApply:      onApply,
		getProviders: getProviders,
		failures:     newAdminFailures(20, time.Minute),
	}
}

// SetAPIKey updates the admin credential without restarting the server. A
// reload from SIGHUP/admin takes effect on the next request.
func (s *Server) SetAPIKey(key string) {
	s.mu.Lock()
	s.apiKey = key
	s.mu.Unlock()
}

// SetPprofEnabled controls whether /debug/pprof routes are registered. They
// are off by default: heap dumps contain provider API keys and prompt data.
func (s *Server) SetPprofEnabled(enabled bool) {
	s.mu.Lock()
	s.pprofEnabled = enabled
	s.mu.Unlock()
}

// SetRequireAuth forces authentication even when apiKey is empty. Production
// wiring sets it so a missing key fails closed instead of exposing the
// control plane. It is a no-op when an API key is configured.
func (s *Server) SetRequireAuth(required bool) {
	s.mu.Lock()
	s.requireAuth = required
	s.mu.Unlock()
}

// Handler returns the mux with auth middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/config", s.handleGetConfig)
	mux.HandleFunc("POST /admin/reload", s.handlePostReload)
	mux.HandleFunc("GET /admin/providers", s.handleGetProviders)
	mux.HandleFunc("PATCH /admin/config", s.handlePatchConfig)
	// liveness for admin itself
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	s.mu.RLock()
	pprofEnabled := s.pprofEnabled
	s.mu.RUnlock()
	if pprofEnabled {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
		mux.Handle("GET /debug/pprof/heap", pprof.Handler("heap"))
		mux.Handle("GET /debug/pprof/goroutine", pprof.Handler("goroutine"))
		mux.Handle("GET /debug/pprof/threadcreate", pprof.Handler("threadcreate"))
		mux.Handle("GET /debug/pprof/block", pprof.Handler("block"))
		mux.Handle("GET /debug/pprof/mutex", pprof.Handler("mutex"))
	}
	// Always install the auth wrapper: the key may be set or rotated by a
	// reload, and the wrapper reads it per request.
	return s.authMiddleware(mux)
}

func isUnresolvedPlaceholder(v string) bool {
	return len(v) > 3 && v[:2] == "${" && v[len(v)-1] == '}'
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		apiKey := s.apiKey
		requireAuth := s.requireAuth
		s.mu.RUnlock()

		// An empty key means auth is disabled only when the operator opted in
		// with allow_insecure. Otherwise fail closed.
		if apiKey == "" || isUnresolvedPlaceholder(apiKey) {
			if requireAuth {
				s.audit(r, "auth_misconfigured", "", "admin api key is not configured")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "admin api key is not configured"})
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		ip := adminClientIP(r)
		if s.failures != nil {
			if blocked, retry := s.failures.blocked(ip); blocked {
				s.audit(r, "auth_blocked", "", "too many failed admin authentication attempts")
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds()+0.5)))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "too many failed authentication attempts"})
				return
			}
		}

		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		token = strings.TrimSpace(token)
		if token == "" {
			// also allow X-Admin-API-Key header
			token = strings.TrimSpace(r.Header.Get("X-Admin-API-Key"))
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
			if s.failures != nil {
				s.failures.record(ip)
			}
			s.audit(r, "auth_failure", actorFingerprint(token), "invalid admin api key")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid admin api key"})
			return
		}
		if s.failures != nil {
			s.failures.reset(ip)
		}
		s.audit(r, "auth_success", actorFingerprint(token), "")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	cfgCopy := s.cfg.Clone()
	s.mu.RUnlock()
	redacted := redactConfig(cfgCopy)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", cfgETag(cfgCopy))
	_ = json.NewEncoder(w).Encode(redacted)
}

func (s *Server) handlePostReload(w http.ResponseWriter, r *http.Request) {
	if s.configPath == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "config path not set"})
		return
	}
	newCfg, err := config.Load(s.configPath)
	if err != nil {
		s.log.Error("admin reload failed", "error", err)
		s.audit(r, "reload_failure", actorFromRequest(r), err.Error())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	// if onApply is set, delegate full apply (including cfg swap) to it
	if s.onApply != nil {
		if err := s.onApply(newCfg); err != nil {
			s.log.Error("admin reload apply failed", "error", err)
			s.audit(r, "reload_apply_failure", actorFromRequest(r), err.Error())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// ensure admin cfg reflects newCfg (onApply should have called SetConfig, but ensure)
		s.mu.Lock()
		s.cfg = newCfg
		s.mu.Unlock()
		s.log.Info("admin reload succeeded")
		s.audit(r, "reload", actorFromRequest(r), "")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "reloaded"})
		return
	}
	// standalone mode (tests): swap internally
	s.mu.Lock()
	oldCfg := s.cfg
	s.cfg = newCfg
	s.mu.Unlock()
	s.log.Info("admin reload succeeded")
	s.audit(r, "reload", actorFromRequest(r), "")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "reloaded"})
	// keep old for potential rollback if needed? No onApply nil means no live state to rollback
	_ = oldCfg
}

func (s *Server) handleGetProviders(w http.ResponseWriter, _ *http.Request) {
	if s.registry != nil {
		all := s.registry.All()
		var out []ProviderInfo
		for _, p := range all {
			info := ProviderInfo{Name: p.Name(), Models: p.Models()}
			for _, model := range p.Models() {
				if caps, ok := s.registry.Capabilities(p.Name(), model); ok {
					if info.Capabilities == nil {
						info.Capabilities = make(map[string]capability.Set)
					}
					info.Capabilities[model] = caps
				}
			}
			out = append(out, info)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"providers": out})
		return
	}
	if s.getProviders != nil {
		out := s.getProviders()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"providers": out})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"providers": []any{}})
}

// AdminPatch is the JSON payload for PATCH /admin/config.
// Only fields present are applied. Durations are strings like "30s", "5m".
type AdminPatch struct {
	RateLimit *struct {
		RequestsPerMinute *int    `json:"requests_per_minute"`
		Burst             *int    `json:"burst"`
		TokensPerMinute   *int    `json:"tokens_per_minute"`
		TokenBurst        *int    `json:"token_burst"`
		RedisURL          *string `json:"redis_url"`
	} `json:"rate_limit"`
	Cache *struct {
		TTL                    *string  `json:"ttl"`
		MaxTTL                 *string  `json:"max_ttl"`
		MaxSize                *int     `json:"max_size"`
		MaxBytes               *int64   `json:"max_bytes"`
		SemanticEnabled        *bool    `json:"semantic_enabled"`
		SemanticThreshold      *float64 `json:"semantic_threshold"`
		SemanticEmbeddingModel *string  `json:"semantic_embedding_model"`
		SemanticTopK           *int     `json:"semantic_top_k"`
		SemanticMaxEntries     *int     `json:"semantic_max_entries"`
	} `json:"cache"`
	Health *struct {
		ReadinessCacheTTL   *string `json:"readiness_cache_ttl"`
		CheckTimeout        *string `json:"check_timeout"`
		ProviderTimeout     *string `json:"provider_timeout"`
		SkipExpensiveChecks *bool   `json:"skip_expensive_checks"`
	} `json:"health"`
	Resilience *struct {
		Retry *struct {
			MaxAttempts *int    `json:"max_attempts"`
			BaseDelay   *string `json:"base_delay"`
			MaxDelay    *string `json:"max_delay"`
		} `json:"retry"`
		Circuit *struct {
			FailureThreshold *int    `json:"failure_threshold"`
			OpenTimeout      *string `json:"open_timeout"`
		} `json:"circuit"`
		Hedge *struct {
			Enabled *bool   `json:"enabled"`
			Delay   *string `json:"delay"`
		} `json:"hedge"`
	} `json:"resilience"`
	Routing *struct {
		Weighted map[string][]config.WeightedEntry `json:"weighted"`
	} `json:"routing"`
	Logging *struct {
		Level  *string `json:"level"`
		Format *string `json:"format"`
	} `json:"logging"`
}

func (s *Server) handlePatchConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot read body"})
		return
	}
	var patch AdminPatch
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		msg := "multiple JSON values are not allowed"
		if err != nil {
			msg = "invalid JSON: " + err.Error()
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if err := validatePatchLimits(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Serialize the read-modify-apply cycle so If-Match is meaningful.
	s.patchMu.Lock()
	defer s.patchMu.Unlock()

	s.mu.RLock()
	oldCfg := s.cfg.Clone()
	currentETag := cfgETag(s.cfg)
	s.mu.RUnlock()
	if ifMatch := strings.TrimSpace(r.Header.Get("If-Match")); ifMatch != "" && ifMatch != "*" && ifMatch != currentETag {
		s.audit(r, "patch_conflict", actorFromRequest(r), "etag mismatch")
		writeJSON(w, http.StatusConflict, map[string]string{"error": "config version does not match If-Match; reload and retry", "etag": currentETag})
		return
	}

	base := oldCfg.Clone()
	if err := applyPatch(base, &patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := base.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if s.onApply != nil {
		if err := s.onApply(base); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	} else {
		s.mu.Lock()
		s.cfg = base
		s.mu.Unlock()
	}

	// Optional persistence: temp file + fsync + rename, rollback on failure.
	if err := s.persistConfig(base); err != nil {
		if s.onApply != nil {
			if rbErr := s.onApply(oldCfg); rbErr != nil {
				s.log.Error("admin patch rollback failed", "error", rbErr)
			}
		}
		s.mu.Lock()
		s.cfg = oldCfg
		s.mu.Unlock()
		s.audit(r, "patch_persist_failed", actorFromRequest(r), err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist config: " + err.Error()})
		return
	}

	// onApply normally syncs admin cfg, but standalone mode and a nil onApply
	// need this explicit swap too.
	s.mu.Lock()
	s.cfg = base
	s.mu.Unlock()

	newETag := cfgETag(base)
	s.audit(r, "patch", actorFromRequest(r), summarizePatch(&patch))
	s.log.Info("admin patch applied")
	w.Header().Set("ETag", newETag)
	writeJSON(w, http.StatusOK, map[string]string{"status": "patched", "etag": newETag})
}

func applyPatch(cfg *config.Config, patch *AdminPatch) error {
	if patch.RateLimit != nil {
		if patch.RateLimit.RequestsPerMinute != nil {
			cfg.RateLimit.RequestsPerMinute = *patch.RateLimit.RequestsPerMinute
		}
		if patch.RateLimit.Burst != nil {
			cfg.RateLimit.Burst = *patch.RateLimit.Burst
		}
		if patch.RateLimit.TokensPerMinute != nil {
			cfg.RateLimit.TokensPerMinute = *patch.RateLimit.TokensPerMinute
		}
		if patch.RateLimit.TokenBurst != nil {
			cfg.RateLimit.TokenBurst = *patch.RateLimit.TokenBurst
		}
		if patch.RateLimit.RedisURL != nil {
			cfg.RateLimit.RedisURL = *patch.RateLimit.RedisURL
		}
	}
	if patch.Cache != nil {
		if patch.Cache.TTL != nil {
			d, err := time.ParseDuration(*patch.Cache.TTL)
			if err != nil {
				return fmt.Errorf("cache.ttl invalid duration %q", *patch.Cache.TTL)
			}
			cfg.Cache.TTL = d
		}
		if patch.Cache.MaxSize != nil {
			cfg.Cache.MaxSize = *patch.Cache.MaxSize
		}
		if patch.Cache.MaxBytes != nil {
			cfg.Cache.MaxBytes = *patch.Cache.MaxBytes
		}
		if patch.Cache.MaxTTL != nil {
			d, err := time.ParseDuration(*patch.Cache.MaxTTL)
			if err != nil {
				return fmt.Errorf("cache.max_ttl invalid duration %q", *patch.Cache.MaxTTL)
			}
			cfg.Cache.MaxTTL = d
		}
		if patch.Cache.SemanticEnabled != nil {
			cfg.Cache.SemanticEnabled = *patch.Cache.SemanticEnabled
		}
		if patch.Cache.SemanticThreshold != nil {
			cfg.Cache.SemanticThreshold = *patch.Cache.SemanticThreshold
		}
		if patch.Cache.SemanticEmbeddingModel != nil {
			cfg.Cache.SemanticEmbeddingModel = *patch.Cache.SemanticEmbeddingModel
		}
		if patch.Cache.SemanticTopK != nil {
			cfg.Cache.SemanticTopK = *patch.Cache.SemanticTopK
		}
		if patch.Cache.SemanticMaxEntries != nil {
			cfg.Cache.SemanticMaxEntries = *patch.Cache.SemanticMaxEntries
		}
	}
	if patch.Resilience != nil {
		if patch.Resilience.Retry != nil {
			if patch.Resilience.Retry.MaxAttempts != nil {
				cfg.Resilience.Retry.MaxAttempts = *patch.Resilience.Retry.MaxAttempts
			}
			if patch.Resilience.Retry.BaseDelay != nil {
				d, err := time.ParseDuration(*patch.Resilience.Retry.BaseDelay)
				if err != nil {
					return fmt.Errorf("retry.base_delay invalid %q", *patch.Resilience.Retry.BaseDelay)
				}
				cfg.Resilience.Retry.BaseDelay = d
			}
			if patch.Resilience.Retry.MaxDelay != nil {
				d, err := time.ParseDuration(*patch.Resilience.Retry.MaxDelay)
				if err != nil {
					return fmt.Errorf("retry.max_delay invalid %q", *patch.Resilience.Retry.MaxDelay)
				}
				cfg.Resilience.Retry.MaxDelay = d
			}
		}
		if patch.Resilience.Circuit != nil {
			if patch.Resilience.Circuit.FailureThreshold != nil {
				cfg.Resilience.Circuit.FailureThreshold = *patch.Resilience.Circuit.FailureThreshold
			}
			if patch.Resilience.Circuit.OpenTimeout != nil {
				d, err := time.ParseDuration(*patch.Resilience.Circuit.OpenTimeout)
				if err != nil {
					return fmt.Errorf("circuit.open_timeout invalid %q", *patch.Resilience.Circuit.OpenTimeout)
				}
				cfg.Resilience.Circuit.OpenTimeout = d
			}
		}
		if patch.Resilience.Hedge != nil {
			if patch.Resilience.Hedge.Enabled != nil {
				cfg.Resilience.Hedge.Enabled = *patch.Resilience.Hedge.Enabled
			}
			if patch.Resilience.Hedge.Delay != nil {
				d, err := time.ParseDuration(*patch.Resilience.Hedge.Delay)
				if err != nil {
					return fmt.Errorf("hedge.delay invalid %q", *patch.Resilience.Hedge.Delay)
				}
				cfg.Resilience.Hedge.Delay = d
			}
		}
	}
	if patch.Health != nil {
		if patch.Health.ReadinessCacheTTL != nil {
			d, err := time.ParseDuration(*patch.Health.ReadinessCacheTTL)
			if err != nil {
				return fmt.Errorf("health.readiness_cache_ttl invalid %q", *patch.Health.ReadinessCacheTTL)
			}
			cfg.Health.ReadinessCacheTTL = d
		}
		if patch.Health.CheckTimeout != nil {
			d, err := time.ParseDuration(*patch.Health.CheckTimeout)
			if err != nil {
				return fmt.Errorf("health.check_timeout invalid %q", *patch.Health.CheckTimeout)
			}
			cfg.Health.CheckTimeout = d
		}
		if patch.Health.ProviderTimeout != nil {
			d, err := time.ParseDuration(*patch.Health.ProviderTimeout)
			if err != nil {
				return fmt.Errorf("health.provider_timeout invalid %q", *patch.Health.ProviderTimeout)
			}
			cfg.Health.ProviderTimeout = d
		}
		if patch.Health.SkipExpensiveChecks != nil {
			cfg.Health.SkipExpensiveChecks = *patch.Health.SkipExpensiveChecks
		}
	}
	if patch.Routing != nil && patch.Routing.Weighted != nil {
		cfg.Routing.Weighted = patch.Routing.Weighted
	}
	if patch.Logging != nil {
		if patch.Logging.Level != nil {
			cfg.Logging.Level = *patch.Logging.Level
		}
		if patch.Logging.Format != nil {
			cfg.Logging.Format = *patch.Logging.Format
		}
	}
	return nil
}

func redactConfig(cfg *config.Config) *config.Config {
	// shallow clone then redact
	out := cfg.Clone()
	for name, p := range out.Providers {
		if p.APIKey != "" {
			p.APIKey = "***"
		}
		// Credentials and query strings in a base URL are secrets too
		// (?key=..., https://user:pass@host). Keep the scheme/host/path so the
		// operator can still recognize the endpoint.
		p.BaseURL = redactURL(p.BaseURL)
		out.Providers[name] = p
	}
	if out.Admin.APIKey != "" {
		out.Admin.APIKey = "***"
	}
	if out.RateLimit.RedisURL != "" {
		out.RateLimit.RedisURL = "***"
	}
	if out.Auth.KeyPepper != "" {
		out.Auth.KeyPepper = "***"
	}
	// also redact auth keys
	for i := range out.Auth.Keys {
		if out.Auth.Keys[i].Key != "" {
			out.Auth.Keys[i].Key = "***"
		}
	}
	return out
}

// redactURL strips userinfo and the query string from a URL. If the value is
// not a parseable URL it is returned unchanged unless it looks like it embeds
// credentials, in which case it is fully redacted.
func redactURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		if strings.Contains(raw, "@") || strings.Contains(raw, "?") {
			return "***"
		}
		return raw
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	if u.RawQuery != "" {
		u.RawQuery = "redacted"
	}
	if u.Fragment != "" {
		u.Fragment = "redacted"
	}
	return u.String()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// cfgETag fingerprints the effective config. It is an opaque version token for
// optimistic concurrency, not a security boundary.
func cfgETag(cfg *config.Config) string {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return `"unknown"`
	}
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// persistConfig writes the effective config to configPath with temp+rename so
// a crash cannot leave a truncated file. The file may contain resolved secrets
// (env expansion happens at load) and is therefore created with mode 0600.
func (s *Server) persistConfig(cfg *config.Config) error {
	if s.configPath == "" {
		return nil
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.configPath)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.configPath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// validatePatchLimits caps patchable knobs so a control-plane mistake or a
// compromised admin key cannot turn the gateway into a memory/CPU sink.
func validatePatchLimits(patch *AdminPatch) error {
	if patch.RateLimit != nil {
		if v := patch.RateLimit.RequestsPerMinute; v != nil && (*v < 0 || *v > 1_000_000) {
			return fmt.Errorf("rate_limit.requests_per_minute must be between 0 and 1000000")
		}
		if v := patch.RateLimit.Burst; v != nil && (*v < 0 || *v > 1_000_000) {
			return fmt.Errorf("rate_limit.burst must be between 0 and 1000000")
		}
		if v := patch.RateLimit.TokensPerMinute; v != nil && (*v < 0 || *v > 1_000_000_000) {
			return fmt.Errorf("rate_limit.tokens_per_minute must be between 0 and 1000000000")
		}
		if v := patch.RateLimit.TokenBurst; v != nil && (*v < 0 || *v > 1_000_000_000) {
			return fmt.Errorf("rate_limit.token_burst must be between 0 and 1000000000")
		}
	}
	if patch.Cache != nil {
		if v := patch.Cache.MaxSize; v != nil && (*v < 0 || *v > 1_000_000) {
			return fmt.Errorf("cache.max_size must be between 0 and 1000000")
		}
		if v := patch.Cache.MaxBytes; v != nil && (*v < 0 || *v > 1<<34) {
			return fmt.Errorf("cache.max_bytes must be between 0 and %d", int64(1)<<34)
		}
		if v := patch.Cache.TTL; v != nil {
			if d, err := time.ParseDuration(*v); err != nil || d < 0 || d > 24*time.Hour {
				return fmt.Errorf("cache.ttl must be a duration between 0 and 24h")
			}
		}
		if v := patch.Cache.MaxTTL; v != nil {
			if d, err := time.ParseDuration(*v); err != nil || d < 0 || d > 24*time.Hour {
				return fmt.Errorf("cache.max_ttl must be a duration between 0 and 24h")
			}
		}
		if v := patch.Cache.SemanticThreshold; v != nil && (*v < 0 || *v > 1) {
			return fmt.Errorf("cache.semantic_threshold must be between 0 and 1")
		}
		if v := patch.Cache.SemanticTopK; v != nil && (*v < 0 || *v > 100) {
			return fmt.Errorf("cache.semantic_top_k must be between 0 and 100")
		}
		if v := patch.Cache.SemanticMaxEntries; v != nil && (*v < 0 || *v > 1_000_000) {
			return fmt.Errorf("cache.semantic_max_entries must be between 0 and 1000000")
		}
	}
	if patch.Resilience != nil {
		if patch.Resilience.Retry != nil {
			if patch.Resilience.Retry.MaxAttempts != nil {
				v := *patch.Resilience.Retry.MaxAttempts
				if v < 0 || v > 10 {
					return fmt.Errorf("resilience.retry.max_attempts must be between 0 and 10")
				}
			}
			if err := checkDuration("resilience.retry.base_delay", patch.Resilience.Retry.BaseDelay); err != nil {
				return err
			}
			if err := checkDuration("resilience.retry.max_delay", patch.Resilience.Retry.MaxDelay); err != nil {
				return err
			}
		}
		if patch.Resilience.Circuit != nil {
			if patch.Resilience.Circuit.FailureThreshold != nil {
				v := *patch.Resilience.Circuit.FailureThreshold
				if v < 0 || v > 10_000 {
					return fmt.Errorf("resilience.circuit.failure_threshold must be between 0 and 10000")
				}
			}
			if err := checkDuration("resilience.circuit.open_timeout", patch.Resilience.Circuit.OpenTimeout); err != nil {
				return err
			}
		}
		if patch.Resilience.Hedge != nil {
			if err := checkDuration("resilience.hedge.delay", patch.Resilience.Hedge.Delay); err != nil {
				return err
			}
		}
	}
	if patch.Health != nil {
		if err := checkDuration("health.readiness_cache_ttl", patch.Health.ReadinessCacheTTL); err != nil {
			return err
		}
		if err := checkDuration("health.check_timeout", patch.Health.CheckTimeout); err != nil {
			return err
		}
		if err := checkDuration("health.provider_timeout", patch.Health.ProviderTimeout); err != nil {
			return err
		}
	}
	return nil
}

func checkDuration(name string, raw *string) error {
	if raw == nil {
		return nil
	}
	d, err := time.ParseDuration(*raw)
	if err != nil || d < 0 || d > 24*time.Hour {
		return fmt.Errorf("%s must be a duration between 0 and 24h", name)
	}
	return nil
}

// GetConfig returns current config (copy, thread-safe).
func (s *Server) GetConfig() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// SetConfig atomically replaces config (for tests / hot-reload).
func (s *Server) SetConfig(cfg *config.Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// audit emits one structured admin_audit record for every authenticated
// control-plane action, including failures. It never logs the credential
// itself: actor is a short SHA-256 fingerprint.
func (s *Server) audit(r *http.Request, action, actor, detail string) {
	if s.log == nil {
		return
	}
	attrs := []any{
		"admin_audit", true,
		"action", action,
		"actor", actor,
		"key_prefix", actor,
		"remote_ip", adminClientIP(r),
		"method", r.Method,
		"path", r.URL.Path,
		"request_id", r.Header.Get("X-Request-ID"),
	}
	if detail != "" {
		attrs = append(attrs, "detail", detail)
	}
	switch action {
	case "auth_failure", "auth_blocked", "auth_misconfigured", "patch_conflict", "patch_persist_failed":
		s.log.Warn("admin_audit", attrs...)
	default:
		s.log.Info("admin_audit", attrs...)
	}
}

func actorFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
}

// actorFromRequest extracts the presented admin credential for the audit
// fingerprint. It never returns the credential itself.
func actorFromRequest(r *http.Request) string {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	token = strings.TrimSpace(token)
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Admin-API-Key"))
	}
	return actorFingerprint(token)
}

func adminClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// summarizePatch renders the requested changes for the audit trail. Values are
// included for non-secret knobs; redis_url is named but never logged because it
// may embed credentials.
func summarizePatch(patch *AdminPatch) string {
	var fields []string
	if patch.RateLimit != nil {
		if v := patch.RateLimit.RequestsPerMinute; v != nil {
			fields = append(fields, fmt.Sprintf("rate_limit.requests_per_minute=%d", *v))
		}
		if v := patch.RateLimit.Burst; v != nil {
			fields = append(fields, fmt.Sprintf("rate_limit.burst=%d", *v))
		}
		if v := patch.RateLimit.TokensPerMinute; v != nil {
			fields = append(fields, fmt.Sprintf("rate_limit.tokens_per_minute=%d", *v))
		}
		if v := patch.RateLimit.TokenBurst; v != nil {
			fields = append(fields, fmt.Sprintf("rate_limit.token_burst=%d", *v))
		}
		if patch.RateLimit.RedisURL != nil {
			fields = append(fields, "rate_limit.redis_url=set")
		}
	}
	if c := patch.Cache; c != nil {
		if v := c.TTL; v != nil {
			fields = append(fields, "cache.ttl="+*v)
		}
		if v := c.MaxTTL; v != nil {
			fields = append(fields, "cache.max_ttl="+*v)
		}
		if v := c.MaxSize; v != nil {
			fields = append(fields, fmt.Sprintf("cache.max_size=%d", *v))
		}
		if v := c.MaxBytes; v != nil {
			fields = append(fields, fmt.Sprintf("cache.max_bytes=%d", *v))
		}
		if v := c.SemanticEnabled; v != nil {
			fields = append(fields, fmt.Sprintf("cache.semantic_enabled=%t", *v))
		}
		if v := c.SemanticThreshold; v != nil {
			fields = append(fields, fmt.Sprintf("cache.semantic_threshold=%g", *v))
		}
	}
	if patch.Resilience != nil {
		fields = append(fields, "resilience")
	}
	if patch.Health != nil {
		fields = append(fields, "health")
	}
	if patch.Routing != nil {
		fields = append(fields, "routing")
	}
	if l := patch.Logging; l != nil {
		if v := l.Level; v != nil {
			fields = append(fields, "logging.level="+*v)
		}
		if v := l.Format; v != nil {
			fields = append(fields, "logging.format="+*v)
		}
	}
	return strings.Join(fields, ",")
}

// adminFailures is the control-plane counterpart of auth's brute-force
// limiter. It is intentionally process-local and bounded.
type adminFailures struct {
	mu      sync.Mutex
	entries map[string]adminFailure
	limit   int
	window  time.Duration
	now     func() time.Time
}

type adminFailure struct {
	count int
	start time.Time
}

func newAdminFailures(limit int, window time.Duration) *adminFailures {
	return &adminFailures{
		entries: make(map[string]adminFailure),
		limit:   limit,
		window:  window,
		now:     time.Now,
	}
}

func (l *adminFailures) blocked(ip string) (bool, time.Duration) {
	if l.limit <= 0 || ip == "" {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok {
		return false, 0
	}
	now := l.now()
	if now.Sub(e.start) >= l.window {
		delete(l.entries, ip)
		return false, 0
	}
	if e.count < l.limit {
		return false, 0
	}
	return true, l.window - now.Sub(e.start)
}

func (l *adminFailures) record(ip string) {
	if l.limit <= 0 || ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[ip]
	if !ok || now.Sub(e.start) >= l.window {
		l.entries[ip] = adminFailure{count: 1, start: now}
		return
	}
	e.count++
	l.entries[ip] = e
	if len(l.entries) > 4096 {
		for key, entry := range l.entries {
			if now.Sub(entry.start) >= l.window {
				delete(l.entries, key)
			}
		}
	}
}

func (l *adminFailures) reset(ip string) {
	if l.limit <= 0 || ip == "" {
		return
	}
	l.mu.Lock()
	delete(l.entries, ip)
	l.mu.Unlock()
}
