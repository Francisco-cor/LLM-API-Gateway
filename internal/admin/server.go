package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
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

	mu       sync.RWMutex
	cfg      *config.Config
	registry *proxy.Registry

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
	}
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
	var h http.Handler = mux
	if s.apiKey != "" && s.apiKey != "${ADMIN_API_KEY}" {
		h = s.authMiddleware(h)
	}
	return h
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		// allow Bearer token or raw key
		token := strings.TrimPrefix(auth, "Bearer ")
		token = strings.TrimSpace(token)
		if token == "" {
			// also allow X-Admin-API-Key header
			token = r.Header.Get("X-Admin-API-Key")
		}
		if token != s.apiKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid admin api key"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	cfgCopy := s.cfg.Clone()
	s.mu.RUnlock()
	redacted := redactConfig(cfgCopy)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(redacted)
}

func (s *Server) handlePostReload(w http.ResponseWriter, _ *http.Request) {
	if s.configPath == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "config path not set"})
		return
	}
	newCfg, err := config.Load(s.configPath)
	if err != nil {
		s.log.Error("admin reload failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	// if onApply is set, delegate full apply (including cfg swap) to it
	if s.onApply != nil {
		if err := s.onApply(newCfg); err != nil {
			s.log.Error("admin reload apply failed", "error", err)
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
			out = append(out, ProviderInfo{Name: p.Name(), Models: p.Models()})
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
		RedisURL          *string `json:"redis_url"`
	} `json:"rate_limit"`
	Cache *struct {
		TTL               *string  `json:"ttl"`
		MaxSize           *int     `json:"max_size"`
		SemanticEnabled   *bool    `json:"semantic_enabled"`
		SemanticThreshold *float64 `json:"semantic_threshold"`
	} `json:"cache"`
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "cannot read body"})
		return
	}
	var patch AdminPatch
	if err := json.Unmarshal(body, &patch); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	// if onApply is set, delegate patch apply to it (it will validate and swap)
	if s.onApply != nil {
		// clone to validate patch first (quick)
		s.mu.RLock()
		base := s.cfg.Clone()
		s.mu.RUnlock()
		if err := applyPatch(base, &patch); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if err := base.Validate(); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if err := s.onApply(base); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// sync admin cfg
		s.mu.Lock()
		s.cfg = base
		s.mu.Unlock()
		s.log.Info("admin patch applied")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "patched"})
		return
	}
	// standalone mode
	s.mu.RLock()
	base2 := s.cfg.Clone()
	s.mu.RUnlock()
	if err := applyPatch(base2, &patch); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if err := base2.Validate(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.cfg = base2
	s.mu.Unlock()
	s.log.Info("admin patch applied")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "patched"})
}

func applyPatch(cfg *config.Config, patch *AdminPatch) error {
	if patch.RateLimit != nil {
		if patch.RateLimit.RequestsPerMinute != nil {
			cfg.RateLimit.RequestsPerMinute = *patch.RateLimit.RequestsPerMinute
		}
		if patch.RateLimit.Burst != nil {
			cfg.RateLimit.Burst = *patch.RateLimit.Burst
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
		if patch.Cache.SemanticEnabled != nil {
			cfg.Cache.SemanticEnabled = *patch.Cache.SemanticEnabled
		}
		if patch.Cache.SemanticThreshold != nil {
			cfg.Cache.SemanticThreshold = *patch.Cache.SemanticThreshold
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
			out.Providers[name] = p
		}
	}
	if out.Admin.APIKey != "" {
		out.Admin.APIKey = "***"
	}
	// also redact auth keys
	for i := range out.Auth.Keys {
		if out.Auth.Keys[i].Key != "" {
			out.Auth.Keys[i].Key = "***"
		}
	}
	return out
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
