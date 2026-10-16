package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type syncProbeClient struct {
	mesh.ClusterClient
	probe func(context.Context, string, string, *mesh.QueryParams) (*mesh.Response, error)
}

func (c *syncProbeClient) Unicast(ctx context.Context, node, path string, params *mesh.QueryParams) (*mesh.Response, error) {
	return c.probe(ctx, node, path, params)
}

func compatibleSyncClusterClient() mesh.ClusterClient {
	return &syncProbeClient{probe: func(context.Context, string, string, *mesh.QueryParams) (*mesh.Response, error) {
		body, err := json.Marshal(version.VersionInfo{ClusterProtocol: version.ClusterProtocolVersion})
		return &mesh.Response{StatusCode: http.StatusOK, Body: body}, err
	}}
}

type syncTestTransport func(*http.Request) (*http.Response, error)

func (f syncTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func syncTestResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func awaitSyncTest(t *testing.T, f *providerSyncFanOut) SyncReport {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	return f.AwaitQuiesce(ctx)
}

func TestProviderSyncProtocolFreshEvidence(t *testing.T) {
	window := version.LocalProtocolWindow()
	for _, tc := range []struct {
		name      string
		protocol  int
		status    int
		raw       string
		err       error
		nilClient bool
		allow     bool
	}{
		{name: "compatible", protocol: window.Max, status: 200, allow: true},
		{name: "zero", status: 200},
		{name: "below minimum", protocol: window.Min - 1, status: 200},
		{name: "above maximum", protocol: window.Max + 1, status: 200},
		{name: "missing protocol", raw: `{}`, status: 200},
		{name: "malformed", raw: `{`, status: 200},
		{name: "HTTP failure", protocol: window.Max, status: 503},
		{name: "transport failure", err: errors.New("synthetic unavailable peer")},
		{name: "canceled probe", err: context.Canceled},
		{name: "nil client", nilClient: true},
	} {
		for _, status := range []mesh.EndpointStatus{mesh.StatusUp, mesh.StatusDown, mesh.StatusUnknown} {
			t.Run(tc.name+"/"+string(status), func(t *testing.T) {
				store := newFanOutTestStore(t)
				require.NoError(t, store.AddProviderInstance("other", pkgConfig.NewOllamaConnectProvider("other", "http://fixture.invalid", "")))
				var probes, posts atomic.Int32
				probe := &syncProbeClient{probe: func(ctx context.Context, node, path string, params *mesh.QueryParams) (*mesh.Response, error) {
					probes.Add(1)
					assert.Equal(t, "http://worker.invalid:9090", node)
					assert.Equal(t, "/zzrouter/v1/internal/version", path)
					assert.Equal(t, http.MethodGet, params.Method)
					deadline, bounded := ctx.Deadline()
					assert.True(t, bounded)
					assert.LessOrEqual(t, time.Until(deadline), providerSyncPushTimeout)
					raw := tc.raw
					if raw == "" {
						body, err := json.Marshal(version.VersionInfo{Version: &version.Version{Major: 9999}, ClusterProtocol: tc.protocol})
						require.NoError(t, err)
						raw = string(body)
					}
					return &mesh.Response{StatusCode: tc.status, Body: []byte(raw)}, tc.err
				}}
				client := &http.Client{Transport: syncTestTransport(func(r *http.Request) (*http.Response, error) {
					assert.Equal(t, "https://worker.invalid:9091", r.URL.Scheme+"://"+r.URL.Host)
					posts.Add(1)
					return syncTestResponse(r, 200, `{}`), nil
				})}
				f := newProviderSyncFanOut(store, func() bool { return true }, func() []*mesh.Endpoint {
					return []*mesh.Endpoint{{URL: "http://worker.invalid:9090", ClusterURL: "https://worker.invalid:9091", NodeName: "worker", Status: status}}
				}, func() *http.Client { return client }, func() mesh.ClusterClient {
					if tc.nilClient {
						return nil
					}
					return probe
				})
				for round := int32(1); round <= 2; round++ {
					f.fire(store.Config())
					got := awaitSyncTest(t, f)
					if tc.allow {
						assert.Equal(t, SyncReport{State: SyncComplete, Landed: []string{"worker"}}, got)
						assert.Equal(t, 2*round, posts.Load())
					} else {
						assert.Equal(t, SyncReport{State: SyncPartial}, got)
						assert.Zero(t, posts.Load(), "incompatible or unreadable evidence must prevent every POST")
					}
					if !tc.nilClient {
						assert.Equal(t, round, probes.Load(), "one probe per target per round, including coalesced provider pushes")
					}
				}
			})
		}
	}
}

func TestProviderSyncProtocolEmptyIdentityCannotSelectAnotherPeer(t *testing.T) {
	store := newFanOutTestStore(t)
	var probes, posts atomic.Int32
	probe := &syncProbeClient{probe: func(ctx context.Context, _, _ string, _ *mesh.QueryParams) (*mesh.Response, error) {
		probes.Add(1)
		return compatibleSyncClusterClient().Unicast(ctx, "other", "", nil)
	}}
	client := &http.Client{Transport: syncTestTransport(func(r *http.Request) (*http.Response, error) {
		posts.Add(1)
		return syncTestResponse(r, 200, `{}`), nil
	})}
	f := newProviderSyncFanOut(store, func() bool { return true }, func() []*mesh.Endpoint {
		return []*mesh.Endpoint{{ClusterURL: "https://unknown.invalid:9091", NodeName: "unknown"}}
	}, func() *http.Client { return client }, func() mesh.ClusterClient { return probe })
	f.fire(store.Config())
	assert.Equal(t, SyncReport{State: SyncPartial}, awaitSyncTest(t, f))
	assert.Zero(t, probes.Load())
	assert.Zero(t, posts.Load())
	_, err := queryClusterVersion(t.Context(), probe, "")
	assert.ErrorIs(t, err, update.ErrNodeUnavailable)
}

func TestProviderSyncProtocolReprobesAfterRollback(t *testing.T) {
	store := newFanOutTestStore(t)
	var protocol atomic.Int32
	protocol.Store(int32(version.ClusterProtocolVersion))
	var posts atomic.Int32
	probe := &syncProbeClient{probe: func(context.Context, string, string, *mesh.QueryParams) (*mesh.Response, error) {
		body, err := json.Marshal(version.VersionInfo{ClusterProtocol: int(protocol.Load())})
		return &mesh.Response{StatusCode: 200, Body: body}, err
	}}
	client := &http.Client{Transport: syncTestTransport(func(r *http.Request) (*http.Response, error) {
		posts.Add(1)
		return syncTestResponse(r, 200, `{}`), nil
	})}
	f := newProviderSyncFanOut(store, func() bool { return true }, func() []*mesh.Endpoint {
		return []*mesh.Endpoint{{URL: "https://worker.invalid", NodeName: "worker", Status: mesh.StatusUp}}
	}, func() *http.Client { return client }, func() mesh.ClusterClient { return probe })
	f.fire(store.Config())
	assert.Equal(t, SyncComplete, awaitSyncTest(t, f).State)
	protocol.Store(int32(version.MinClusterProtocolVersion - 1))
	f.fire(store.Config())
	assert.Equal(t, SyncPartial, awaitSyncTest(t, f).State)
	assert.EqualValues(t, 1, posts.Load(), "cached UP state and a previous passing probe must not authorize a later round")
}

func TestProviderSyncProtocolPendingKeepsHealthyLanded(t *testing.T) {
	store := newFanOutTestStore(t)
	entered, release, posted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	probe := &syncProbeClient{probe: func(ctx context.Context, node, path string, params *mesh.QueryParams) (*mesh.Response, error) {
		if node == "https://slow.invalid" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return nil, context.DeadlineExceeded
		}
		return compatibleSyncClusterClient().Unicast(ctx, node, path, params)
	}}
	client := &http.Client{Transport: syncTestTransport(func(r *http.Request) (*http.Response, error) {
		assert.Equal(t, "healthy.invalid", r.URL.Host)
		close(posted)
		return syncTestResponse(r, 200, `{}`), nil
	})}
	f := newProviderSyncFanOut(store, func() bool { return true }, func() []*mesh.Endpoint {
		return []*mesh.Endpoint{{URL: "https://slow.invalid", NodeName: "slow"}, {URL: "https://healthy.invalid", NodeName: "healthy"}}
	}, func() *http.Client { return client }, func() mesh.ClusterClient { return probe })
	f.fire(store.Config())
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("slow probe was not started")
	}
	select {
	case <-posted:
	case <-time.After(3 * time.Second):
		t.Fatal("healthy push waited for slow version probe")
	}
	var report SyncReport
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		report = f.AwaitQuiesce(ctx)
		return report.Reached("healthy")
	}, time.Second, time.Millisecond)
	assert.Equal(t, SyncPending, report.State)
	unblock.Do(func() { close(release) })
	assert.Equal(t, SyncReport{State: SyncPartial, Landed: []string{"healthy"}}, awaitSyncTest(t, f))
}

func TestProviderSyncProtocolMeshIdentityAndWholeBytes(t *testing.T) {
	store := newFanOutTestStore(t)
	_, err := store.WriteAsset("ollama", "workflow.json", []byte(`{"operator":"asset"}`))
	require.NoError(t, err)
	schemaPath := filepath.Join(store.DirPath(), "external", "ollama", "schema.yaml")
	const schema = "parameters: {}\n"
	require.NoError(t, os.WriteFile(schemaPath, []byte(schema), 0600))
	config, err := store.ReadProviderConfigBytes("ollama")
	require.NoError(t, err)
	var probes, posts atomic.Int32
	client := &http.Client{Transport: syncTestTransport(func(r *http.Request) (*http.Response, error) {
		assert.Equal(t, "https://worker.invalid:9091", r.URL.Scheme+"://"+r.URL.Host, "version and POST must both use the cluster transport address")
		if r.Method == http.MethodGet {
			assert.Equal(t, "/zzrouter/v1/internal/version", r.URL.Path)
			probes.Add(1)
			body, err := json.Marshal(version.VersionInfo{ClusterProtocol: version.ClusterProtocolVersion})
			require.NoError(t, err)
			return syncTestResponse(r, 200, string(body)), nil
		}
		posts.Add(1)
		var body ProviderSyncBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, config, body.ConfigYAML)
		assert.Equal(t, []byte(schema), body.SchemaYAML)
		assert.Equal(t, map[string][]byte{"workflow.json": []byte(`{"operator":"asset"}`)}, body.Assets)
		return syncTestResponse(r, 200, `{}`), nil
	})}
	cluster, err := mesh.NewCluster(&mesh.Config{}, "http://coordinator.invalid:9090", nil, nil, mesh.NewConnector(mesh.ConnectorConfig{HTTPClient: client}))
	require.NoError(t, err)
	cluster.Stop() // Fixture dispatches directly, with no health-monitor traffic.
	ep := &mesh.Endpoint{URL: "http://worker.invalid:9090", ClusterURL: "https://worker.invalid:9091", NodeName: "worker", Status: mesh.StatusUp}
	require.NoError(t, cluster.RegisterEndpoint(ep))
	f := newProviderSyncFanOut(store, func() bool { return true }, func() []*mesh.Endpoint { return []*mesh.Endpoint{ep} }, func() *http.Client { return client }, cluster.GetClient)
	f.fire(store.Config())
	assert.Equal(t, SyncReport{State: SyncComplete, Landed: []string{"worker"}}, awaitSyncTest(t, f))
	assert.EqualValues(t, 1, probes.Load())
	assert.EqualValues(t, 1, posts.Load())
}

func TestProviderSyncProtocolReceiverRefusesChangedContractBeforeDisk(t *testing.T) {
	store, workerStore := newFanOutTestStore(t), newFanOutTestStore(t)
	before, err := workerStore.ReadProviderConfigBytes("ollama")
	require.NoError(t, err)
	_, err = workerStore.WriteAsset("ollama", "keep.json", []byte("keep"))
	require.NoError(t, err)
	schemaPath := filepath.Join(workerStore.DirPath(), "external", "ollama", "schema.yaml")
	require.NoError(t, os.WriteFile(schemaPath, []byte("parameters: {}\n"), 0600))
	// These bytes model a sender contract the receiver no longer understands.
	path := filepath.Join(store.DirPath(), "external", "ollama", "config.yaml")
	require.NoError(t, os.WriteFile(path, append(append([]byte{}, before...), []byte("future_managed_key: newer\n")...), 0600))
	worker := &Server{node: &NodeIdentity{name: "worker", mode: pkgConfig.ClusterModeWorker}, configStore: workerStore}
	engine := gin.New()
	engine.POST("/zzrouter/v1/internal/sync/providers/:name", worker.HandleProviderSync)
	var posted atomic.Int32
	client := &http.Client{Transport: syncTestTransport(func(r *http.Request) (*http.Response, error) {
		posted.Add(1)
		out := httptest.NewRecorder()
		engine.ServeHTTP(out, r)
		assert.Equal(t, http.StatusBadRequest, out.Code)
		return out.Result(), nil
	})}
	f := newProviderSyncFanOut(store, func() bool { return true }, func() []*mesh.Endpoint {
		return []*mesh.Endpoint{{URL: "https://worker.invalid", NodeName: "worker"}}
	}, func() *http.Client { return client }, compatibleSyncClusterClient)
	f.fire(store.Config())
	assert.Equal(t, SyncReport{State: SyncPartial}, awaitSyncTest(t, f))
	assert.EqualValues(t, 1, posted.Load(), "the probe passed; receiver validation must be the backstop")
	after, err := workerStore.ReadProviderConfigBytes("ollama")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	afterSchema, err := os.ReadFile(schemaPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("parameters: {}\n"), afterSchema)
	assetDir, err := workerStore.Assets("ollama")
	require.NoError(t, err)
	set, err := assetDir.ReadAll()
	require.NoError(t, err)
	assert.Equal(t, map[string][]byte{"keep.json": []byte("keep")}, set)
}
