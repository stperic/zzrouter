package connectivity

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ============================================================================
// ConnectionManager Tests
// ============================================================================

func TestNewConnectionManager(t *testing.T) {
	cm := NewConnectionManager()

	if cm == nil {
		t.Fatal("NewConnectionManager() returned nil")
	}
	if cm.httpClient == nil {
		t.Error("httpClient should not be nil")
	}
	if cm.streamingClient == nil {
		t.Error("streamingClient should not be nil")
	}
}

func TestConnectionManager_GetHTTPClient(t *testing.T) {
	cm := NewConnectionManager()
	client := cm.GetHTTPClient()

	if client == nil {
		t.Fatal("GetHTTPClient() returned nil")
	}
	if client.Timeout != 30*time.Second {
		t.Errorf("HTTP client timeout = %v, want %v", client.Timeout, 30*time.Second)
	}
}

func TestConnectionManager_GetStreamingClient(t *testing.T) {
	cm := NewConnectionManager()
	client := cm.GetStreamingClient()

	if client == nil {
		t.Fatal("GetStreamingClient() returned nil")
	}
	// Streaming client should have no timeout
	if client.Timeout != 0 {
		t.Errorf("Streaming client timeout = %v, want 0", client.Timeout)
	}
}

func TestConnectionManager_HTTPClient_TransportSettings(t *testing.T) {
	cm := NewConnectionManager()

	// Transport is wrapped in metricsRoundTripper, check via stats
	stats := cm.GetStats()
	if stats.HTTPMaxIdleConns != 100 {
		t.Errorf("HTTPMaxIdleConns = %d, want 100", stats.HTTPMaxIdleConns)
	}
	if stats.HTTPMaxIdlePerNode != 20 {
		t.Errorf("HTTPMaxIdlePerNode = %d, want 20", stats.HTTPMaxIdlePerNode)
	}
}

func TestConnectionManager_StreamingClient_TransportSettings(t *testing.T) {
	cm := NewConnectionManager()

	// Transport is wrapped in metricsRoundTripper, check via stats
	stats := cm.GetStats()
	if stats.StreamMaxIdleConns != 100 {
		t.Errorf("StreamMaxIdleConns = %d, want 100", stats.StreamMaxIdleConns)
	}
	if stats.StreamMaxIdlePerNode != 30 {
		t.Errorf("StreamMaxIdlePerNode = %d, want 30", stats.StreamMaxIdlePerNode)
	}
}

func TestConnectionManager_GetStats_InitialValues(t *testing.T) {
	cm := NewConnectionManager()
	stats := cm.GetStats()

	// Initially, no requests have been made
	if stats.HTTPActiveRequests != 0 {
		t.Errorf("HTTPActiveRequests = %d, want 0", stats.HTTPActiveRequests)
	}
	if stats.HTTPTotalRequests != 0 {
		t.Errorf("HTTPTotalRequests = %d, want 0", stats.HTTPTotalRequests)
	}
	if stats.StreamActiveRequests != 0 {
		t.Errorf("StreamActiveRequests = %d, want 0", stats.StreamActiveRequests)
	}
	if stats.StreamTotalRequests != 0 {
		t.Errorf("StreamTotalRequests = %d, want 0", stats.StreamTotalRequests)
	}
}

func TestConnectionManager_GetStats_TracksRequests(t *testing.T) {
	cm := NewConnectionManager()

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Make a request via the HTTP client
	client := cm.GetHTTPClient()
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	_ = resp.Body.Close()

	// Check that total requests increased
	stats := cm.GetStats()
	if stats.HTTPTotalRequests != 1 {
		t.Errorf("HTTPTotalRequests = %d, want 1", stats.HTTPTotalRequests)
	}

	// Active should be 0 after request completes
	if stats.HTTPActiveRequests != 0 {
		t.Errorf("HTTPActiveRequests = %d, want 0 after request completes", stats.HTTPActiveRequests)
	}

	// Make another request via streaming client
	streamClient := cm.GetStreamingClient()
	resp2, err := streamClient.Get(server.URL)
	if err != nil {
		t.Fatalf("Stream request failed: %v", err)
	}
	_ = resp2.Body.Close()

	// Check streaming stats
	stats = cm.GetStats()
	if stats.StreamTotalRequests != 1 {
		t.Errorf("StreamTotalRequests = %d, want 1", stats.StreamTotalRequests)
	}
}

// Circuit breaker tests moved to pkg/fallback/breaker_test.go alongside the
// implementation. Connectivity-specific tests below exercise the connector
// layer that wraps the breaker; the breaker itself is exercised in fallback.

// ============================================================================
// ProviderConnector Tests
// ============================================================================

func TestNewProviderConnector(t *testing.T) {
	pc := NewProviderConnector()

	if pc == nil {
		t.Fatal("NewProviderConnector() returned nil")
	}
	if pc.httpClient == nil {
		t.Error("httpClient should not be nil")
	}
	if pc.circuitBreakers == nil {
		t.Error("circuitBreakers should not be nil")
	}
}

func TestProviderConnector_BackoffConfig(t *testing.T) {
	pc := NewProviderConnector()

	// Verify provider-optimized defaults
	if pc.backoff.Initial != 200*time.Millisecond {
		t.Errorf("Initial = %v, want 200ms", pc.backoff.Initial)
	}
	if pc.backoff.Max != 15*time.Second {
		t.Errorf("Max = %v, want 15s", pc.backoff.Max)
	}
	if pc.maxRetries != 4 {
		t.Errorf("maxRetries = %d, want 4", pc.maxRetries)
	}
}

func TestProviderConnector_isPermanentProviderError(t *testing.T) {
	pc := NewProviderConnector()

	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"no such host (DNS NotFound)", &net.DNSError{Name: "badhost", IsNotFound: true}, true},
		{"invalid url (wrapped)", fmt.Errorf("malformed url: %w", ErrPermanent), true},
		{"unauthorized (wrapped 401)", fmt.Errorf("HTTP 401: %w", ErrPermanent), true},
		{"forbidden (wrapped 403)", fmt.Errorf("HTTP 403: %w", ErrPermanent), true},
		{"method not allowed (wrapped 405)", fmt.Errorf("HTTP 405: %w", ErrPermanent), true},
		{"DNS error but not NotFound", &net.DNSError{Name: "host", IsTimeout: true}, false},
		{"connection refused", errors.New("dial tcp: connection refused"), false},
		{"timeout", errors.New("context deadline exceeded"), false},
		{"connection reset", errors.New("connection reset by peer"), false},
		{"generic error", errors.New("something went wrong"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pc.isPermanentProviderError(tt.err)
			if got != tt.permanent {
				t.Errorf("isPermanentProviderError(%q) = %v, want %v", tt.err, got, tt.permanent)
			}
		})
	}
}
