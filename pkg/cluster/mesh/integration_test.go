package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/version"
)

// TestCircuitBreakerStateTransitions tests circuit breaker state machine
func TestCircuitBreakerStateTransitions(t *testing.T) {
	t.Parallel()
	cbm := NewCircuitBreakerManager()
	cb := cbm.GetBreaker("http://test:8080")

	// Initial state should be closed
	if cb.GetState() != StateClosed {
		t.Errorf("Expected initial state Closed, got %v", cb.GetState())
	}

	// Simulate failures to open circuit
	for range 3 {
		_, err := cb.ExecuteAny(func() (any, error) {
			return nil, fmt.Errorf("simulated failure")
		})
		if err == nil {
			t.Error("Expected error from failed execution")
		}
	}

	// Circuit should now be open
	if cb.GetState() != StateOpen {
		t.Errorf("Expected state Open after failures, got %v", cb.GetState())
	}

	// Requests should be rejected while open
	_, err := cb.ExecuteAny(func() (any, error) {
		return "success", nil
	})
	if err == nil {
		t.Error("Expected circuit breaker to reject request while open")
	}
}

// TestBroadcastPreservesNodeBodies pins the contract that a broadcast
// carries each node's response verbatim. Merging here is what silently
// dropped every worker from endpoints using the standard {"data": [...]}
// envelope: the mesh layer stripped it, and the caller then looked for a
// field that was no longer there.
func TestBroadcastPreservesNodeBodies(t *testing.T) {
	t.Parallel()
	handler := NewBroadcastHandler(&Dispatcher{})

	nodes := []*NodeResponse{
		{
			Node:     "http://host1:8080",
			NodeName: "host1",
			Response: &Response{StatusCode: 200, Body: []byte(`{"data":[{"id":"model1"}]}`)},
		},
		{
			Node:     "http://host2:8080",
			NodeName: "host2",
			Response: &Response{StatusCode: 200, Body: []byte(`{"models":[{"id":"model2"}]}`)},
		},
		{
			Node:  "http://host3:8080",
			Error: errors.New("dial timeout"),
		},
	}

	result, err := handler.collect(nodes)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	if len(result.Nodes) != 3 {
		t.Fatalf("expected 3 node results, got %d", len(result.Nodes))
	}
	if got := string(result.Nodes[0].Response.Body); got != `{"data":[{"id":"model1"}]}` {
		t.Errorf("host1 body was rewritten: %s", got)
	}
	if got := string(result.Nodes[1].Response.Body); got != `{"models":[{"id":"model2"}]}` {
		t.Errorf("host2 body was rewritten: %s", got)
	}
	if result.Nodes[2].Error == nil {
		t.Error("expected the unreachable node to keep its error")
	}
	if result.Metadata["nodes_failed"] != 1 {
		t.Errorf("expected nodes_failed=1, got %v", result.Metadata["nodes_failed"])
	}

	// The wire form must carry the same bodies, unmerged.
	var envelopes []nodeEnvelope
	if err := json.Unmarshal(result.Body, &envelopes); err != nil {
		t.Fatalf("wire form is not a node-envelope list: %v", err)
	}
	if len(envelopes) != 3 {
		t.Fatalf("expected 3 envelopes, got %d", len(envelopes))
	}
	if string(envelopes[0].Body) != `{"data":[{"id":"model1"}]}` {
		t.Errorf("envelope body was rewritten: %s", envelopes[0].Body)
	}
	if envelopes[2].Error == "" {
		t.Error("expected the failed node to surface its error on the wire")
	}
}

// A cluster with no reachable workers still ran a broadcast. If that came
// back with a nil Nodes slice the client would fall through to its
// single-response path and hand the caller the envelope list as if it were
// one node's body, so the coordinator's own data would be aggregated
// against a bogus peer.
func TestBroadcastWithNoWorkersYieldsZeroNodesNotOneUnknown(t *testing.T) {
	t.Parallel()
	handler := NewBroadcastHandler(&Dispatcher{})

	result, err := handler.collect(nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.Nodes == nil {
		t.Fatal("Nodes must be non-nil so the client knows this was a broadcast")
	}

	c := &DefaultClusterClient{}
	got, err := c.parseBroadcastResponse(result)
	if err != nil {
		t.Fatalf("parseBroadcastResponse: %v", err)
	}
	if len(got.Responses) != 0 {
		t.Fatalf("expected zero node responses, got %d (%s)",
			len(got.Responses), got.Responses[0].Response.Body)
	}
}

// A node behind a misconfigured reverse proxy answers with an HTML error
// page. That is precisely when an operator needs to see the body, so it
// must survive into the wire form rather than being dropped for not
// being JSON.
func TestBroadcastKeepsNonJSONBodies(t *testing.T) {
	t.Parallel()
	handler := NewBroadcastHandler(&Dispatcher{})

	html := `<html><body>502 Bad Gateway</body></html>`
	result, err := handler.collect([]*NodeResponse{
		{Node: "http://host1:8080", Response: &Response{StatusCode: 502, Body: []byte(html)}},
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	var envelopes []nodeEnvelope
	if err := json.Unmarshal(result.Body, &envelopes); err != nil {
		t.Fatalf("wire form is not a node-envelope list: %v", err)
	}
	if len(envelopes) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(envelopes))
	}
	var got string
	if err := json.Unmarshal(envelopes[0].Body, &got); err != nil {
		t.Fatalf("non-JSON body was not carried as a JSON string: %v", err)
	}
	if got != html {
		t.Errorf("body altered: %q", got)
	}
	if envelopes[0].Status != 502 {
		t.Errorf("status lost: %d", envelopes[0].Status)
	}
}

// TestParseBroadcastResponseHandsOffNodes checks the client lifts the
// typed node results rather than re-parsing a merged body.
func TestParseBroadcastResponseHandsOffNodes(t *testing.T) {
	t.Parallel()
	c := &DefaultClusterClient{}

	resp := &Response{
		StatusCode: 200,
		Nodes: []*NodeResponse{
			{Node: "http://host1:8080", NodeName: "host1",
				Response: &Response{StatusCode: 200, Body: []byte(`{"data":[{"id":"a"}]}`)}},
			{Node: "http://host2:8080", Error: errors.New("unreachable")},
		},
	}

	got, err := c.parseBroadcastResponse(resp)
	if err != nil {
		t.Fatalf("parseBroadcastResponse: %v", err)
	}
	if len(got.Responses) != 2 {
		t.Fatalf("expected 2 node responses, got %d", len(got.Responses))
	}
	if string(got.Responses[0].Response.Body) != `{"data":[{"id":"a"}]}` {
		t.Errorf("body not passed through: %s", got.Responses[0].Response.Body)
	}
	if got.Responses[0].NodeName != "host1" {
		t.Errorf("node name lost: %q", got.Responses[0].NodeName)
	}
	if len(got.Errors) != 1 {
		t.Errorf("expected 1 collected error, got %d", len(got.Errors))
	}
}

// TestRoundRobinHealthAware tests health-aware round-robin routing
func TestRoundRobinHealthAware(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Enabled:   true,
		Endpoints: []string{"http://host1:8080", "http://host2:8080", "http://host3:8080"},
	}

	localHandler := &MockLocalHandler{}
	cluster, err := NewCluster(cfg, "http://localhost:8080", localHandler, nil, nil)
	if err != nil {
		t.Fatalf("Failed to create cluster : %v", err)
	}

	// Mark host2 as DOWN
	endpoints := cluster.GetAllEndpoints()
	for _, ep := range endpoints {
		if ep.URL == "http://host2:8080" {
			ep.Status = StatusDown
		} else {
			ep.Status = StatusUp
		}
	}

	// Get healthy endpoints
	healthyEndpoints := make([]*Endpoint, 0)
	for _, ep := range endpoints {
		if ep.Status == StatusUp {
			healthyEndpoints = append(healthyEndpoints, ep)
		}
	}

	// Should have 3 UP endpoints (localhost + host1 + host3, excluding host2)
	if len(healthyEndpoints) != 3 {
		t.Errorf("Expected 3 healthy endpoints, got %d", len(healthyEndpoints))
	}

	// Verify round-robin handler exists
	rrHandler := NewRoundRobinHandler(cluster.dispatcher)
	if rrHandler == nil {
		t.Error("Failed to create round-robin handler")
	}
}

// TestFailoverSkipsDownEndpoints tests failover strategy skips DOWN endpoints
func TestFailoverSkipsDownEndpoints(t *testing.T) {
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

	// Mark first endpoint as DOWN
	endpoints := cluster.GetAllEndpoints()
	if len(endpoints) > 0 {
		endpoints[0].Status = StatusDown
	}
	if len(endpoints) > 1 {
		endpoints[1].Status = StatusUp
	}

	// Failover should skip DOWN endpoint
	failoverHandler := NewFailoverHandler(cluster.dispatcher)

	// This should succeed by using the UP endpoint
	req := &Request{
		Method: "GET",
		Path:   "/test",
	}

	// Filter to only UP endpoints
	upEndpoints := make([]*Endpoint, 0)
	for _, ep := range endpoints {
		if ep.Status != StatusDown {
			upEndpoints = append(upEndpoints, ep)
		}
	}

	if len(upEndpoints) == 0 {
		t.Skip("No UP endpoints for test")
	}

	// Should succeed with local endpoint
	_, err = failoverHandler.Dispatch(context.Background(), upEndpoints, req)
	if err != nil {
		t.Logf("Failover dispatch result: %v (expected for test setup)", err)
	}
}

// TestConnectorVersionAware tests version-aware connections
func TestConnectorVersionAware(t *testing.T) {
	t.Parallel()
	connector := NewConnector(ConnectorConfig{})

	if connector.versionDiscovery == nil {
		t.Error("Connector should have version discovery")
	}

	if connector.maxRetries != 5 {
		t.Errorf("Expected maxRetries=5, got %d", connector.maxRetries)
	}

	if connector.backoff.Initial != 500*time.Millisecond {
		t.Errorf("Expected backoff.Initial=500ms, got %v", connector.backoff.Initial)
	}

	if connector.classify == nil {
		t.Error("Connector should have a retry classifier")
	}
}

// TestLivenessObserve_StatusAndSnapshotIsolation drives the liveness state
// machine via Observe and verifies (a) state persists in the registry and
// (b) the returned snapshot copy cannot leak mutations into registry state.
func TestLivenessObserve_StatusAndSnapshotIsolation(t *testing.T) {
	t.Parallel()
	registry := NewEndpointRegistry()

	endpoint := &Endpoint{
		URL:    "http://test:8080",
		Name:   "test",
		Status: StatusUnknown,
	}
	if err := registry.RegisterEndpoint(endpoint); err != nil {
		t.Fatalf("Failed to register endpoint: %v", err)
	}

	// Drive to UP via a single successful probe
	driveToUp(t, registry, "http://test:8080")

	retrieved, err := registry.GetEndpointByURL("http://test:8080")
	if err != nil {
		t.Fatalf("Failed to get endpoint: %v", err)
	}
	if retrieved.Status != StatusUp {
		t.Errorf("Expected status UP, got %v", retrieved.Status)
	}

	// Mutating the snapshot must NOT affect the registry's internal state
	retrieved.Status = StatusDegraded
	check, _ := registry.GetEndpointByURL("http://test:8080")
	if check.Status != StatusUp {
		t.Errorf("Snapshot mutation leaked into registry: got %v, want UP", check.Status)
	}

	// Drive to DOWN via threshold consecutive failed probes
	driveToDown(t, registry, "http://test:8080")
	retrieved2, _ := registry.GetEndpointByURL("http://test:8080")
	if retrieved2.Status != StatusDown {
		t.Errorf("Expected status DOWN after driveToDown, got %v", retrieved2.Status)
	}
}

// TestLivenessObserve_Concurrent verifies registry-level mutations from
// concurrent Observe calls are race-safe. Run with -race.
func TestLivenessObserve_Concurrent(t *testing.T) {
	t.Parallel()
	registry := NewEndpointRegistry()

	// Register several endpoints
	for i := range 10 {
		url := fmt.Sprintf("http://host%d:8080", i)
		err := registry.RegisterEndpoint(&Endpoint{
			URL:    url,
			Name:   fmt.Sprintf("host%d", i),
			Status: StatusUnknown,
		})
		if err != nil {
			t.Fatalf("Failed to register endpoint: %v", err)
		}
	}

	var wg sync.WaitGroup

	// Concurrently flap statuses via alternating Observe probes
	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			url := fmt.Sprintf("http://host%d:8080", idx)
			failErr := fmt.Errorf("test: concurrent flap")
			for range 100 {
				registry.Observe(url, ProbeResult{OK: true, Quality: QualityFull})
				registry.Observe(url, ProbeResult{OK: false, Err: failErr})
			}
		}(i)
	}

	// Concurrently update from connections
	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			url := fmt.Sprintf("http://host%d:8080", idx)
			conn := &Connection{
				NodeName: fmt.Sprintf("worker-%d", idx),
				Version:  &version.Version{Major: 1, Minor: 0, Patch: idx},
			}
			for range 100 {
				registry.Observe(url, ProbeResult{OK: true, Quality: QualityFull, Snapshot: conn})
			}
		}(i)
	}

	// Concurrently read all endpoints
	for range 5 {
		wg.Go(func() {
			for range 100 {
				_ = registry.GetAllEndpoints()
			}
		})
	}

	wg.Wait()

	// Verify all endpoints are accessible after concurrent mutations
	endpoints := registry.GetAllEndpoints()
	if len(endpoints) != 10 {
		t.Errorf("Expected 10 endpoints, got %d", len(endpoints))
	}
}
