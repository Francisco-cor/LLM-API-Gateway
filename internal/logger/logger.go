package logger

import (
	"log/slog"
	"os"
	"strings"
)

// New builds a slog.Logger writing to stdout, configured by level and format
// ("json" or "text") as read from the gateway's logging config.
// It redacts sensitive fields (Authorization, api_key, key) via ReplaceAttr.
func New(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       parseLevel(level),
		ReplaceAttr: redactAttr,
	}

	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

func redactAttr(groups []string, a slog.Attr) slog.Attr {
	// redacted keys (case-insensitive)
	key := strings.ToLower(a.Key)
	if key == "authorization" || key == "x-api-key" || key == "api_key" || key == "api-key" || key == "key" {
		return slog.String(a.Key, "[REDACTED]")
	}
	// redact Authorization header values that slipped into message
	if a.Value.Kind() == slog.KindString {
		v := a.Value.String()
		if strings.Contains(strings.ToLower(v), "bearer ") || strings.Contains(strings.ToLower(v), "sk-") {
			// only redact if looks like a secret, keep rest
			if len(v) > 20 {
				return slog.String(a.Key, "[REDACTED]")
			}
		}
	}
	return a
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
