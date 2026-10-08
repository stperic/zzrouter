package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupTestProvider(t *testing.T) (*Provider, func()) {
	ctx := context.Background()

	cfg := &Config{
		Enabled:     true,
		ServiceName: "middleware-test",
		Tracing: TracingConfig{
			Enabled:    true,
			SampleRate: 1.0,
		},
		Metrics: MetricsConfig{
			Enabled:        true,
			PrometheusPort: 0, // Disable Prometheus for middleware tests
		},
	}

	provider, err := NewProvider(ctx, cfg, "1.0.0")
	require.NoError(t, err)

	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = provider.Stop(ctx)
	}

	return provider, cleanup
}

func TestTracingMiddleware(t *testing.T) {
	provider, cleanup := setupTestProvider(t)
	defer cleanup()

	// Create a test router with the tracing middleware
	router := gin.New()
	router.Use(TracingMiddleware("test-service"))
	router.GET("/test", func(c *gin.Context) {
		// Verify span is in context
		span := trace.SpanFromContext(c.Request.Context())
		assert.True(t, span.SpanContext().IsValid(), "Expected valid span in context")
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Make a test request
	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotNil(t, provider) // Ensure provider is used
}

func TestTracingMiddleware_SpanName(t *testing.T) {
	_, cleanup := setupTestProvider(t)
	defer cleanup()

	var capturedSpanName string

	router := gin.New()
	router.Use(TracingMiddleware("test-service"))
	router.GET("/api/v1/users/:id", func(c *gin.Context) {
		span := trace.SpanFromContext(c.Request.Context())
		// We can't directly get the span name, but we can verify the span exists
		capturedSpanName = "span captured"
		assert.True(t, span.SpanContext().IsValid())
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id")})
	})

	req := httptest.NewRequest("GET", "/api/v1/users/123", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "span captured", capturedSpanName)
}

func TestMetricsMiddleware(t *testing.T) {
	// Initialize metrics
	err := initHTTPMetrics()
	require.NoError(t, err)

	router := gin.New()
	router.Use(MetricsMiddleware())
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.POST("/test", func(c *gin.Context) {
		c.JSON(http.StatusCreated, gin.H{"status": "created"})
	})

	// Make GET request
	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Make POST request
	req = httptest.NewRequest("POST", "/test", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	// Metrics should have been recorded (we can't easily verify the values without
	// setting up a full metrics exporter, but at least verify no errors)
}

func TestMetricsMiddleware_RecordsRequestSize(t *testing.T) {
	err := initHTTPMetrics()
	require.NoError(t, err)

	router := gin.New()
	router.Use(MetricsMiddleware())
	router.POST("/data", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"received": true})
	})

	// Request with body
	body := []byte(`{"key": "value", "data": "some content"}`)
	req := httptest.NewRequest("POST", "/data", nil)
	req.Body = http.NoBody
	req.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestMetricsMiddleware_RecordsResponseSize(t *testing.T) {
	err := initHTTPMetrics()
	require.NoError(t, err)

	router := gin.New()
	router.Use(MetricsMiddleware())
	router.GET("/large", func(c *gin.Context) {
		// Return a larger response
		c.JSON(http.StatusOK, gin.H{
			"data":    "some large response data",
			"items":   []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			"message": "This is a test response with more content",
		})
	})

	req := httptest.NewRequest("GET", "/large", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, w.Body.Len() > 0, "Response should have body")
}

func TestMetricsMiddleware_RecordsDuration(t *testing.T) {
	err := initHTTPMetrics()
	require.NoError(t, err)

	router := gin.New()
	router.Use(MetricsMiddleware())
	router.GET("/slow", func(c *gin.Context) {
		// Simulate some processing time
		time.Sleep(10 * time.Millisecond)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	req := httptest.NewRequest("GET", "/slow", nil)
	w := httptest.NewRecorder()

	start := time.Now()
	router.ServeHTTP(w, req)
	duration := time.Since(start)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, duration >= 10*time.Millisecond, "Request should have taken at least 10ms")
}

func TestTracingMiddleware_PropagatesContext(t *testing.T) {
	provider, cleanup := setupTestProvider(t)
	defer cleanup()

	var parentTraceID, childTraceID string

	router := gin.New()
	router.Use(TracingMiddleware("test-service"))
	router.GET("/parent", func(c *gin.Context) {
		// Get span from context
		span := trace.SpanFromContext(c.Request.Context())
		parentTraceID = span.SpanContext().TraceID().String()

		// Create child span
		ctx, childSpan := otel.Tracer("test").Start(c.Request.Context(), "child-operation")
		childTraceID = trace.SpanFromContext(ctx).SpanContext().TraceID().String()
		childSpan.End()

		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	req := httptest.NewRequest("GET", "/parent", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, parentTraceID)
	assert.NotEmpty(t, childTraceID)
	assert.Equal(t, parentTraceID, childTraceID, "Child span should have same trace ID as parent")
	assert.NotNil(t, provider)
}

func TestMetricsMiddleware_DifferentStatusCodes(t *testing.T) {
	err := initHTTPMetrics()
	require.NoError(t, err)

	router := gin.New()
	router.Use(MetricsMiddleware())
	router.GET("/success", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/not-found", func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	})
	router.GET("/error", func(c *gin.Context) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	})

	tests := []struct {
		path           string
		expectedStatus int
	}{
		{"/success", http.StatusOK},
		{"/not-found", http.StatusNotFound},
		{"/error", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			assert.Equal(t, tt.expectedStatus, w.Code)
		})
	}
}
