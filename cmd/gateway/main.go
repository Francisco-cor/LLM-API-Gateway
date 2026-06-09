package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/logger"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/proxy"
	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	log := logger.New(cfg.Logging.Level, cfg.Logging.Format)

	providers, err := buildProviders(cfg, log)
	if err != nil {
		log.Error("provider setup failed", "error", err)
		os.Exit(1)
	}

	registry := proxy.NewRegistry(providers)
	limiter := ratelimit.New(cfg.RateLimit.RequestsPerMinute, cfg.RateLimit.Burst)

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", proxy.NewHandler(registry, cfg.FallbackChain, log))
	mux.Handle("GET /health", proxy.NewHealthHandler())
	mux.Handle("GET /health/providers", proxy.NewHealthProvidersHandler(registry))

	var handler http.Handler = mux
	if cfg.RateLimit.Enabled {
		handler = proxy.RateLimit(limiter, handler)
	}
	handler = proxy.Logging(log, handler)
	handler = proxy.RequestID(handler)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      handler,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("gateway listening", "addr", srv.Addr)
		serverErr <- srv.ListenAndServe()
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	case <-quit:
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Error("graceful shutdown failed", "error", err)
		}
	}
}

// buildProviders constructs a Provider for each configured backend that has
// a non-empty API key.
func buildProviders(cfg *config.Config, log *slog.Logger) ([]provider.Provider, error) {
	var providers []provider.Provider

	if pc, ok := cfg.Providers["openai"]; ok && pc.APIKey != "" {
		providers = append(providers, provider.NewOpenAI(pc.APIKey, pc.BaseURL, pc.Timeout, pc.Models))
		log.Info("provider configured", "name", "openai", "models", pc.Models)
	}
	if pc, ok := cfg.Providers["anthropic"]; ok && pc.APIKey != "" {
		providers = append(providers, provider.NewAnthropic(pc.APIKey, pc.BaseURL, pc.Timeout, pc.Models))
		log.Info("provider configured", "name", "anthropic", "models", pc.Models)
	}
	if pc, ok := cfg.Providers["gemini"]; ok && pc.APIKey != "" {
		providers = append(providers, provider.NewGemini(pc.APIKey, pc.BaseURL, pc.Timeout, pc.Models))
		log.Info("provider configured", "name", "gemini", "models", pc.Models)
	}

	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers configured: set at least one of OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY")
	}
	return providers, nil
}
