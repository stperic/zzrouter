package observability

import (
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/utils"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
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

// TracingMiddleware returns a Gin middleware that creates spans for HTTP requests.
// It uses otelgin's default span naming which uses route templates (e.g., "/api/users/:id")
// instead of actual paths (e.g., "/api/users/123") to avoid high cardinality.
func TracingMiddleware(serviceName string) gin.HandlerFunc {
	return otelgin.Middleware(serviceName)
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
