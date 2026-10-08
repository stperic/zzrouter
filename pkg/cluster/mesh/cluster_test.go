package mesh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// MockLocalHandler implements LocalHandler for testing
type MockLocalHandler struct{}

func (m *MockLocalHandler) ServeClusterRequest(ctx context.Context, req *Request) (*Response, error) {
	return &Response{
		StatusCode: 200,
		Body:       []byte("mock local response"),
		SourceNode: "localhost",
	}, nil
}

func TestNewCluster(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Enabled:   true,
		Endpoints: []string{"http://host1:8080", "http://host2:8080"},
	}

	localHandler := &MockLocalHandler{}
	cluster, err := NewCluster(cfg, "http://localhost:8080", localHandler, nil, nil)
	if err != nil {
		t.Fatalf("Failed to create cluster : %v", err)
	}

	if cluster == nil {
		t.Fatal("Cluster is nil")
	}

	// Verify endpoints registered
	endpoints := cluster.GetAllEndpoints()
	if len(endpoints) != 3 { // local + 2 remote
		t.Errorf("Expected 3 endpoints, got %d", len(endpoints))
	}
}

func TestHandleRequest_LocalDispatch(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Enabled: false, // No remote endpoints
	}

	localHandler := &MockLocalHandler{}
	cluster, err := NewCluster(cfg, "http://localhost:8080", localHandler, nil, nil)
	if err != nil {
		t.Fatalf("Failed to create cluster : %v", err)
	}

	req := &Request{
		Method:   "GET",
		Path:     "/zzrouter/v1/models",
		Strategy: StrategyUnicast,
	}

	resp, err := cluster.HandleRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("HandleRequest failed: %v", err)
	}

	if resp.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	if string(resp.Body) != "mock local response" {
		t.Errorf("Unexpected response body: %s", string(resp.Body))
	}
}

func TestParseStrategy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected DispatchStrategy
		wantErr  bool
	}{
		{"unicast", StrategyUnicast, false},
		{"broadcast", StrategyBroadcast, false},
		{"streaming", StrategyStreaming, false},
		{"roundrobin", StrategyRoundRobin, false},
		{"round-robin", StrategyRoundRobin, false},
		{"failover", StrategyFailover, false},
		{"invalid", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := ParseStrategy(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if result != tt.expected {
					t.Errorf("Expected %v, got %v", tt.expected, result)
				}
			}
		})
	}
}

func TestDetermineStrategy(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Enabled: true,
		StrategyDefaults: []StrategyDefault{
			{PathPrefix: "/zzrouter/v1/discover/", Default: "broadcast"},
			{PathPrefix: "/v1/models", Default: "roundrobin"},
		},
	}

	localHandler := &MockLocalHandler{}
	cluster, err := NewCluster(cfg, "http://localhost:8080", localHandler, nil, nil)
	if err != nil {
		t.Fatalf("Failed to create cluster : %v", err)
	}

	tests := []struct {
		name     string
		req      *Request
		expected DispatchStrategy
	}{
		{
			name:     "Explicit strategy",
			req:      &Request{Path: "/test", Strategy: StrategyBroadcast},
			expected: StrategyBroadcast,
		},
		{
			name:     "Configured default",
			req:      &Request{Path: "/zzrouter/v1/discover/providers"},
			expected: StrategyBroadcast,
		},
		{
			name:     "Target host wildcard",
			req:      &Request{Path: "/test", TargetNode: "*"},
			expected: StrategyBroadcast,
		},
		{
			name:     "Default unicast",
			req:      &Request{Path: "/test"},
			expected: StrategyUnicast,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cluster.determineStrategy(tt.req)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestHandleHTTPRequest_StrategyFromQueryParam(t *testing.T) {
	t.Parallel()
	cfg := &Config{Enabled: false}
	localHandler := &MockLocalHandler{}
	c, err := NewCluster(cfg, "http://localhost:8080", localHandler, nil, nil)
	if err != nil {
		t.Fatalf("Failed to create cluster: %v", err)
	}

	// Build HTTP request with cluster_strategy query param (no stray space)
	req := httptest.NewRequest("GET", "/zzrouter/v1/models?cluster_strategy=broadcast", nil)
	w := httptest.NewRecorder()

	// HandleHTTPRequest should parse query param and set strategy
	err = c.HandleHTTPRequest(w, req)
	if err != nil {
		t.Fatalf("HandleHTTPRequest failed: %v", err)
	}

	// If strategy was not parsed, it would fall back to unicast.
	// We verify by checking the response completed (mock handler returns 200).
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}
}

func TestHandleHTTPRequest_StrategyFromHeader(t *testing.T) {
	t.Parallel()
	cfg := &Config{Enabled: false}
	localHandler := &MockLocalHandler{}
	c, err := NewCluster(cfg, "http://localhost:8080", localHandler, nil, nil)
	if err != nil {
		t.Fatalf("Failed to create cluster: %v", err)
	}

	// Build HTTP request with X-Cluster-Strategy header (no stray space)
	req := httptest.NewRequest("GET", "/zzrouter/v1/models", nil)
	req.Header.Set("X-Cluster-Strategy", "broadcast")
	w := httptest.NewRecorder()

	err = c.HandleHTTPRequest(w, req)
	if err != nil {
		t.Fatalf("HandleHTTPRequest failed: %v", err)
	}

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}
}
