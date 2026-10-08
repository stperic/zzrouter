package observability

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
)

// HeaderCarrier wraps http.Header to implement propagation.TextMapCarrier.
type HeaderCarrier http.Header

// Get returns the value for the given key.
func (c HeaderCarrier) Get(key string) string {
	return http.Header(c).Get(key)
}

// Set sets the value for the given key.
func (c HeaderCarrier) Set(key, value string) {
	http.Header(c).Set(key, value)
}

// Keys returns all keys in the carrier.
func (c HeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// InjectTraceContext injects the trace context from ctx into the HTTP headers.
// This should be called when making outgoing HTTP requests to propagate the trace.
func InjectTraceContext(ctx context.Context, headers http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, HeaderCarrier(headers))
}

// W3CTraceContextHeaders are the standard W3C Trace Context header names.
const (
	TraceParentHeader = "traceparent"
	TraceStateHeader  = "tracestate"
)
