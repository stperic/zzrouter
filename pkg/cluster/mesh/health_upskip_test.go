package mesh

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestCheckAllNodes_ProbesStatusUpEndpoint asserts that the cache-
// coherency Commit A change (drop UP-skip) took effect: checkAllNodes
// now probes endpoints that are already StatusUp, so a dropped notify
// reconciles on the next tick instead of waiting for a flap.
func TestCheckAllNodes_ProbesStatusUpEndpoint(t *testing.T) {
	var probeCount int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&probeCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hostname":"mock","apps_detail":[]}`))
	}))
	defer srv.Close()

	reg := NewEndpointRegistry()
	if err := reg.RegisterEndpoint(&Endpoint{URL: srv.URL, IsLocal: false}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Mark the endpoint as StatusUp — pre-Commit-A this would cause
	// checkAllNodes to skip it.
	reg.mu.Lock()
	reg.endpoints[srv.URL].Status = StatusUp
	reg.mu.Unlock()

	hm := NewHealthMonitor(&HealthMonitorConfig{
		Connector:       NewConnector(ConnectorConfig{HTTPClient: srv.Client()}),
		Registry:        reg,
		CircuitBreakers: NewCircuitBreakerManager(),
		CheckInterval:   500 * time.Millisecond,
	})

	hm.checkAllNodes()

	got := atomic.LoadInt64(&probeCount)
	if got == 0 {
		t.Fatalf("StatusUp endpoint was not probed — UP-skip regression")
	}
}
