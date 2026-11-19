package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/keys"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/affinity"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

var responseResourceRequests = []struct{ method, suffix string }{
	{http.MethodGet, ""}, {http.MethodDelete, ""},
	{http.MethodGet, "/input_items"}, {http.MethodPost, "/cancel"},
}

func TestResponseAffinity_CreateFreezesConnectionBeforeReply(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			started, finish := make(chan struct{}), make(chan struct{})
			var oldCalls, replacementCalls atomic.Int32
			old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
					return
				}
				oldCalls.Add(1)
				close(started)
				<-finish
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_frozen\"}}\n\n"))
					w.(http.Flusher).Flush()
					_, _ = w.Write([]byte("data: {\"delta\":\"" + strings.Repeat("x", defaultResponseBodyCaptureCap+1) + "\"}\n\ndata: [DONE]\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"output":[{"id":"nested-decoy","text":"` + strings.Repeat("x", defaultResponseBodyCaptureCap+1) + `"}],"id":"resp_frozen","object":"response"}`))
			}))
			t.Cleanup(old.Close)
			replacement := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { replacementCalls.Add(1) }))
			t.Cleanup(replacement.Close)
			s := createTestNodeWithDefaults(t)
			cfg := &config.AppsConfig{}
			require.NoError(t, cfg.AddApp("origin", config.ServiceConfig{Enabled: new(true), Mode: "service", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: old.URL}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"responses"}}}))
			s.configStore = config.NewMemoryAppsConfigStore(cfg)
			s.providers.appMgr.ReloadConfig(cfg)
			s.model.Resolver = staticModelResolver{provider: "origin"}
			before, ok := s.backend.Resolve("origin")
			require.True(t, ok)
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				result <- postAsAdmin(t, s, "/v1/responses", fmt.Sprintf(`{"model":"m","input":"hi","stream":%t}`, stream))
			}()
			<-started
			require.NoError(t, cfg.UpdateApp("origin", func(sc *config.ServiceConfig) error { sc.Runtime.Endpoint = replacement.URL; return nil }))
			close(finish)
			created := <-result
			require.Equal(t, http.StatusOK, created.Code, created.Body.String())
			origin, ok := s.inference.affinity.Lookup("resp_frozen", affinity.Owner{Kind: "static", ID: "admin"})
			require.True(t, ok, "JSON and long SSE creates must record the response identifier")
			require.Equal(t, before.Revision, origin.ConnectionRevision, "record the actual pre-send connection, not the reconfigured alias")
			s.config.OpenAICompat.DefaultBackend = "origin"
			for _, operation := range responseResourceRequests {
				response := makeAuthRequest(t, s, operation.method, "/v1/responses/resp_frozen"+operation.suffix, TestAdminKey, nil)
				require.Equal(t, http.StatusConflict, response.Code, string(response.Body))
				require.Contains(t, string(response.Body), "response_origin_unavailable")
			}
			continued := postAsAdmin(t, s, "/v1/responses", `{"model":"m","input":"continue","previous_response_id":"resp_frozen"}`)
			require.Equal(t, http.StatusConflict, continued.Code, continued.Body.String())
			require.EqualValues(t, 1, oldCalls.Load())
			require.Zero(t, replacementCalls.Load(), "no operation may move to the replacement or default connection")
		})
	}
}

func TestResponseAffinity_OwnerAndTenantIsolation(t *testing.T) {
	backend := &serviceBackend{wire: []string{"responses"}, status: http.StatusOK, reply: `{"id":"resp_private","object":"response"}`}
	s := newServiceGroupNode(t, []string{"origin"}, map[string]*serviceBackend{"origin": backend})
	for _, name := range []string{"a", "b"} {
		_, err := s.services.Teams.CreateTeam(t.Context(), &CreateTeamRequest{ID: name, Name: name}, "admin")
		require.NoError(t, err)
	}
	creator := loadTestKey(t, s, "response-owner", keys.VirtualKey{Name: "Creator", Role: "user", TeamID: "a"})
	others := []string{TestAdminKey,
		seedTestKey(t, s.accessStore, "same-tenant-other-key", keys.VirtualKey{Name: "Peer", Role: "user", TeamID: "a", TeamRole: "member"}),
		seedTestKey(t, s.accessStore, "other-tenant", keys.VirtualKey{Name: "Other tenant", Role: "user", TeamID: "b", TeamRole: "member"}),
	}
	create := makeAuthRequest(t, s, http.MethodPost, "/v1/responses", creator, map[string]any{"model": "claude", "input": "private"})
	require.Equal(t, http.StatusOK, create.Code, string(create.Body))
	for _, caller := range others {
		for _, operation := range responseResourceRequests {
			response := makeAuthRequest(t, s, operation.method, "/v1/responses/resp_private"+operation.suffix, caller, nil)
			require.Equal(t, http.StatusNotFound, response.Code, string(response.Body))
		}
		response := makeAuthRequest(t, s, http.MethodPost, "/v1/responses", caller, map[string]any{"model": "claude", "input": "steal", "previous_response_id": "resp_private"})
		require.Equal(t, http.StatusNotFound, response.Code, string(response.Body))
	}
	require.Len(t, backend.requests(), 1, "other principals cannot reach the resource's backend")
	for _, operation := range responseResourceRequests {
		if operation.method == http.MethodDelete {
			continue
		}
		response := makeAuthRequest(t, s, operation.method, "/v1/responses/resp_private"+operation.suffix, creator, nil)
		require.Equal(t, http.StatusOK, response.Code, string(response.Body))
	}
	// A failed delete preserves its binding; a successful delete forgets it.
	backend.mu.Lock()
	backend.status = http.StatusServiceUnavailable
	backend.mu.Unlock()
	failed := makeAuthRequest(t, s, http.MethodDelete, "/v1/responses/resp_private", creator, nil)
	require.Equal(t, http.StatusServiceUnavailable, failed.Code, string(failed.Body))
	require.Equal(t, 1, s.inference.affinity.Size())
	backend.mu.Lock()
	backend.status = http.StatusOK
	backend.mu.Unlock()
	deleted := makeAuthRequest(t, s, http.MethodDelete, "/v1/responses/resp_private", creator, nil)
	require.Equal(t, http.StatusOK, deleted.Code, string(deleted.Body))
	missing := makeAuthRequest(t, s, http.MethodGet, "/v1/responses/resp_private", creator, nil)
	require.Equal(t, http.StatusNotFound, missing.Code, string(missing.Body))
	// Existing anonymous stateless inference remains available.
	stateless := makeAuthRequest(t, s, http.MethodPost, "/v1/responses", "", map[string]any{"model": "claude", "input": "hi", "store": false})
	require.Equal(t, http.StatusOK, stateless.Code, string(stateless.Body))
}

func responseRunNode(t *testing.T, port int, name string, mode config.ClusterMode) (*Server, *instance.Instance) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "qwen.gguf"), []byte("GGUF"), 0o600))
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: mode, NodeName: name}, modelsDir: root})
	_, err := s.configStore.SetProviderEnabled("llamacpp", "", true)
	require.NoError(t, err)
	require.NoError(t, s.appsConfig().UpdateApp("llamacpp", func(sc *config.ServiceConfig) error {
		sc.Capabilities.WireEndpoints = []string{"responses"}
		return nil
	}))
	s.providers.appMgr.ReloadConfig(s.appsConfig())
	s.model.Resolver = staticModelResolver{provider: "llamacpp"}
	run := instance.NewInstance(name+"-origin", "llamacpp", "qwen", port, 0, 0)
	run.WireModel = "qwen"
	run.MarkRunning(false, nil)
	require.NoError(t, s.providers.appMgr.Instances().Register(run))
	t.Cleanup(func() { _ = s.providers.appMgr.Instances().Remove(run.ID) })
	return s, run
}

func TestResponseAffinity_LocalRunCannotMoveToReplacement(t *testing.T) {
	var calls atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_run","object":"response"}`))
	}))
	t.Cleanup(engine.Close)
	u, err := url.Parse(engine.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	s, run := responseRunNode(t, port, "local", config.ClusterModeDisabled)
	created := postAsAdmin(t, s, "/v1/responses", `{"model":"qwen","input":"hi"}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	origin, ok := s.inference.affinity.Lookup("resp_run", affinity.Owner{Kind: "static", ID: "admin"})
	require.True(t, ok)
	require.Equal(t, run.ID, origin.RunID)
	for _, operation := range responseResourceRequests {
		if operation.method == http.MethodDelete {
			continue
		}
		response := makeAuthRequest(t, s, operation.method, "/v1/responses/resp_run"+operation.suffix, TestAdminKey, nil)
		require.Equal(t, http.StatusOK, response.Code, string(response.Body))
	}
	continued := postAsAdmin(t, s, "/v1/responses", `{"model":"qwen","input":"again","previous_response_id":"resp_run"}`)
	require.Equal(t, http.StatusOK, continued.Code, continued.Body.String())
	require.NoError(t, s.providers.appMgr.Instances().Remove(run.ID))
	replacement := instance.NewInstance("replacement", "llamacpp", "qwen", port, 0, 0)
	replacement.MarkRunning(false, nil)
	require.NoError(t, s.providers.appMgr.Instances().Register(replacement))
	t.Cleanup(func() { _ = s.providers.appMgr.Instances().Remove(replacement.ID) })
	before := calls.Load()
	for _, operation := range responseResourceRequests {
		response := makeAuthRequest(t, s, operation.method, "/v1/responses/resp_run"+operation.suffix, TestAdminKey, nil)
		require.Equal(t, http.StatusConflict, response.Code, string(response.Body))
	}
	refused := postAsAdmin(t, s, "/v1/responses", `{"model":"qwen","input":"again","previous_response_id":"resp_run"}`)
	require.Equal(t, http.StatusConflict, refused.Code, refused.Body.String())
	require.Equal(t, before, calls.Load(), "a same-model replacement cannot receive old resource calls")
}

type responseNodeResolver struct{ node, provider string }

func (r responseNodeResolver) Resolve(_ context.Context, ref resolver.Reference) (*resolver.Resolved, error) {
	model, _ := splitOllamaAddr(ref.Model)
	return &resolver.Resolved{ModelName: model, Provider: r.provider, Node: r.node}, nil
}

func responseWorkerMTLS(t *testing.T, worker *Server) (string, *http.Client) {
	t.Helper()
	root := t.TempDir()
	ca, err := clusterid.LoadOrCreateCA(filepath.Join(root, "ca"))
	require.NoError(t, err)
	sign := func(name string, role clusterid.Role) *clusterid.Identity {
		identity, err := clusterid.LoadOrCreateIdentity(filepath.Join(root, name))
		require.NoError(t, err)
		csr, err := identity.CSR(name, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
		require.NoError(t, err)
		cert, err := ca.Sign(csr, role)
		require.NoError(t, err)
		require.NoError(t, identity.InstallSignedCert(cert))
		return identity
	}
	sign("worker", clusterid.RoleWorker)
	coord := sign("coordinator", clusterid.RoleCoordinator)
	clusterDir := filepath.Join(root, "cluster")
	require.NoError(t, os.MkdirAll(clusterDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(clusterDir, "ca.pem"), ca.CertPEM(), 0o600))
	listener, err := clusternode.New(clusternode.Config{Mode: clusternode.Worker, BindHost: "127.0.0.1", IdentityDir: filepath.Join(root, "worker"), ClusterDir: clusterDir, CoordinatorURL: "https://127.0.0.1:1", NodeName: "worker"})
	require.NoError(t, err)
	require.NoError(t, listener.SetAdminAPIHandler(worker.engine))
	require.NoError(t, listener.SetWorkerCompatHandler(buildWorkerCompatEngine(worker)))
	require.NoError(t, listener.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, listener.Stop(context.Background())) })
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{{Certificate: [][]byte{coord.Certificate().Raw}, PrivateKey: coord.Signer()}}}}
	t.Cleanup(transport.CloseIdleConnections)
	return "https://" + listener.Addr().String(), &http.Client{Transport: transport}
}

func TestResponseAffinity_WorkerMTLSKeepsRunAndCaller(t *testing.T) {
	for _, tc := range []struct{ name, provider, model string }{{"known-provider", "llamacpp", "qwen"}, {"uncatalogued-node-pin", "", "qwen@worker"}} {
		t.Run(tc.name, func(t *testing.T) { testResponseWorkerOwnership(t, tc.provider, tc.model) })
	}
}

func testResponseWorkerOwnership(t *testing.T, resolvedProvider, model string) {
	t.Helper()
	var calls atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Empty(t, r.Header.Get(constants.HeaderResponseOwner), "trusted caller identity must stop at the worker")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_worker","object":"response"}`))
	}))
	t.Cleanup(engine.Close)
	u, err := url.Parse(engine.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	coordinator := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord"})
	worker, run := responseRunNode(t, port, "worker", config.ClusterModeWorker)
	base, client := responseWorkerMTLS(t, worker)
	coordinator.clusterMTLSClient = client
	require.NoError(t, coordinator.cluster.coordinator.RegisterEndpoint(&mesh.Endpoint{Name: "worker", URL: "http://worker.invalid:9090", ClusterURL: base}))
	provider, ok := worker.appsConfig().LookupApp("llamacpp")
	require.True(t, ok)
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("llamacpp", provider))
	coordinator.configStore = config.NewMemoryAppsConfigStore(cfg)
	coordinator.model.Resolver = responseNodeResolver{node: "worker", provider: resolvedProvider}
	var catalog []map[string]any
	if resolvedProvider != "" {
		catalog = []map[string]any{replicaItem("worker", "llamacpp")}
	}
	routingCatalog(t, coordinator, catalog)
	created := makeAuthRequest(t, coordinator, http.MethodPost, "/v1/responses", TestUserKey, map[string]any{"model": model, "input": "hi"})
	require.Equal(t, http.StatusOK, created.Code, string(created.Body))
	// A client-supplied owner hint cannot impersonate the creator on either node.
	spoofed := makeRequest(t, coordinator, TestRequest{Method: http.MethodGet, Path: "/v1/responses/resp_worker", Headers: map[string]string{"X-API-Key": TestAdminKey, constants.HeaderResponseOwner: strings.Repeat("a", 64)}})
	require.Equal(t, http.StatusNotFound, spoofed.Code, string(spoofed.Body))
	require.EqualValues(t, 1, calls.Load())
	for _, operation := range responseResourceRequests {
		if operation.method == http.MethodDelete {
			continue
		}
		response := makeAuthRequest(t, coordinator, operation.method, "/v1/responses/resp_worker"+operation.suffix, TestUserKey, nil)
		require.Equal(t, http.StatusOK, response.Code, string(response.Body))
	}
	continued := makeAuthRequest(t, coordinator, http.MethodPost, "/v1/responses", TestUserKey, map[string]any{"model": model, "input": "again", "previous_response_id": "resp_worker"})
	require.Equal(t, http.StatusOK, continued.Code, string(continued.Body))
	require.NoError(t, worker.providers.appMgr.Instances().Remove(run.ID))
	replacement := instance.NewInstance("replacement-worker-run", "llamacpp", "qwen", port, 0, 0)
	replacement.MarkRunning(false, nil)
	require.NoError(t, worker.providers.appMgr.Instances().Register(replacement))
	t.Cleanup(func() { _ = worker.providers.appMgr.Instances().Remove(replacement.ID) })
	before := calls.Load()
	for _, operation := range responseResourceRequests {
		response := makeAuthRequest(t, coordinator, operation.method, "/v1/responses/resp_worker"+operation.suffix, TestUserKey, nil)
		require.Equal(t, http.StatusConflict, response.Code, string(response.Body))
	}
	require.Equal(t, before, calls.Load(), "the mTLS worker rejects resource calls before touching a replacement run")
	// A worker restart loses the registry and therefore refuses, even if the model is running.
	worker.inference.affinity.Stop()
	worker.inference.affinity = newResponseAffinity(0)
	missing := makeAuthRequest(t, coordinator, http.MethodGet, "/v1/responses/resp_worker", TestUserKey, nil)
	require.Equal(t, http.StatusNotFound, missing.Code, string(missing.Body))
	require.Equal(t, before, calls.Load())
}
