package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
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
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/model/cache"
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

func notificationCounter(t *testing.T, s *Server) *atomic.Int32 {
	t.Helper()
	calls := new(atomic.Int32)
	client := &http.Client{Transport: mcpRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return httptest.NewRecorder().Result(), nil
	})}
	s.cluster.signaler = clustersignal.New(clustersignal.Config{
		IsWorker: func() bool { return true }, MTLSClient: func() *http.Client { return client },
		CoordinatorURL: func() string { return "https://coordinator.test" },
	})
	s.cluster.signaler.Start(t.Context())
	t.Cleanup(func() { s.cluster.signaler.Stop(context.Background()) })
	return calls
}

func TestCatalogNotificationsDrainCleanly(t *testing.T) {
	s := createSeededTestNode(t)
	calls := notificationCounter(t, s)
	for wave := 1; wave <= 2; wave++ {
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(s.notifyCoordinatorAsync)
		}
		wg.Wait()
		require.Eventually(t, func() bool { return calls.Load() == int32(wave*20) }, time.Second, time.Millisecond)
		s.cluster.signaler.Stop(context.Background())
		s.cluster.signaler.Start(t.Context())
		require.EqualValues(t, wave*20, calls.Load(), "each wave drains before another starts")
	}
}

func TestCatalogNotificationsDrainingShortCircuits(t *testing.T) {
	s := createSeededTestNode(t)
	calls := notificationCounter(t, s)
	s.draining.Store(true)
	s.notifyCoordinatorAsync()
	s.cluster.signaler.Stop(context.Background())
	require.Zero(t, calls.Load(), "draining must not start a notification")
}

type noticeInventoryClient struct {
	mesh.ClusterClient
	reads      atomic.Int32
	broadcast  atomic.Int32
	fail       bool
	beforeRead func()
}

func (c *noticeInventoryClient) Unicast(_ context.Context, node, path string, params *mesh.QueryParams) (*mesh.Response, error) {
	if c.beforeRead != nil {
		c.beforeRead()
	}
	c.reads.Add(1)
	if node != "worker" || path != "/zzrouter/v1/internal/models" || params.Method != http.MethodGet {
		return nil, errors.New("unexpected inventory request")
	}
	if c.fail {
		return nil, errors.New("worker unreachable")
	}
	return &mesh.Response{StatusCode: http.StatusOK, Body: []byte(`{"models":[{"name":"fresh","node":"worker"}]}`)}, nil
}

func (c *noticeInventoryClient) Broadcast(context.Context, string, *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	c.broadcast.Add(1)
	return &mesh.BroadcastResponse{}, nil
}

func TestHandleInternalRefreshCacheCertifiedScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		cert *x509.Certificate
		fail bool
		want int
		body string
	}{
		{"certified empty body", workerCert("worker"), false, http.StatusOK, `{}`},
		{"body cannot aim re-probe", workerCert("worker"), false, http.StatusOK, `{"node_url":"https://elsewhere.invalid"}`},
		{"body is not parsed", workerCert("worker"), false, http.StatusOK, `{`},
		{"no certificate", nil, false, http.StatusForbidden, `{}`},
		{"unknown worker", workerCert("other"), false, http.StatusForbidden, `{}`},
		{"unreachable worker", workerCert("worker"), true, http.StatusServiceUnavailable, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, coord, _ := mutationNodes(t, "worker")
			client := &noticeInventoryClient{fail: tc.fail}
			coord.model.Cache = cache.New(cache.Config{
				Node: coord.node, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: coord.appsConfig,
				ClusterClient: func() mesh.ClusterClient { return client },
			})
			require.NoError(t, coord.model.Cache.RefreshCacheSync(t.Context()))
			client.broadcast.Store(0)
			_ = catalogNoticeEngine(t, coord, worker)
			certified := coord.certifiedWorker(workerCert("worker"))
			require.NotNil(t, certified)
			for _, host := range []string{"127.0.0.1", "127.0.0.1:1"} {
				require.Equal(t, certified.NodeName, coord.resolveEndpointToNodename(host), "the full-read resolver agrees with the certificate for a matching endpoint host")
			}
			coord.MarkClusterEndpointDown("http://127.0.0.1:1")
			client.beforeRead = func() {
				ep := coord.certifiedWorker(workerCert("worker"))
				require.NotNil(t, ep)
				require.Equal(t, mesh.StatusUp, ep.Status, "a DOWN worker must be re-probed before its inventory read")
			}
			req := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/models/refresh", strings.NewReader(tc.body))
			if tc.cert != nil {
				req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{tc.cert}, VerifiedChains: [][]*x509.Certificate{{tc.cert}}}
			}
			rec := httptest.NewRecorder()
			buildCoordInternalEngine(coord).ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
			require.Zero(t, client.broadcast.Load())
			if tc.want == http.StatusForbidden {
				require.Zero(t, client.reads.Load(), "an unidentified notice refreshes nothing")
				require.Equal(t, mesh.StatusDown, coord.certifiedWorker(workerCert("worker")).Status, "an unidentified notice probes nothing")
				require.Equal(t, true, coord.model.Cache.GetCacheStats()["valid"], "an unidentified notice cannot invalidate")
			} else {
				require.EqualValues(t, 1, client.reads.Load())
			}
			if tc.want == http.StatusOK {
				require.True(t, coord.model.Cache.HasModel("fresh"), "acknowledgement follows publication")
			}
			if tc.fail {
				require.Len(t, coord.model.Cache.UnavailableInventories("worker", ""), 1)
			}
		})
	}
}

func TestProviderSyncNotifiesOnlyForChangedPush(t *testing.T) {
	worker, coord, dir := mutationNodes(t, "worker")
	var notices atomic.Int32
	worker.cluster.signaler.Stop(context.Background())
	engine := catalogNoticeEngine(t, coord, worker)
	client := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		notices.Add(1)
		require.True(t, worker.model.Cache.HasModel("fresh"), "a changed push publishes locally before notifying")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, r)
		return rec.Result(), nil
	})}
	worker.cluster.signaler = clustersignal.New(clustersignal.Config{
		IsWorker: worker.node.IsWorker, MTLSClient: func() *http.Client { return client },
		CoordinatorURL: func() string { return "https://coordinator.test" },
	})
	worker.cluster.signaler.Start(t.Context())
	configBytes, err := worker.configStore.ReadProviderConfigBytes("llamacpp")
	require.NoError(t, err)
	push := func(config []byte, changed bool) {
		c, rec := mutationContext(t, http.MethodPost, "/zzrouter/v1/internal/sync/providers/llamacpp", ProviderSyncBody{Kind: "on-demand", ConfigYAML: config})
		c.Params = gin.Params{{Key: "name", Value: "llamacpp"}}
		worker.HandleProviderSync(c)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), `"changed":`+map[bool]string{true: "true", false: "false"}[changed])
	}
	push(configBytes, false)
	require.Zero(t, notices.Load(), "an in-step push has no mutation to publish")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fresh.gguf"), []byte("GGUF"), 0600))
	push(append(configBytes, '\n'), true)
	require.EqualValues(t, 1, notices.Load())
	require.True(t, coord.model.Cache.HasModel("fresh"), "a changed push answers after coordinator publication")
}
