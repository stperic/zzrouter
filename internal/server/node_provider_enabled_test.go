package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

// peerCertHandler serves h as if each request presented cert as its
// verified client certificate. The listener's certificate gate itself is
// tested separately; this fixture only decides which worker is calling.
func peerCertHandler(cert *x509.Certificate, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cert != nil {
			state := tls.ConnectionState{}
			if r.TLS != nil {
				state = *r.TLS
			}
			state.PeerCertificates = []*x509.Certificate{cert}
			state.VerifiedChains = [][]*x509.Certificate{{cert}}
			r.TLS = &state
		}
		h.ServeHTTP(w, r)
	})
}

func workerCert(name string) *x509.Certificate {
	return &x509.Certificate{Subject: pkix.Name{CommonName: name}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
}

type enableCluster struct {
	coord, worker *Server
	workerSrv     *httptest.Server
}

// testWorkerInternalHandler changes only the version: plain test builds advertise
// 0.0.0-dev, which Connector.attemptConnection's CheckCompatibility rejects below 0.1.0.
func testWorkerInternalHandler(t *testing.T, worker *Server) http.Handler {
	t.Helper()
	engine := buildInternalEngine(worker)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zzrouter/v1/internal/version" {
			info := version.GetCurrentVersionInfo(version.ServiceTypeHost, version.ServerCapabilities)
			info.Version = version.ClusterCompatibility.MinSupportedVersion
			require.NoError(t, json.NewEncoder(w).Encode(info))
			return
		}
		engine.ServeHTTP(w, r)
	})
}

// newEnableCluster wires a coordinator and a worker the way the cluster
// listener does: the coordinator pushes the provider tree to the worker's
// internal engine, and the worker reaches the coordinator's worker-facing
// engine presenting cert.
func newEnableCluster(t *testing.T, cert *x509.Certificate) enableCluster {
	t.Helper()
	worker := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "worker", ClusterMode: pkgConfig.ClusterModeWorker, SeedProvidersDir: true})
	workerSrv := httptest.NewTLSServer(testWorkerInternalHandler(t, worker))
	t.Cleanup(workerSrv.Close)
	workerPort := workerSrv.Listener.Addr().(*net.TCPAddr).Port

	coord := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "coord", ClusterMode: pkgConfig.ClusterModeCoordinator, SeedProvidersDir: true})
	// A running node has already persisted its normalized provider tree.
	require.NoError(t, coord.appsConfig().SaveToDir(coord.configStore.DirPath()))
	require.NoError(t, worker.appsConfig().SaveToDir(worker.configStore.DirPath()))
	cluster, err := mesh.NewCluster(&mesh.Config{NodeName: "coord"}, "http://coord.invalid", coord, nil, mesh.NewConnector(mesh.ConnectorConfig{
		HTTPClient: workerSrv.Client(), ClusterPort: workerPort,
	}))
	require.NoError(t, err)
	coord.cluster.coordinator = cluster
	t.Cleanup(cluster.Stop)
	coord.clusterMTLSClient = workerSrv.Client()
	require.NoError(t, cluster.RegisterEndpoint(&mesh.Endpoint{URL: workerSrv.URL, ClusterURL: workerSrv.URL, NodeName: "worker", Status: mesh.StatusUp}))

	coordSrv := httptest.NewTLSServer(peerCertHandler(cert, buildCoordInternalEngine(coord)))
	t.Cleanup(coordSrv.Close)
	worker.cluster.signaler = clustersignal.New(clustersignal.Config{
		IsWorker:        func() bool { return true },
		MTLSClient:      coordSrv.Client,
		CoordinatorURL:  func() string { return coordSrv.URL },
		WorkerPublicURL: func() string { return workerSrv.URL },
	})
	worker.cluster.signaler.Start(t.Context())
	t.Cleanup(func() { worker.cluster.signaler.Stop(context.Background()) })
	return enableCluster{coord: coord, worker: worker, workerSrv: workerSrv}
}

func runsProvider(t *testing.T, s *Server, name string) bool {
	t.Helper()
	sc, ok := s.configStore.Config().LookupApp(name)
	require.True(t, ok)
	return sc.IsEnabled()
}

func providerFile(t *testing.T, s *Server, kind, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(s.configStore.DirPath(), kind, name, "config.yaml"))
	require.NoError(t, err)
	return body
}

// A worker's install enabled the provider in its cached copy of the tree,
// and the coordinator's next sync put its own value back. The choice now
// lives in the coordinator's tree, so the sync carries it.
func TestWorkerEnableChoiceSurvivesProviderSync(t *testing.T) {
	c := newEnableCluster(t, workerCert("worker"))
	require.False(t, runsProvider(t, c.worker, "vllm"), "the template ships vllm disabled")

	require.NoError(t, c.worker.FinalizeOnboarding("vllm"))
	require.True(t, runsProvider(t, c.worker, "vllm"), "finalization returns once the worker runs with it")
	require.False(t, runsProvider(t, c.coord, "vllm"), "the choice is the worker's, not the coordinator's")
	require.Equal(t, providerFile(t, c.coord, "on-demand", "vllm"), providerFile(t, c.worker, "on-demand", "vllm"),
		"the worker holds the coordinator's bytes")

	c.coord.reconcileProviderTree("test")
	require.Equal(t, SyncComplete, c.coord.awaitProviderSync(context.Background()).State)
	require.True(t, runsProvider(t, c.worker, "vllm"))

	require.NoError(t, c.worker.FinalizeOffboarding("vllm"))
	require.False(t, runsProvider(t, c.worker, "vllm"))
	c.coord.reconcileProviderTree("test")
	require.Equal(t, SyncComplete, c.coord.awaitProviderSync(context.Background()).State)
	require.False(t, runsProvider(t, c.worker, "vllm"))
}

func TestCoordinatorRecordsOnlyACertifiedWorkersOwnChoice(t *testing.T) {
	for _, tc := range []struct {
		name string
		cert *x509.Certificate
	}{
		{"no certificate", nil},
		{"unknown node", workerCert("other")},
		{"the coordinator's name", workerCert("coord")},
		{"address the certificate does not cover", &x509.Certificate{Subject: pkix.Name{CommonName: "worker"}, IPAddresses: []net.IP{net.ParseIP("192.0.2.10")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEnableCluster(t, tc.cert)
			before := providerFile(t, c.coord, "on-demand", "vllm")
			err := c.worker.FinalizeOnboarding("vllm")
			require.ErrorContains(t, err, "403")
			require.ErrorContains(t, err, "does not name a worker of this cluster")
			require.Equal(t, before, providerFile(t, c.coord, "on-demand", "vllm"), "nothing is recorded")
			require.False(t, runsProvider(t, c.worker, "vllm"))
		})
	}
}

// The coordinator answers only once its sync reached the worker; a choice
// it recorded but could not deliver is a failed finalization, not a
// success the worker does not run with.
func TestWorkerEnableFailsWhenTheSyncDoesNotLand(t *testing.T) {
	c := newEnableCluster(t, workerCert("worker"))
	c.coord.clusterMTLSClient = &http.Client{Transport: syncTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}, nil
	})}
	err := c.worker.FinalizeOnboarding("vllm")
	var refused *clustersignal.StatusError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, http.StatusServiceUnavailable, refused.Status)
	require.Equal(t, string(httperr.CodeProviderSyncPending), refused.Code)
	require.ErrorContains(t, err, "has not reached worker")
	require.False(t, runsProvider(t, c.worker, "vllm"))

	// The worker hands the coordinator's answer on unchanged.
	relayed := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/zzrouter/v1/internal/providers/vllm", strings.NewReader(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	buildInternalEngine(c.worker).ServeHTTP(relayed, request)
	require.Equal(t, http.StatusServiceUnavailable, relayed.Code, relayed.Body.String())
	require.Contains(t, relayed.Body.String(), `"code":"provider_sync_pending"`)
	choice := c.coord.configStore.Config().GetOnDemand("vllm").Nodes["worker"].Enabled
	require.True(t, choice != nil && *choice, "the coordinator keeps the choice for the next sync")

	// A retry changes nothing on the coordinator, and still delivers.
	c.coord.clusterMTLSClient = c.workerSrv.Client()
	require.NoError(t, c.worker.FinalizeOnboarding("vllm"))
	require.True(t, runsProvider(t, c.worker, "vllm"))
}

// Sync never carries cloud providers or registries, so a worker's copy is
// its own: it enables it locally, and the coordinator refuses to record
// such a choice for it.
func TestWorkerOwnsProvidersTheCoordinatorDoesNotSync(t *testing.T) {
	c := newEnableCluster(t, workerCert("worker"))
	before := providerFile(t, c.coord, "cloud", "openai")
	require.NoError(t, c.worker.FinalizeOnboarding("openai"))
	require.True(t, runsProvider(t, c.worker, "openai"))
	require.Equal(t, before, providerFile(t, c.coord, "cloud", "openai"))

	err := c.worker.cluster.signaler.RecordProviderEnabled(context.Background(), "openai", true)
	var refused *clustersignal.StatusError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, http.StatusForbidden, refused.Status)
	require.Equal(t, before, providerFile(t, c.coord, "cloud", "openai"))
}

// Sleeping workers accept TCP connections without answering; their protocol probes must not hold push slots.
func TestWorkerEnableLandsWhileOtherWorkersHang(t *testing.T) {
	c := newEnableCluster(t, workerCert("worker"))
	hung, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = hung.Close() })
	go func() {
		for {
			conn, err := hung.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	for _, name := range []string{"sleeper-1", "sleeper-2"} {
		url := "https://" + hung.Addr().String() + "/" + name
		require.NoError(t, c.coord.cluster.coordinator.RegisterEndpoint(&mesh.Endpoint{URL: url, ClusterURL: url, NodeName: name, Status: mesh.StatusUp}))
	}

	require.NoError(t, c.worker.FinalizeOnboarding("vllm"))
	require.True(t, roundOpen(c.coord.providers.syncFanOut), "the enable waited for the round to give up on the sleepers")
	require.True(t, runsProvider(t, c.worker, "vllm"))
	for _, ep := range c.coord.GetClusterEndpoints() {
		if ep.NodeName == "worker" {
			require.Equal(t, mesh.StatusUp, ep.Status, "the narrowed re-probe must keep the worker reachable")
		}
	}
}

// roundOpen reports whether f still has a push round running; the
// sleepers keep one open until their pushes time out.
func roundOpen(f *providerSyncFanOut) bool {
	f.mu.Lock()
	idle := f.idle
	f.mu.Unlock()
	select {
	case <-idle:
		return false
	default:
		return true
	}
}

func TestWorkerEnableDeadlineIncludesConfigMutation(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{NodeName: "coord", ClusterMode: pkgConfig.ClusterModeCoordinator, SeedProvidersDir: true})
	store, err := pkgConfig.NewAppsConfigStore(s.configStore.DirPath())
	require.NoError(t, err)
	s.configStore = store
	require.NoError(t, s.cluster.coordinator.RegisterEndpoint(&mesh.Endpoint{URL: "https://127.0.0.1:9443", NodeName: "worker", Status: mesh.StatusUp}))
	synctest.Test(t, func(t *testing.T) {
		mutated, returned, idle := make(chan struct{}), make(chan struct{}), make(chan struct{})
		// AwaitWorker relies on writes == current.carries, no landed worker, and an open idle channel.
		s.providers.syncFanOut = &providerSyncFanOut{writes: 1, current: &roundProgress{carries: 1}, idle: idle}
		store.OnChange("slow mutation", func(*pkgConfig.AppsConfig) pkgConfig.ReloadDisposition {
			time.Sleep(providerSyncWaitTimeout)
			close(mutated)
			return pkgConfig.DispositionApplied
		})
		c, rec := mutationContext(t, http.MethodPut, "/zzrouter/v1/internal/providers/vllm/enabled", map[string]bool{"enabled": true})
		c.Params = gin.Params{{Key: "name", Value: "vllm"}}
		cert := workerCert("worker")
		c.Request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		go func() { s.handleNodeProviderEnabled(c); close(returned) }()
		t.Cleanup(func() { close(idle); <-returned })
		<-mutated
		synctest.Wait()
		select {
		case <-returned:
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		default:
			t.Fatal("enable started a new wait window after the mutation consumed its deadline")
		}
		select {
		case <-idle:
			t.Fatal("the sync round must still be pending when the deadline answers")
		default:
		}
	})
}
