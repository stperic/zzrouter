package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
)

// createSeededTestNode wraps createTestNode with a templates.InstallDefaults
// so configStore is populated — required for Finalize tests that mutate
// real providers (e.g. ollama).
func createSeededTestNode(t *testing.T) *Server {
	t.Helper()
	home, err := os.MkdirTemp("", "zzrouter-seed-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("ZZROUTER_TEST_HOME", home)
	require.NoError(t, templates.InstallDefaults(filepath.Join(home, "providers")))

	hostCfg := &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Bind: "localhost",
			Port: getNextTestPort(),
			Name: "test-host",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode: pkgConfig.ClusterModeDisabled,
		},
		Auth: pkgConfig.AuthConfig{AdminKey: TestAdminKey},
	}
	s, err := NewServerWithOptions(hostCfg)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupTestNode(s) })
	return s
}

// TestFinalizeOnboarding_HookFiresOnSuccess asserts the post-finalize
// hook is invoked after a successful FinalizeOnboarding. This is the
// worker-side seam that triggers NotifyMasterCacheRefresh.
func TestFinalizeOnboarding_HookFiresOnSuccess(t *testing.T) {
	s := createSeededTestNode(t)

	var called atomic.Int32
	var gotName atomic.Value
	s.SetOnFinalizeCallback(func(name string) {
		called.Add(1)
		gotName.Store(name)
	})

	// ollama is a default-enabled always-on provider in the shipped
	// templates; FinalizeOnboarding on it is idempotent (re-enable),
	// which still fires the hook.
	if err := s.FinalizeOnboarding("ollama"); err != nil {
		t.Fatalf("FinalizeOnboarding: %v", err)
	}
	if got := called.Load(); got != 1 {
		t.Fatalf("hook invocations = %d, want 1", got)
	}
	if n, _ := gotName.Load().(string); n != "ollama" {
		t.Errorf("hook name = %q, want ollama", n)
	}
}

// TestFinalizeOnboarding_HookSilentOnFailure asserts that when
// SetProviderEnabled fails (unknown provider), the hook does NOT
// fire — guarantees no phantom notify on a failed mutation.
func TestFinalizeOnboarding_HookSilentOnFailure(t *testing.T) {
	s := createSeededTestNode(t)

	var called atomic.Int32
	s.SetOnFinalizeCallback(func(string) { called.Add(1) })

	err := s.FinalizeOnboarding("definitely-not-a-real-provider")
	if err == nil {
		t.Fatalf("FinalizeOnboarding on bogus provider must error")
	}
	if got := called.Load(); got != 0 {
		t.Fatalf("hook fired on failure: %d invocations", got)
	}
}

// TestFinalizeOffboarding_HookSilentOnFailure — symmetric guard for the
// teardown funnel.
func TestFinalizeOffboarding_HookSilentOnFailure(t *testing.T) {
	s := createSeededTestNode(t)

	var called atomic.Int32
	s.SetOnFinalizeCallback(func(string) { called.Add(1) })

	err := s.FinalizeOffboarding("definitely-not-a-real-provider")
	if err == nil {
		t.Fatalf("FinalizeOffboarding on bogus provider must error")
	}
	if got := called.Load(); got != 0 {
		t.Fatalf("hook fired on failure: %d invocations", got)
	}
}

// TestFinalize_LastMutationTimestamp asserts the dirty-bit is bumped
// on successful Finalize (fuel for the drop-UP-skip correctness floor).
func TestFinalize_LastMutationTimestamp(t *testing.T) {
	s := createSeededTestNode(t)

	before := s.lastMutation.Load()
	if err := s.FinalizeOnboarding("ollama"); err != nil {
		t.Fatalf("FinalizeOnboarding: %v", err)
	}
	after := s.lastMutation.Load()
	if after <= before {
		t.Fatalf("lastMutation did not advance: before=%d after=%d", before, after)
	}
}

// TestCoalescer_DrainsCleanly fires N parallel Async calls and asserts
// the coalescer drains without hanging and without deadlock. The exact
// "runs count" is not directly observable through the public API — a
// stronger test requires injecting a counter into RefreshClusterEndpoints.
func TestCoalescer_DrainsCleanly(t *testing.T) {
	s := createSeededTestNode(t)

	const callers = 20
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			s.RefreshClusterEndpointsAsync()
		}()
	}
	wg.Wait()

	drained := make(chan struct{})
	go func() {
		s.refreshWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatalf("refreshWG never drained — coalescer is stuck")
	}

	// Fire a second wave after drain to verify running/queued flags reset cleanly.
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			s.RefreshClusterEndpointsAsync()
		}()
	}
	wg.Wait()

	drained2 := make(chan struct{})
	go func() {
		s.refreshWG.Wait()
		close(drained2)
	}()
	select {
	case <-drained2:
	case <-time.After(3 * time.Second):
		t.Fatalf("second wave stuck — flags didn't reset")
	}
}

// TestCoalescer_DrainingShortCircuits asserts RefreshClusterEndpointsAsync
// is a no-op once draining is set — no goroutine spawn, no wg entry.
func TestCoalescer_DrainingShortCircuits(t *testing.T) {
	s := createSeededTestNode(t)
	s.draining.Store(true)

	// Before: ensure wg is at zero.
	s.refreshWG.Wait()

	s.RefreshClusterEndpointsAsync()

	// Wait returns immediately iff no Add was called.
	done := make(chan struct{})
	go func() {
		s.refreshWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("refreshWG.Add was called despite draining")
	}
}

// TestHandleInternalRefreshCache_FansOutBothCaches asserts the
// coord-side notify handler invokes BOTH the cache-C invalidator and
// the cache-B refresh (the unit fan-out contract from the doc).
func TestHandleInternalRefreshCache_FansOutBothCaches(t *testing.T) {
	var invalidated, refreshed atomic.Int32
	node := NewNodeIdentity(&pkgConfig.NodeConfig{
		Cluster: pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeCoordinator},
	})
	exec := &SystemExecutor{
		node:             node,
		invalidateCache:  func() { invalidated.Add(1) },
		refreshEndpoints: func() { refreshed.Add(1) },
	}

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/zzrouter/v1/internal/models/refresh", nil)
	exec.HandleInternalRefreshCache(c)

	if got := invalidated.Load(); got != 1 {
		t.Errorf("invalidateCache invocations = %d, want 1", got)
	}
	if got := refreshed.Load(); got != 1 {
		t.Errorf("refreshEndpoints invocations = %d, want 1", got)
	}
}
