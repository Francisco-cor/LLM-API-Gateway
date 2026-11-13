package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestControllerSetLevelChangesOutput(t *testing.T) {
	var buf bytes.Buffer
	lv := &slog.LevelVar{}
	lv.Set(slog.LevelWarn)
	// Build the controller the same way NewControlled does, but with a buffer
	// so the test can observe the level change.
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: lv}))
	ctl := &Controller{level: lv, format: "json"}

	log.Debug("debug line")
	log.Info("info line")
	log.Warn("warn line")
	if strings.Contains(buf.String(), "info line") {
		t.Fatal("info was emitted at warn level")
	}

	if err := ctl.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel(debug): %v", err)
	}
	buf.Reset()
	log.Debug("debug line")
	if !strings.Contains(buf.String(), "debug line") {
		t.Fatalf("debug was not emitted after SetLevel: %s", buf.String())
	}
}

func TestControllerSetLevelRejectsUnknown(t *testing.T) {
	lv := &slog.LevelVar{}
	ctl := &Controller{level: lv, format: "json"}
	if err := ctl.SetLevel("verbose"); err == nil {
		t.Fatal("unknown level was accepted")
	}
}

func TestControllerSetFormatRequiresRestart(t *testing.T) {
	ctl := &Controller{level: &slog.LevelVar{}, format: "json"}
	if err := ctl.SetFormat("json"); err != nil {
		t.Fatalf("same format: %v", err)
	}
	if err := ctl.SetFormat(""); err != nil {
		t.Fatalf("empty format should be a no-op: %v", err)
	}
	if err := ctl.SetFormat("text"); !errors.Is(err, ErrFormatRequiresRestart) {
		t.Fatalf("SetFormat(text) = %v, want ErrFormatRequiresRestart", err)
	}
	if err := ctl.SetFormat("xml"); err == nil {
		t.Fatal("invalid format was accepted")
	}
}

func TestLoggerRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: redactAttr})
	slog.New(handler).Info("test",
		"authorization", "Bearer secret-token",
		"api_key", "sk-secret",
		"key_pepper", "pepper-secret",
		"model", "gpt-4o",
	)
	if strings.Contains(buf.String(), "secret-token") || strings.Contains(buf.String(), "sk-secret") || strings.Contains(buf.String(), "pepper-secret") {
		t.Fatalf("logger leaked a secret: %s", buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log is not JSON: %v", err)
	}
	if entry["api_key"] != "[REDACTED]" {
		t.Fatalf("api_key = %v, want [REDACTED]", entry["api_key"])
	}
}

func TestNewControlled(t *testing.T) {
	log, ctl := NewControlled("warn", "text")
	if log == nil || ctl == nil {
		t.Fatal("NewControlled returned nil")
	}
	if err := ctl.SetLevel("error"); err != nil {
		t.Fatalf("SetLevel(error): %v", err)
	}
	if err := ctl.SetFormat("text"); err != nil {
		t.Fatalf("SetFormat(same): %v", err)
	}
}
