package tracing

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Init sets up global TracerProvider. If OTEL_EXPORTER_OTLP_ENDPOINT is set,
// it would configure OTLP exporter (currently always uses SDK with AlwaysSample
// so traceparent is always generated even without exporter).
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

	// Always use SDK provider so spans generate valid trace IDs (needed for logs
	// and traceparent propagation) even without an exporter configured.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// If OTLP endpoint is set, the exporter would be added here (omitted for minimal deps)
	_ = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")

	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}
