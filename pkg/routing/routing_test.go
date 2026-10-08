package routing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
)

// ============================================================================
// Types Tests
// ============================================================================

func TestRoutingError_Error(t *testing.T) {
	tests := []struct {
		name     string
		err      *RoutingError
		contains string
	}{
		{
			name: "with cause",
			err: &RoutingError{
				Message: "test message",
				Details: "test details",
				Cause:   errors.New("root cause"),
			},
			contains: "caused by",
		},
		{
			name: "without cause",
			err: &RoutingError{
				Message: "test message",
				Details: "test details",
			},
			contains: "test message: test details",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errStr := tt.err.Error()
			if !containsString(errStr, tt.contains) {
				t.Errorf("RoutingError.Error() = %q, should contain %q", errStr, tt.contains)
			}
		})
	}
}

func TestRoutingError_Unwrap(t *testing.T) {
	cause := errors.New("root cause")
	err := &RoutingError{
		Message: "wrapper",
		Cause:   cause,
	}

	if err.Unwrap() != cause {
		t.Errorf("RoutingError.Unwrap() should return the cause")
	}
}

// ============================================================================
// Mock Types
// ============================================================================

// mockClusterClient implements mesh.ClusterClient for testing
type mockClusterClient struct {
	unicastResp     *mesh.Response
	unicastErr      error
	broadcastResp   *mesh.BroadcastResponse
	broadcastErr    error
	unicastCalled   bool
	broadcastCalled bool
}

func (m *mockClusterClient) Query(ctx context.Context, endpoint string, params *mesh.QueryParams) (*mesh.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *mockClusterClient) Broadcast(ctx context.Context, endpoint string, params *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	m.broadcastCalled = true
	return m.broadcastResp, m.broadcastErr
}

func (m *mockClusterClient) Unicast(ctx context.Context, host string, endpoint string, params *mesh.QueryParams) (*mesh.Response, error) {
	m.unicastCalled = true
	return m.unicastResp, m.unicastErr
}

func (m *mockClusterClient) Stream(ctx context.Context, host string, endpoint string, params *mesh.QueryParams) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

// mockLocalHandler is a simple local handler for testing
func mockLocalHandler(statusCode int, body []byte, err error) LocalHandler {
	return func(ctx context.Context, req *Request) (*Response, error) {
		if err != nil {
			return nil, err
		}
		return &Response{
			StatusCode: statusCode,
			Body:       body,
			Node:       "localhost",
		}, nil
	}
}

// ============================================================================
// LocalOnlyRouter Tests
// ============================================================================

func TestNewLocalOnlyRouter(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	router := NewLocalOnlyRouterWithClient("node1", "9090", handler, nil)

	if router == nil {
		t.Fatal("NewLocalOnlyRouterWithClient() returned nil")
	}
	if router.nodeName != "node1" {
		t.Errorf("nodeName = %q, want %q", router.nodeName, "node1")
	}
	if router.serverPort != "9090" {
		t.Errorf("serverPort = %q, want %q", router.serverPort, "9090")
	}
}

func TestLocalOnlyRouter_Unicast(t *testing.T) {
	body := []byte(`{"models": []}`)
	handler := mockLocalHandler(200, body, nil)
	router := NewLocalOnlyRouterWithClient("node1", "9090", handler, nil)

	ctx := context.Background()
	resp, err := router.Unicast(ctx, "any-host", "/api/tags", "GET", nil)

	if err != nil {
		t.Fatalf("Unicast() error = %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if string(resp.Body) != string(body) {
		t.Errorf("Body = %q, want %q", string(resp.Body), string(body))
	}
}

func TestLocalOnlyRouter_Unicast_Error(t *testing.T) {
	handler := mockLocalHandler(0, nil, errors.New("handler error"))
	router := NewLocalOnlyRouterWithClient("node1", "9090", handler, nil)

	ctx := context.Background()
	_, err := router.Unicast(ctx, "any-host", "/api/tags", "GET", nil)

	if err == nil {
		t.Error("Unicast() should return error when handler fails")
	}
}

func TestLocalOnlyRouter_Broadcast(t *testing.T) {
	body := []byte(`{"models": ["llama2"]}`)
	handler := mockLocalHandler(200, body, nil)
	router := NewLocalOnlyRouterWithClient("node1", "9090", handler, nil)

	ctx := context.Background()
	resp, err := router.Broadcast(ctx, "/api/tags", "GET", nil)

	if err != nil {
		t.Fatalf("Broadcast() error = %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
}

func TestLocalOnlyRouter_Route(t *testing.T) {
	body := []byte(`{"status": "ok"}`)
	handler := mockLocalHandler(200, body, nil)
	router := NewLocalOnlyRouterWithClient("node1", "9090", handler, nil)

	ctx := context.Background()
	req := &Request{
		Method: "GET",
		Path:   "/api/status",
		Node:   "some-host",
	}

	resp, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
}

func TestLocalOnlyRouter_MultiRoute_Empty(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	router := NewLocalOnlyRouterWithClient("node1", "9090", handler, nil)

	ctx := context.Background()
	resp, err := router.MultiRoute(ctx, "/api/test", "POST", map[string][]byte{})

	if err != nil {
		t.Fatalf("MultiRoute() error = %v", err)
	}
	if string(resp.Body) != "[]" {
		t.Errorf("Body = %q, want %q", string(resp.Body), "[]")
	}
}

func TestLocalOnlyRouter_MultiRoute_LocalNodes(t *testing.T) {
	handler := mockLocalHandler(200, []byte(`{"status": "ok"}`), nil)
	router := NewLocalOnlyRouterWithClient("node1", "9090", handler, nil)

	ctx := context.Background()
	hostBodies := map[string][]byte{
		"":        []byte(`{"key": "value1"}`),
		"@master": []byte(`{"key": "value2"}`),
		"node1":   []byte(`{"key": "value3"}`),
	}

	resp, err := router.MultiRoute(ctx, "/api/test", "POST", hostBodies)
	if err != nil {
		t.Fatalf("MultiRoute() error = %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}

	// Parse response to verify all hosts were processed
	var responses []map[string]any
	if err := json.Unmarshal(resp.Body, &responses); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if len(responses) != 3 {
		t.Errorf("Expected 3 responses, got %d", len(responses))
	}
}

func TestLocalOnlyRouter_MultiRoute_RemoteNode(t *testing.T) {
	handler := mockLocalHandler(200, []byte(`{"status": "ok"}`), nil)
	router := NewLocalOnlyRouterWithClient("node1", "9090", handler, nil)

	ctx := context.Background()
	hostBodies := map[string][]byte{
		"remote-host": []byte(`{"key": "value"}`),
	}

	resp, err := router.MultiRoute(ctx, "/api/test", "POST", hostBodies)
	if err != nil {
		t.Fatalf("MultiRoute() error = %v", err)
	}

	// Parse response to verify error was recorded
	var responses []map[string]any
	if err := json.Unmarshal(resp.Body, &responses); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if len(responses) != 1 {
		t.Fatalf("Expected 1 response, got %d", len(responses))
	}

	// Remote host should have an error
	if _, hasError := responses[0]["error"]; !hasError {
		t.Error("Remote host should have error in response")
	}
}

// ============================================================================
// ClusterAwareRouter Tests
// ============================================================================

func TestNewClusterAwareRouter(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	mockClient := &mockClusterClient{}
	router := NewClusterAwareRouterWithClient("node1", mockClient, "9090", handler, nil)

	if router == nil {
		t.Fatal("NewClusterAwareRouterWithClient() returned nil")
	}
	if router.nodeName != "node1" {
		t.Errorf("nodeName = %q, want %q", router.nodeName, "node1")
	}
}

func TestClusterAwareRouter_Unicast_Local_EmptyNode(t *testing.T) {
	body := []byte(`{"models": []}`)
	handler := mockLocalHandler(200, body, nil)
	mockClient := &mockClusterClient{}
	router := NewClusterAwareRouterWithClient("node1", mockClient, "9090", handler, nil)

	ctx := context.Background()
	resp, err := router.Unicast(ctx, "", "/api/tags", "GET", nil)

	if err != nil {
		t.Fatalf("Unicast() error = %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	// Should NOT call cluster client for local execution
	if mockClient.unicastCalled {
		t.Error("Should not call cluster client for empty host")
	}
}

func TestClusterAwareRouter_Unicast_Local_Localhost(t *testing.T) {
	body := []byte(`{"status": "ok"}`)
	handler := mockLocalHandler(200, body, nil)
	mockClient := &mockClusterClient{}
	router := NewClusterAwareRouterWithClient("node1", mockClient, "9090", handler, nil)

	ctx := context.Background()
	resp, err := router.Unicast(ctx, "localhost", "/api/status", "GET", nil)

	if err != nil {
		t.Fatalf("Unicast() error = %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if mockClient.unicastCalled {
		t.Error("Should not call cluster client for localhost")
	}
}

func TestClusterAwareRouter_Unicast_Local_NodeName(t *testing.T) {
	body := []byte(`{"status": "ok"}`)
	handler := mockLocalHandler(200, body, nil)
	mockClient := &mockClusterClient{}
	router := NewClusterAwareRouterWithClient("my-node", mockClient, "9090", handler, nil)

	ctx := context.Background()
	resp, err := router.Unicast(ctx, "my-node", "/api/status", "GET", nil)

	if err != nil {
		t.Fatalf("Unicast() error = %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if mockClient.unicastCalled {
		t.Error("Should not call cluster client when host matches node name")
	}
}

func TestClusterAwareRouter_Unicast_Remote(t *testing.T) {
	handler := mockLocalHandler(200, []byte("local"), nil)
	mockClient := &mockClusterClient{
		unicastResp: &mesh.Response{
			StatusCode: 200,
			Body:       []byte(`{"status": "remote"}`),
			SourceNode: "http://203.0.113.10:9090",
		},
	}
	router := NewClusterAwareRouterWithClient("node1", mockClient, "9090", handler, nil)

	ctx := context.Background()
	resp, err := router.Unicast(ctx, "remote-host", "/api/status", "GET", nil)

	if err != nil {
		t.Fatalf("Unicast() error = %v", err)
	}
	if !mockClient.unicastCalled {
		t.Error("Should call cluster client for remote host")
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	// Verify host is extracted from URL
	if resp.Node != "203.0.113.10" {
		t.Errorf("Node = %q, want %q", resp.Node, "203.0.113.10")
	}
}

func TestClusterAwareRouter_Unicast_RemoteNoCluster(t *testing.T) {
	handler := mockLocalHandler(200, []byte("local"), nil)
	// No cluster client configured
	router := NewClusterAwareRouterWithClient("node1", nil, "9090", handler, nil)

	ctx := context.Background()
	_, err := router.Unicast(ctx, "remote-host", "/api/status", "GET", nil)

	if err == nil {
		t.Error("Unicast() should return error when no cluster client and remote host requested")
	}
}

func TestClusterAwareRouter_Broadcast(t *testing.T) {
	localBody := []byte(`{"models": [{"name": "local-model"}]}`)
	handler := mockLocalHandler(200, localBody, nil)

	mockClient := &mockClusterClient{
		broadcastResp: &mesh.BroadcastResponse{
			Responses: []*mesh.NodeResponse{
				{
					Node: "remote1",
					Response: &mesh.Response{
						StatusCode: 200,
						Body:       []byte(`{"models": [{"name": "remote-model"}]}`),
					},
				},
			},
		},
	}
	router := NewClusterAwareRouterWithClient("node1", mockClient, "9090", handler, nil)

	ctx := context.Background()
	resp, err := router.Broadcast(ctx, "/api/tags", "GET", nil)

	if err != nil {
		t.Fatalf("Broadcast() error = %v", err)
	}
	if !mockClient.broadcastCalled {
		t.Error("Should call cluster client for broadcast")
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
}

func TestClusterAwareRouter_Route_Broadcast(t *testing.T) {
	localBody := []byte(`{"models": []}`)
	handler := mockLocalHandler(200, localBody, nil)
	mockClient := &mockClusterClient{
		broadcastResp: &mesh.BroadcastResponse{},
	}
	router := NewClusterAwareRouterWithClient("node1", mockClient, "9090", handler, nil)

	ctx := context.Background()

	tests := []struct {
		name string
		host string
	}{
		{"empty host", ""},
		{"wildcard", "*"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient.broadcastCalled = false
			req := &Request{
				Method: "GET",
				Path:   "/api/tags",
				Node:   tt.host,
			}

			_, err := router.Route(ctx, req)
			if err != nil {
				t.Fatalf("Route() error = %v", err)
			}
			// Route with empty/wildcard host should use Broadcast
			if !mockClient.broadcastCalled {
				t.Error("Route() with wildcard host should call Broadcast")
			}
		})
	}
}

func TestClusterAwareRouter_Route_Unicast(t *testing.T) {
	localBody := []byte(`{"status": "ok"}`)
	handler := mockLocalHandler(200, localBody, nil)
	mockClient := &mockClusterClient{
		unicastResp: &mesh.Response{
			StatusCode: 200,
			Body:       []byte(`{"status": "remote"}`),
			SourceNode: "http://203.0.113.10:9090",
		},
	}
	router := NewClusterAwareRouterWithClient("node1", mockClient, "9090", handler, nil)

	ctx := context.Background()
	req := &Request{
		Method: "GET",
		Path:   "/api/status",
		Node:   "specific-host",
	}

	_, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	// Since specific-host != node1, it should try cluster client
	if !mockClient.unicastCalled {
		t.Error("Route() with specific host should call Unicast")
	}
}

func TestClusterAwareRouter_MultiRoute_Empty(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	mockClient := &mockClusterClient{}
	router := NewClusterAwareRouterWithClient("node1", mockClient, "9090", handler, nil)

	ctx := context.Background()
	resp, err := router.MultiRoute(ctx, "/api/test", "POST", map[string][]byte{})

	if err != nil {
		t.Fatalf("MultiRoute() error = %v", err)
	}
	if string(resp.Body) != "[]" {
		t.Errorf("Body = %q, want %q", string(resp.Body), "[]")
	}
}

func TestClusterAwareRouter_MultiRoute_WithNodes(t *testing.T) {
	localBody := []byte(`{"status": "processed"}`)
	handler := mockLocalHandler(200, localBody, nil)
	mockClient := &mockClusterClient{
		broadcastResp: &mesh.BroadcastResponse{},
	}
	router := NewClusterAwareRouterWithClient("node1", mockClient, "9090", handler, nil)

	ctx := context.Background()
	hostBodies := map[string][]byte{
		"":      []byte(`{"key": "value1"}`),
		"node1": []byte(`{"key": "value2"}`),
	}

	resp, err := router.MultiRoute(ctx, "/api/test", "POST", hostBodies)
	if err != nil {
		t.Fatalf("MultiRoute() error = %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
}

func TestClusterAwareRouter_isLocalIP(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	router := NewClusterAwareRouterWithClient("node1", nil, "9090", handler, nil)

	tests := []struct {
		name     string
		host     string
		expected bool
	}{
		{"loopback IPv4", "127.0.0.1", true},
		{"loopback IPv6", "::1", true},
		{"loopback with port", "127.0.0.1:9090", true},
		// External IPs should be false (unless they match local interfaces)
		{"external IP", "8.8.8.8", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := router.isLocalIP(tt.host)
			if got != tt.expected {
				t.Errorf("isLocalIP(%q) = %v, want %v", tt.host, got, tt.expected)
			}
		})
	}
}

// ============================================================================
// Response Aggregation Tests
// ============================================================================

func TestClusterAwareRouter_detectArrayFieldName(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	router := NewClusterAwareRouterWithClient("node1", nil, "9090", handler, nil)

	tests := []struct {
		name      string
		localBody []byte
		expected  string
	}{
		{"models field", []byte(`{"models": []}`), "models"},
		{"instances field", []byte(`{"instances": []}`), "instances"},
		{"downloads field", []byte(`{"downloads": []}`), "downloads"},
		{"providers field", []byte(`{"providers": []}`), "providers"},
		{"data field", []byte(`{"data": []}`), "data"},
		{"unknown field", []byte(`{"other": []}`), ""},
		{"invalid json", []byte(`not json`), ""},
		{"empty body", []byte{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localResp := &Response{Body: tt.localBody}
			got := router.detectArrayFieldName(localResp, nil)
			if got != tt.expected {
				t.Errorf("detectArrayFieldName() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestClusterAwareRouter_detectArrayFieldName_FromCluster(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	router := NewClusterAwareRouterWithClient("node1", nil, "9090", handler, nil)

	// Local has no recognizable field, but cluster does
	localResp := &Response{Body: []byte(`{"unknown": []}`)}
	clusterResp := &mesh.BroadcastResponse{
		Responses: []*mesh.NodeResponse{
			{
				Response: &mesh.Response{
					StatusCode: 200,
					Body:       []byte(`{"models": ["model1"]}`),
				},
			},
		},
	}

	got := router.detectArrayFieldName(localResp, clusterResp)
	if got != "models" {
		t.Errorf("detectArrayFieldName() = %q, want %q", got, "models")
	}
}

func TestClusterAwareRouter_aggregateResponses_NoField(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	router := NewClusterAwareRouterWithClient("node1", nil, "9090", handler, nil)

	// Single object response (no array field) - should return first successful
	localResp := &Response{
		StatusCode: 200,
		Body:       []byte(`{"name": "model1", "size": 1234}`),
	}

	resp, err := router.aggregateResponses(localResp, nil)
	if err != nil {
		t.Fatalf("aggregateResponses() error = %v", err)
	}
	if string(resp.Body) != string(localResp.Body) {
		t.Errorf("Should return local response for single object")
	}
}

func TestClusterAwareRouter_aggregateResponses_WithModels(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	router := NewClusterAwareRouterWithClient("node1", nil, "9090", handler, nil)

	localResp := &Response{
		StatusCode: 200,
		Body:       []byte(`{"models": [{"name": "local-model"}]}`),
	}
	clusterResp := &mesh.BroadcastResponse{
		Responses: []*mesh.NodeResponse{
			{
				Node: "remote1",
				Response: &mesh.Response{
					StatusCode: 200,
					Body:       []byte(`{"models": [{"name": "remote-model"}]}`),
				},
			},
		},
	}

	resp, err := router.aggregateResponses(localResp, clusterResp)
	if err != nil {
		t.Fatalf("aggregateResponses() error = %v", err)
	}

	// Parse aggregated response
	var result map[string]any
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	models, ok := result["models"].([]any)
	if !ok {
		t.Fatal("Response should have 'models' array")
	}
	if len(models) != 2 {
		t.Errorf("Expected 2 models, got %d", len(models))
	}
}

func TestClusterAwareRouter_aggregateResponses_NoSuccessful(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	router := NewClusterAwareRouterWithClient("node1", nil, "9090", handler, nil)

	// No local response and no cluster responses
	resp, err := router.aggregateResponses(nil, nil)
	if err != nil {
		t.Fatalf("aggregateResponses() error = %v", err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("StatusCode = %d, want 404", resp.StatusCode)
	}
}

func TestClusterAwareRouter_aggregateResponses_ErrorLocal(t *testing.T) {
	handler := mockLocalHandler(200, []byte("ok"), nil)
	router := NewClusterAwareRouterWithClient("node1", nil, "9090", handler, nil)

	// Local error response, cluster has data
	localResp := &Response{
		StatusCode: 500,
		Body:       []byte(`{"error": "internal error"}`),
		Error:      errors.New("internal error"),
	}
	clusterResp := &mesh.BroadcastResponse{
		Responses: []*mesh.NodeResponse{
			{
				Node: "remote1",
				Response: &mesh.Response{
					StatusCode: 200,
					Body:       []byte(`{"name": "model1"}`),
				},
			},
		},
	}

	resp, err := router.aggregateResponses(localResp, clusterResp)
	if err != nil {
		t.Fatalf("aggregateResponses() error = %v", err)
	}
	// Should return cluster response since local failed
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200 (from cluster)", resp.StatusCode)
	}
}

// ============================================================================
// Request/Response Type Tests
// ============================================================================

func TestRequest_Fields(t *testing.T) {
	req := &Request{
		Method:    "POST",
		Path:      "/api/generate",
		Node:      "localhost",
		ModelName: "llama2",
		Body:      []byte(`{"prompt": "test"}`),
		Headers:   http.Header{"Content-Type": []string{"application/json"}},
		Timeout:   30 * time.Second,
	}

	if req.Method != "POST" {
		t.Errorf("Method = %q, want %q", req.Method, "POST")
	}
	if req.Path != "/api/generate" {
		t.Errorf("Path = %q, want %q", req.Path, "/api/generate")
	}
	if req.Node != "localhost" {
		t.Errorf("Node = %q, want %q", req.Node, "localhost")
	}
	if req.ModelName != "llama2" {
		t.Errorf("ModelName = %q, want %q", req.ModelName, "llama2")
	}
	if req.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want %v", req.Timeout, 30*time.Second)
	}
}

func TestResponse_Fields(t *testing.T) {
	resp := &Response{
		StatusCode: 200,
		Body:       []byte(`{"status": "ok"}`),
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Node:       "test-host",
		Duration:   100 * time.Millisecond,
		Error:      nil,
	}

	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, 200)
	}
	if resp.Node != "test-host" {
		t.Errorf("Node = %q, want %q", resp.Node, "test-host")
	}
	if resp.Duration != 100*time.Millisecond {
		t.Errorf("Duration = %v, want %v", resp.Duration, 100*time.Millisecond)
	}
}

func TestModelLocation_Fields(t *testing.T) {
	loc := &ModelLocation{
		ModelName: "llama2:7b",
		Node:      "gpu-server",
		Provider:  "ollama",
		LastSeen:  time.Now(),
	}

	if loc.ModelName != "llama2:7b" {
		t.Errorf("ModelName = %q, want %q", loc.ModelName, "llama2:7b")
	}
	if loc.Node != "gpu-server" {
		t.Errorf("Node = %q, want %q", loc.Node, "gpu-server")
	}
	if loc.Provider != "ollama" {
		t.Errorf("Provider = %q, want %q", loc.Provider, "ollama")
	}
}

// ============================================================================
// Sentinel Error Tests
// ============================================================================

// ============================================================================
// Helper Functions
// ============================================================================

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
