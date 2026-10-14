package observability

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/utils"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/semconv/v1.41.0/httpconv"
	"go.opentelemetry.io/otel/trace"
)

// httpMetrics holds the HTTP metrics instruments.
type httpMetrics struct {
	requestCounter  metric.Int64Counter
	requestDuration metric.Float64Histogram
	requestSize     metric.Int64Histogram
	responseSize    metric.Int64Histogram
}

var (
	globalHTTPMetrics *httpMetrics
	httpMetricsOnce   sync.Once
	httpMetricsErr    error
)

// initHTTPMetrics initializes HTTP metrics instruments. Thread-safe via sync.Once.
func initHTTPMetrics() error {
	httpMetricsOnce.Do(func() {
		httpMetricsErr = doInitHTTPMetrics()
	})
	return httpMetricsErr
}

// doInitHTTPMetrics performs the actual metrics initialization.
func doInitHTTPMetrics() error {
	meter := otel.Meter("zzrouter.http")

	requestCounter, err := meter.Int64Counter(
		"zzrouter.http.requests.total",
		metric.WithDescription("Total number of HTTP requests"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return err
	}

	requestDuration, err := meter.Float64Histogram(
		"zzrouter.http.request.duration",
		metric.WithDescription("HTTP request duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		return err
	}

	requestSize, err := meter.Int64Histogram(
		"zzrouter.http.request.size",
		metric.WithDescription("HTTP request body size in bytes"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	responseSize, err := meter.Int64Histogram(
		"zzrouter.http.response.size",
		metric.WithDescription("HTTP response body size in bytes"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	globalHTTPMetrics = &httpMetrics{
		requestCounter:  requestCounter,
		requestDuration: requestDuration,
		requestSize:     requestSize,
		responseSize:    responseSize,
	}

	return nil
}

// TracingMiddleware records HTTP spans and standard HTTP metrics from safe metadata.
func TracingMiddleware(serviceName string) gin.HandlerFunc {
	tracer := otel.Tracer("zzrouter.http.server")
	meter := otel.Meter("zzrouter.http.server")
	duration, err := httpconv.NewServerRequestDuration(meter, metric.WithExplicitBucketBoundaries(
		0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
	))
	if err != nil {
		otel.Handle(err)
	}
	requestSize, err := httpconv.NewServerRequestBodySize(meter)
	if err != nil {
		otel.Handle(err)
	}
	responseSize, err := httpconv.NewServerResponseBodySize(meter)
	if err != nil {
		otel.Handle(err)
	}
	identity := BoundMetadata(serviceName)
	return func(c *gin.Context) {
		route := c.FullPath()
		if route == "/metrics" || route == "/health" || strings.HasPrefix(route, "/health/") {
			c.Next()
			return
		}
		method := standardHTTPMethod(c.Request.Method)
		name := method + " unmatched"
		if route != "" {
			route = BoundMetadata(route)
			name = method + " " + route
		}
		scheme := "http"
		if c.Request.TLS != nil {
			scheme = "https"
		}
		protocol := "unknown"
		switch {
		case c.Request.ProtoMajor == 1 && c.Request.ProtoMinor == 0:
			protocol = "1.0"
		case c.Request.ProtoMajor == 1 && c.Request.ProtoMinor == 1:
			protocol = "1.1"
		case c.Request.ProtoMajor == 2:
			protocol = "2"
		case c.Request.ProtoMajor == 3:
			protocol = "3"
		}
		attrs := []attribute.KeyValue{
			semconv.HTTPRequestMethodKey.String(method), semconv.URLScheme(scheme),
			semconv.ServerAddress(identity), semconv.NetworkProtocolName("http"), semconv.NetworkProtocolVersion(protocol),
		}
		if route != "" {
			attrs = append(attrs, semconv.HTTPRoute(route))
		}
		original := c.Request.Context()
		// Extract W3C context without forwarding caller baggage.
		ctx := ExtractTraceContext(original, c.Request.Header)
		ctx, span := tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		start, contentLength := utils.Now(), c.Request.ContentLength
		c.Request = c.Request.WithContext(ctx)
		defer func() {
			recovered := recover()
			status := c.Writer.Status()
			category, description := "", ""
			switch {
			case recovered == http.ErrAbortHandler:
				category, description = "client_cancellation", "client cancellation"
			case recovered != nil:
				if !c.Writer.Written() {
					status = 500
				}
				category, description = "internal_failure", "internal failure"
			case original.Err() == context.DeadlineExceeded:
				category, description = "timeout", "timeout"
			case original.Err() != nil:
				category, description = "client_cancellation", "client cancellation"
			case status >= 500:
				category, description = "internal_failure", "internal failure"
			}
			attrs = append(attrs, semconv.HTTPResponseStatusCode(status))
			if category != "" {
				attrs = append(attrs, semconv.ErrorTypeKey.String(category))
				span.SetStatus(codes.Error, description)
			}
			span.SetAttributes(attrs...)
			set := attribute.NewSet(attrs...)
			if duration.Inst() != nil {
				duration.RecordSet(ctx, time.Since(start).Seconds(), set)
			}
			if requestSize.Inst() != nil {
				requestSize.RecordSet(ctx, contentLength, set)
			}
			if responseSize.Inst() != nil {
				responseSize.RecordSet(ctx, int64(c.Writer.Size()), set)
			}
			span.End()
			c.Request = c.Request.WithContext(original)
			if recovered != nil {
				panic(recovered)
			}
		}()
		c.Next()
	}
}

func standardHTTPMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return method
	default:
		return "_OTHER"
	}
}

// MetricsMiddleware returns a Gin middleware that records HTTP metrics.
func MetricsMiddleware() gin.HandlerFunc {
	// Initialize metrics (thread-safe via sync.Once)
	if err := initHTTPMetrics(); err != nil {
		// If metrics fail to initialize, return a no-op middleware
		return func(c *gin.Context) {
			c.Next()
		}
	}

	return func(c *gin.Context) {
		start := utils.Now()

		// Record request size
		if c.Request.ContentLength > 0 {
			globalHTTPMetrics.requestSize.Record(c.Request.Context(), c.Request.ContentLength,
				metric.WithAttributes(
					semconv.HTTPRequestMethodKey.String(c.Request.Method),
					semconv.HTTPRouteKey.String(c.FullPath()),
				),
			)
		}

		// Process request
		c.Next()

		// Calculate duration
		duration := time.Since(start).Seconds()

		// Common attributes for all metrics
		attrs := []attribute.KeyValue{
			semconv.HTTPRequestMethodKey.String(c.Request.Method),
			semconv.HTTPRouteKey.String(c.FullPath()),
			semconv.HTTPResponseStatusCodeKey.Int(c.Writer.Status()),
		}

		// Record request count
		globalHTTPMetrics.requestCounter.Add(c.Request.Context(), 1, metric.WithAttributes(attrs...))

		// Record duration
		globalHTTPMetrics.requestDuration.Record(c.Request.Context(), duration, metric.WithAttributes(attrs...))

		// Record response size
		responseSize := int64(c.Writer.Size())
		if responseSize > 0 {
			globalHTTPMetrics.responseSize.Record(c.Request.Context(), responseSize, metric.WithAttributes(attrs...))
		}
	}
}
