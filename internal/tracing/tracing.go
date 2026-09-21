package tracing

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Init sets up the global TracerProvider. If OTEL_EXPORTER_OTLP_ENDPOINT is
// set, spans are exported over OTLP/gRPC; otherwise the SDK still creates valid
// trace IDs locally for propagation and logging.
// Returns shutdown func.
func Init(serviceName string) (func(context.Context) error, error) {
	// Always set propagator for traceparent
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	res, err := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceNameKey.String(serviceName)),
	)
	if err != nil {
		// fallback to noop if resource fails, but still set tracer
		tp := trace.NewNoopTracerProvider()
		otel.SetTracerProvider(tp)
		return func(context.Context) error { return nil }, nil
	}

	providerOptions := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	}

	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	exporterMode := strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER"))
	if endpoint != "" && exporterMode != "none" {
		host, secure, err := normalizeEndpoint(endpoint)
		if err != nil {
			tp := sdktrace.NewTracerProvider(providerOptions...)
			otel.SetTracerProvider(tp)
			return tp.Shutdown, fmt.Errorf("invalid OTLP endpoint: %w", err)
		}
		exporterOptions := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(host)}
		if !secure {
			exporterOptions = append(exporterOptions, otlptracegrpc.WithInsecure())
		}
		exporter, err := otlptracegrpc.New(context.Background(), exporterOptions...)
		if err != nil {
			tp := sdktrace.NewTracerProvider(providerOptions...)
			otel.SetTracerProvider(tp)
			return tp.Shutdown, fmt.Errorf("create OTLP exporter: %w", err)
		}
		providerOptions = append(providerOptions, sdktrace.WithBatcher(exporter))
	}

	// Always use SDK provider so spans generate valid trace IDs (needed for logs
	// and traceparent propagation) even without an exporter configured.
	tp := sdktrace.NewTracerProvider(providerOptions...)

	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func normalizeEndpoint(raw string) (host string, secure bool, err error) {
	if !strings.Contains(raw, "://") {
		if raw == "" {
			return "", false, fmt.Errorf("endpoint is empty")
		}
		return raw, false, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" || u.Path != "" && u.Path != "/" {
		return "", false, fmt.Errorf("endpoint must contain only scheme and host")
	}
	return u.Host, u.Scheme == "https", nil
}

func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}
