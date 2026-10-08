package observability

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestHeaderCarrier(t *testing.T) {
	headers := http.Header{}
	carrier := HeaderCarrier(headers)

	// Test Set
	carrier.Set("X-Test-Header", "test-value")
	if got := headers.Get("X-Test-Header"); got != "test-value" {
		t.Errorf("Set failed: expected 'test-value', got '%s'", got)
	}

	// Test Get
	if got := carrier.Get("X-Test-Header"); got != "test-value" {
		t.Errorf("Get failed: expected 'test-value', got '%s'", got)
	}

	// Test Keys
	keys := carrier.Keys()
	if len(keys) != 1 {
		t.Errorf("Keys failed: expected 1 key, got %d", len(keys))
	}
}

func TestInjectTraceContext(t *testing.T) {
	// Set up a tracer provider for testing
	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Create a span
	tracer := tp.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "test-span")
	defer span.End()

	// Inject trace context into headers
	headers := http.Header{}
	InjectTraceContext(ctx, headers)

	// Verify traceparent header was set
	if headers.Get(TraceParentHeader) == "" {
		t.Error("traceparent header not set")
	}
}
