package logger

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// ErrFormatRequiresRestart is returned when a reload tries to change the log
// format. slog handlers are bound to their format and stream, so switching
// between JSON and text at runtime would require rebuilding the logger and
// every component that holds the pointer. Restarting is the safe, honest path.
var ErrFormatRequiresRestart = errors.New("logging.format requires a restart")

// Controller owns the mutable parts of a logger configuration.
type Controller struct {
	mu     sync.RWMutex
	level  *slog.LevelVar
	format string
}

// New builds a slog.Logger writing to stdout, configured by level and format
// ("json" or "text") as read from the gateway's logging config. It redacts
// sensitive fields (Authorization, api_key, key) via ReplaceAttr.
func New(level, format string) *slog.Logger {
	log, _ := NewControlled(level, format)
	return log
}

// NewControlled is New plus a Controller for hot-reloading the level. The
// format is recorded so SetFormat can reject a runtime change.
func NewControlled(level, format string) (*slog.Logger, *Controller) {
	lv := &slog.LevelVar{}
	lv.Set(parseLevel(level))
	opts := &slog.HandlerOptions{
		Level:       lv,
		ReplaceAttr: redactAttr,
	}

	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler), &Controller{level: lv, format: format}
}

// SetLevel applies a logging level at runtime. Unknown levels are rejected so
// a typo cannot silently downgrade to info.
func (c *Controller) SetLevel(level string) error {
	lv, err := parseLevelChecked(level)
	if err != nil {
		return err
	}
	c.mu.RLock()
	var target *slog.LevelVar
	if c.level != nil {
		target = c.level
	}
	c.mu.RUnlock()
	if target == nil {
		return fmt.Errorf("logger controller has no level var")
	}
	target.Set(lv)
	return nil
}

// SetFormat reports ErrFormatRequiresRestart for any format change. Reload
// paths should log that error at Warn level and keep serving. An empty value
// means "not specified" and leaves the current format untouched.
func (c *Controller) SetFormat(format string) error {
	if format == "" {
		return nil
	}
	if format != "json" && format != "text" {
		return fmt.Errorf("logging.format must be json or text, got %q", format)
	}
	c.mu.RLock()
	current := c.format
	c.mu.RUnlock()
	if current != "" && current != format {
		return ErrFormatRequiresRestart
	}
	c.mu.Lock()
	c.format = format
	c.mu.Unlock()
	return nil
}

func redactAttr(groups []string, a slog.Attr) slog.Attr {
	// redacted keys (case-insensitive)
	key := strings.ToLower(a.Key)
	if key == "authorization" || key == "x-api-key" || key == "api_key" || key == "api-key" || key == "key" ||
		key == "admin_api_key" || key == "key_pepper" || key == "pepper" {
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
	lv, err := parseLevelChecked(level)
	if err != nil {
		return slog.LevelInfo
	}
	return lv
}

func parseLevelChecked(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown logging level %q", level)
	}
}
