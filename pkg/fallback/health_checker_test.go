package fallback

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestHealthChecker(t *testing.T, targets []HealthTarget) *HealthChecker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hc := NewHealthChecker(targets)
	hc.Start(ctx)
	t.Cleanup(func() {
		cancel()
		hc.Stop()
	})
	return hc
}

func TestHealthChecker_HealthyByDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	targets := []HealthTarget{
		{DeploymentName: "dep-a", URL: server.URL, Path: "/health", Interval: 100 * time.Millisecond, Timeout: 1 * time.Second},
	}
	hc := newTestHealthChecker(t, targets)

	// Should be healthy initially (optimistic start)
	assert.True(t, hc.IsHealthy("dep-a"))
}

func TestHealthChecker_UnknownDeployment(t *testing.T) {
	hc := newTestHealthChecker(t, nil)

	// Unknown deployments are assumed healthy
	assert.True(t, hc.IsHealthy("nonexistent"))
}

func TestHealthChecker_DetectsUnhealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	targets := []HealthTarget{
		{DeploymentName: "dep-a", URL: server.URL, Path: "/health", Interval: 50 * time.Millisecond, Timeout: 1 * time.Second},
	}
	hc := newTestHealthChecker(t, targets)

	// Wait for at least one probe
	time.Sleep(150 * time.Millisecond)

	assert.False(t, hc.IsHealthy("dep-a"), "should be unhealthy after failed probe")
}

func TestHealthChecker_RecoveryRequiresTwoSuccesses(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		if callCount <= 1 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	targets := []HealthTarget{
		{DeploymentName: "dep-a", URL: server.URL, Path: "/health", Interval: 50 * time.Millisecond, Timeout: 1 * time.Second},
	}
	hc := newTestHealthChecker(t, targets)

	// Wait for first probe (500 → unhealthy)
	time.Sleep(100 * time.Millisecond)
	assert.False(t, hc.IsHealthy("dep-a"), "should be unhealthy after failed probe")

	// Wait for 2 more probes (200 → needs 2 consecutive successes for recovery)
	time.Sleep(200 * time.Millisecond)
	assert.True(t, hc.IsHealthy("dep-a"), "should recover after 2 consecutive successes")
}

func TestHealthChecker_HealthState_TriState(t *testing.T) {
	h := NewHealthChecker([]HealthTarget{
		{DeploymentName: "configured", URL: "http://x", Path: "/p"},
	})
	// configured but no probes run yet → status seeded healthy=true
	if got := h.HealthState("configured"); got != "healthy" {
		t.Fatalf("HealthState(configured) = %q, want %q", got, "healthy")
	}
	// Unknown deployment → "unknown" (distinct from the bool-IsHealthy collapse)
	if got := h.HealthState("never-seen"); got != "unknown" {
		t.Fatalf("HealthState(unknown) = %q, want %q", got, "unknown")
	}
	// Mark unhealthy and re-read
	h.mu.Lock()
	h.status["configured"].healthy = false
	h.mu.Unlock()
	if got := h.HealthState("configured"); got != "unhealthy" {
		t.Fatalf("HealthState after fail = %q, want %q", got, "unhealthy")
	}
}

func TestHealthChecker_EmitsHealthChangedTransitions(t *testing.T) {
	h := NewHealthChecker([]HealthTarget{{DeploymentName: "r1", URL: "http://x", Path: "/p"}})

	type event struct{ replica, state string }
	var got []event
	var mu sync.Mutex
	h.SetEventEmitter(func(replica, state string) {
		mu.Lock()
		got = append(got, event{replica, state})
		mu.Unlock()
	})

	// Probes haven't run yet — seed state is healthy=true. First
	// markUnhealthy must flip to unhealthy + emit.
	h.markUnhealthy("r1")
	// Two markSuccess calls cross the recovery threshold and flip back.
	h.markSuccess("r1")
	h.markSuccess("r1")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 2, "expected one unhealthy and one healthy transition, got %v", got)
	assert.Equal(t, "unhealthy", got[0].state)
	assert.Equal(t, "healthy", got[1].state)
}

func TestHealthChecker_NoEmitOnNoTransition(t *testing.T) {
	h := NewHealthChecker([]HealthTarget{{DeploymentName: "r1", URL: "http://x"}})
	var calls int
	h.SetEventEmitter(func(string, string) { calls++ })
	// Multiple markSuccess on an already-healthy replica must not fire.
	h.markSuccess("r1")
	h.markSuccess("r1")
	h.markSuccess("r1")
	assert.Equal(t, 0, calls)
}

// An endpoint that checks a credential is probed with the one its provider
// declares; probed bare it would answer 401 and read as down.
func TestHealthChecker_SendsTheTargetsCredential(t *testing.T) {
	var mu sync.Mutex
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Get("Authorization")
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	newTestHealthChecker(t, []HealthTarget{{
		DeploymentName: "dep-a", URL: server.URL, Path: "/health",
		Interval: 20 * time.Millisecond, Timeout: time.Second,
		Header: http.Header{"Authorization": {"Bearer t"}},
	}})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen != ""
	}, 2*time.Second, 10*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Bearer t", seen)
}
