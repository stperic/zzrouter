package observability

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/propagation"
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
	headers.Del(TraceParentHeader)
	headers.Del(TraceStateHeader)
	headers.Del("baggage")
	propagation.TraceContext{}.Inject(ctx, HeaderCarrier(headers))
}

// W3CTraceContextHeaders are the standard W3C Trace Context header names.
const (
	TraceParentHeader = "traceparent"
	TraceStateHeader  = "tracestate"
)

// ExtractTraceContext admits W3C trace context without caller baggage.
func ExtractTraceContext(ctx context.Context, headers http.Header) context.Context {
	return propagation.TraceContext{}.Extract(ctx, HeaderCarrier(headers))
}
