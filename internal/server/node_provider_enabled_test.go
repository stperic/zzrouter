package server

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stretchr/testify/require"
)

// peerCertHandler serves h as if each request presented cert as its
// verified client certificate. The listener's certificate gate itself is
// tested separately; this fixture only decides which worker is calling.
func peerCertHandler(cert *x509.Certificate, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cert != nil {
			state := *r.TLS
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

// newEnableCluster wires a coordinator and a worker the way the cluster
// listener does: the coordinator pushes the provider tree to the worker's
// internal engine, and the worker reaches the coordinator's worker-facing
// engine presenting cert.
func newEnableCluster(t *testing.T, cert *x509.Certificate) enableCluster {
	t.Helper()
	worker := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "worker", ClusterMode: pkgConfig.ClusterModeWorker, SeedProvidersDir: true})
	workerSrv := httptest.NewTLSServer(buildInternalEngine(worker))
	t.Cleanup(workerSrv.Close)

	coord := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "coord", ClusterMode: pkgConfig.ClusterModeCoordinator, SeedProvidersDir: true})
	cluster, err := mesh.NewCluster(&mesh.Config{NodeName: "coord"}, "http://coord.invalid", coord, nil, mesh.NewConnector(mesh.ConnectorConfig{HTTPClient: workerSrv.Client()}))
	require.NoError(t, err)
	coord.cluster.coordinator = cluster
	coord.clusterMTLSClient = workerSrv.Client()
	require.NoError(t, cluster.RegisterEndpoint(&mesh.Endpoint{URL: workerSrv.URL, ClusterURL: workerSrv.URL, NodeName: "worker", Status: mesh.StatusUp}))

	coordSrv := httptest.NewTLSServer(peerCertHandler(cert, buildCoordInternalEngine(coord)))
	t.Cleanup(coordSrv.Close)
	worker.cluster.signaler = clustersignal.New(clustersignal.Config{
		IsWorker:       func() bool { return true },
		MTLSClient:     coordSrv.Client,
		CoordinatorURL: func() string { return coordSrv.URL },
	})
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

// Sleeping workers accept connections and never answer, and a round
// stays open until they time out. The worker asking for its own choice
// still gets it at once: two of them hold more goroutines than the round
// has push slots, so their protocol probes must not hold one.
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

	started := time.Now()
	require.NoError(t, c.worker.FinalizeOnboarding("vllm"))
	require.Less(t, time.Since(started), providerSyncPushTimeout/2, "no wait on the sleepers")
	require.True(t, runsProvider(t, c.worker, "vllm"))
}
