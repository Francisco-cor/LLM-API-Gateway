package tracing

import (
	"context"
	"testing"
)

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		host    string
		secure  bool
		wantErr bool
	}{
		{name: "host port", raw: "collector:4317", host: "collector:4317"},
		{name: "http", raw: "http://localhost:4317/", host: "localhost:4317"},
		{name: "https", raw: "https://collector.example:4317", host: "collector.example:4317", secure: true},
		{name: "path rejected", raw: "http://localhost:4318/v1/traces", wantErr: true},
		{name: "scheme rejected", raw: "ftp://localhost:4317", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, secure, err := normalizeEndpoint(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeEndpoint(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if err == nil && (host != tt.host || secure != tt.secure) {
				t.Fatalf("normalizeEndpoint(%q) = (%q, %v), want (%q, %v)", tt.raw, host, secure, tt.host, tt.secure)
			}
		})
	}
}

func TestInitWithoutExporterStillProvidesShutdown(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	shutdown, err := Init("test-service")
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown function")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}
}
