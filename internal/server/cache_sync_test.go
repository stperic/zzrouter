package server

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// seededProviderFiles publishes immutable bytes from a real installation. Each
// fixture still loads its own mutable configuration from private files.
var seededProviderFiles struct {
	once  sync.Once
	files map[string][]byte
	err   error
}

func seedTestProviders(t *testing.T, dir string, providers, enabled []string) {
	t.Helper()
	seededProviderFiles.once.Do(func() {
		seededProviderFiles.files, seededProviderFiles.err = installedProviderFiles()
	})
	require.NoError(t, seededProviderFiles.err)
	found := make(map[string]bool)
	for rel, source := range seededProviderFiles.files {
		parts := strings.Split(rel, "/")
		if len(parts) > 1 && providers != nil && !slices.Contains(providers, parts[1]) {
			continue
		}
		data := source
		if len(parts) == 3 && parts[2] == "config.yaml" {
			found[parts[1]] = true
			if slices.Contains(enabled, parts[1]) {
				var doc map[string]any
				require.NoError(t, yaml.Unmarshal(data, &doc))
				doc["enabled"] = true
				var err error
				data, err = yaml.Marshal(doc)
				require.NoError(t, err)
			}
		}
		path := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
	for _, name := range append(slices.Clone(providers), enabled...) {
		require.True(t, found[name], "fixture provider %q was not installed", name)
	}
}

// installedProviderFiles snapshots a real installation without a test's
// lifetime, so a failure reaches every caller rather than only the first.
func installedProviderFiles() (map[string][]byte, error) {
	source, err := os.MkdirTemp("", "zzrouter-providers-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(source) }()
	if err := templates.InstallDefaults(source); err != nil {
		return nil, err
	}
	files := make(map[string][]byte)
	return files, filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files[rel] = data
		// The receipt is generated; every shipped file must remain byte-identical.
		if strings.HasPrefix(rel, ".") {
			return nil
		}
		shipped, err := templates.AppsFS.ReadFile("files/providers/" + rel)
		if err != nil {
			return err
		}
		if !bytes.Equal(shipped, data) {
			return fmt.Errorf("installed %s differs from the shipped file", rel)
		}
		return nil
	})
}

type seededTestNodeConfig struct {
	TestNodeConfig
	providers        []string
	enabled          []string
	modelsDir        string
	prepareProviders func(string)
}

// createSeededTestNode owns seeded fixtures without changing BuildTestServer's
// exported contract. Cleanup drains the server before restoring its home.
func createSeededTestNode(t *testing.T, options ...seededTestNodeConfig) *Server {
	t.Helper()
	cfg := seededTestNodeConfig{TestNodeConfig: DefaultTestNodeConfig()}
	if len(options) != 0 {
		cfg = options[0]
	}
	if cfg.modelsDir != "" {
		modelregistry.SetModelsRootDirOverride(cfg.modelsDir)
		t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	}
	// Best-effort removal like createTestNode: a handle still open on Windows must not fail the test.
	home, err := os.MkdirTemp("", "zzrouter-seed-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("ZZROUTER_TEST_HOME", home)
	providersDir := filepath.Join(home, "providers")
	seedTestProviders(t, providersDir, cfg.providers, cfg.enabled)
	if cfg.prepareProviders != nil {
		cfg.prepareProviders(providersDir)
	}
	name := cfg.NodeName
	if name == "" {
		name = "test-host"
	}
	hostCfg := &pkgConfig.NodeConfig{
		Node:    pkgConfig.ServeConfig{Bind: "localhost", Port: getNextTestPort(), Name: name},
		Cluster: pkgConfig.ClusterConfig{Mode: cfg.ClusterMode},
		Auth:    pkgConfig.AuthConfig{AdminKey: cfg.AdminKey, UserKey: cfg.UserKey, ClusterNetworkKey: cfg.ClusterKey},
	}
	s, err := NewServerWithOptions(hostCfg)
	require.NoError(t, err)
	require.NotNil(t, s.engine, "test server engine must be constructed")
	t.Cleanup(func() {
		cleanupTestNode(s)
		s.model.Downloads.Stop()
	})
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
	var invalidated, refreshed, published atomic.Int32
	node := NewNodeIdentity(&pkgConfig.NodeConfig{
		Cluster: pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeCoordinator},
	})
	exec := &SystemExecutor{
		node:             node,
		invalidateCache:  func() { invalidated.Add(1) },
		refreshCacheSync: func(context.Context) error { published.Add(1); return nil },
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
	require.EqualValues(t, 1, published.Load())
	require.Equal(t, 200, rec.Code)
}
