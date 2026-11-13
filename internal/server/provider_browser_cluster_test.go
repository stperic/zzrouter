package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic installed files and a launch receipt isolate the transport boundary.
// Actual receipt creation is exercised by prov_apps' memory-admitted prepareRun tests.
func seedBrowserTransportRun(t *testing.T, worker *Server, port int) *instance.Instance {
	t.Helper()
	previous := fsroot.ProviderRootDir()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previous) })
	_, err := worker.configStore.SetProviderEnabled("comfyui", "", true)
	require.NoError(t, err)
	svc, ok := worker.appsConfig().LookupApp("comfyui")
	require.True(t, ok)
	snapshot, err := install.ResolveRecipe(svc, "worker", "comfyui", "", nil)
	require.NoError(t, err)
	manifest := &fsroot.InstallManifest{Version: snapshot.Recipe.Source.Version, WorkingDir: snapshot.WorkingDir, Recipe: &snapshot.Recipe, RecipeFingerprint: snapshot.Fingerprint, PlanID: "transport-fixture", Interpreter: &fsroot.ManifestInterpreter{Path: "python", Version: "3.14.0"}, Inventory: []fsroot.ManifestPackage{{Name: "torch", Version: "2.9"}}}
	require.NoError(t, fsroot.WriteInstallManifest("comfyui", manifest))
	python := fsroot.ProviderVenvPython("comfyui")
	require.NoError(t, os.MkdirAll(filepath.Dir(python), 0700))
	require.NoError(t, os.WriteFile(python, []byte("synthetic interpreter"), 0700))
	run := instance.NewInstance("shared-browser", "comfyui", "", port, 0, 1)
	physicalEndpoint, _, err := worker.providers.appMgr.Upstream(run)
	require.NoError(t, err)
	run.SetResolved(instance.Resolved{Runtime: "comfyui", Browser: &instance.BrowserReceipt{Endpoint: physicalEndpoint, Name: "ComfyUI", InstallPlanID: manifest.PlanID, SourceSHA256: manifest.Recipe.Source.SHA256, RecipeFingerprint: manifest.RecipeFingerprint, Contract: install.Fingerprint(svc.Runtime.Browser), Execution: "synthetic-final-launch"}})
	run.SetStatus(instance.StatusRunning)
	require.NoError(t, worker.providers.appMgr.Instances().Register(run))
	t.Cleanup(func() { run.Cancel(); _ = worker.providers.appMgr.Instances().Remove(run.ID) })
	_, _, err = worker.providers.appMgr.BrowserRun(t.Context(), run.ID)
	require.NoError(t, err)
	return run
}

func TestProviderBrowserRealMTLSHTTPAndWebSocketLifecycle(t *testing.T) {
	for _, reason := range []string{"revocation", "demotion"} {
		t.Run(reason, func(t *testing.T) {
			var calls atomic.Int32
			backendClosed := make(chan struct{})
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "comfy.example.com", r.Host)
				for _, name := range []string{"Cookie", "Authorization", "X-API-Key", "X-Cluster-API-Key", "Forwarded", "X-Forwarded-Host", "X-Zzrouter-Node"} {
					assert.Empty(t, r.Header.Get(name), name)
				}
				if r.URL.Path != "/ws" {
					assert.Equal(t, "/view/a%2Fb", r.URL.EscapedPath())
					assert.Equal(t, "filename=a%2Bb.png", r.URL.RawQuery)
					_, _ = w.Write([]byte{0, 128, 255})
					return
				}
				upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "https://comfy.example.com" }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer close(backendClosed)
				defer conn.Close()
				kind, body, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if conn.WriteMessage(kind, body) != nil {
					return
				}
				_, _, _ = conn.ReadMessage()
			}))
			defer backend.Close()
			endpoint, err := url.Parse(backend.URL)
			require.NoError(t, err)
			port, err := strconv.Atoi(endpoint.Port())
			require.NoError(t, err)
			worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, NodeName: "worker", ClusterMode: config.ClusterModeWorker}, providers: []string{"comfyui"}, enabled: []string{"comfyui"}})
			coordinator, g, _, issuer, _ := sharedBrowserAuthorityFixture(t, "", TestNodeConfig{NodeName: "coordinator", ClusterMode: config.ClusterModeCoordinator})
			key := loadTestKey(t, coordinator, "provider-browser-cluster-reader", keys.VirtualKey{Name: "Cluster browser reader"})
			run := seedBrowserTransportRun(t, worker, port)
			owner := transportNode(t, clusternode.Coordinator, "coordinator", nil)
			workerFinished := make(chan struct{})
			workerNode := transportNode(t, clusternode.Unclaimed, "worker", func(n *clusternode.Node) http.Handler {
				inner := buildInternalEngine(worker)
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					inner.ServeHTTP(w, r)
					if strings.HasSuffix(r.URL.Path, "/ws") {
						close(workerFinished)
					}
				})
			})
			workerURL := enrollTransportNode(t, owner, workerNode)
			coordinator.cluster.listener = owner
			if coordinator.cluster.coordinator != nil {
				coordinator.cluster.coordinator.Stop()
			}
			cluster, err := mesh.NewCluster(&mesh.Config{NodeName: "coordinator"}, "http://coordinator.invalid", coordinator, nil, mesh.NewConnector(mesh.ConnectorConfig{ClusterPort: workerNode.Addr().(*net.TCPAddr).Port}))
			require.NoError(t, err)
			t.Cleanup(cluster.Stop)
			coordinator.cluster.coordinator = cluster
			require.NoError(t, cluster.RegisterEndpoint(&mesh.Endpoint{URL: workerURL, ClusterURL: workerURL, NodeName: "worker", Status: mesh.StatusUp}))
			coordinator.providerBrowser.gateway = g
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				require.NoError(t, coordinator.stopProviderBrowser(ctx))
			})
			location, proof := beginBrowserAuthorizationAt(t, g, "/runs/worker/shared-browser/")
			callback := approveSharedBrowser(t, issuer, location, key)
			callbackRequest := httptest.NewRequest(http.MethodGet, callback.String(), nil)
			callbackRequest.AddCookie(proof)
			authorized := httptest.NewRecorder()
			g.ServeHTTP(authorized, callbackRequest)
			require.Equal(t, http.StatusSeeOther, authorized.Code, authorized.Body.String())
			cookie := authorized.Result().Cookies()[0]
			gatewayFinished := make(chan struct{})
			public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				g.ServeHTTP(w, r)
				if strings.HasSuffix(r.URL.Path, "/ws") {
					close(gatewayFinished)
				}
			}))
			defer public.Close()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, public.URL+"/runs/worker/shared-browser/view/a%2Fb?filename=a%2Bb.png", nil)
			require.NoError(t, err)
			req.Host = g.origin.Host
			req.AddCookie(cookie)
			req.Header.Set("Authorization", "Bearer private")
			response, err := public.Client().Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusOK, response.StatusCode, string(body))
			require.Equal(t, []byte{0, 128, 255}, body)
			closed := httptest.NewRecorder()
			worker.engine.ServeHTTP(closed, httptest.NewRequest(http.MethodGet, "/zzrouter/v1/internal/runs/shared-browser/browser/view", nil))
			require.Equal(t, http.StatusNotFound, closed.Code)
			conn, handshake, err := websocket.DefaultDialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(public.URL, "http")+"/runs/worker/shared-browser/ws", http.Header{"Host": {g.origin.Host}, "Origin": {g.origin.String()}, "Cookie": {cookie.Name + "=" + cookie.Value}})
			if handshake != nil {
				defer handshake.Body.Close()
			}
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetReadDeadline(utils.Now().Add(20*time.Second)))
			payload := []byte{0, 1, 128, 255}
			require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, payload))
			kind, echo, err := conn.ReadMessage()
			require.NoError(t, err)
			require.Equal(t, websocket.BinaryMessage, kind)
			require.Equal(t, payload, echo)
			if reason == "revocation" {
				_, err := coordinator.keyStore.RotateKey("provider-browser-cluster-reader")
				require.NoError(t, err)
			} else {
				require.NoError(t, coordinator.role.Set(t.Context(), role.RoleWorker, "demote with active browser socket"))
			}
			_, _, err = conn.ReadMessage()
			require.Error(t, err)
			for _, done := range []<-chan struct{}{gatewayFinished, workerFinished, backendClosed} {
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("browser socket owner did not drain")
				}
			}
			require.Zero(t, worker.connManager.GetStats().StreamActiveRequests)
			run.Cancel()
			require.EqualValues(t, 2, calls.Load())
		})
	}
}

func TestProviderBrowserAdmissionCannotRetargetAfterConfigReload(t *testing.T) {
	var replacementCalls atomic.Int32
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/view" {
			replacementCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.2.3","models":[]}`))
	}))
	defer replacement.Close()
	original := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("original owned engine")) }))
	defer original.Close()
	originalURL, err := url.Parse(original.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(originalURL.Port())
	require.NoError(t, err)
	worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, NodeName: "worker", ClusterMode: config.ClusterModeWorker}, providers: []string{"comfyui"}, enabled: []string{"comfyui"}})
	run := seedBrowserTransportRun(t, worker, port)
	admitted, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	worker.providers.browserAdmitted = func() { close(admitted); <-release }
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { worker.serveProviderRunBrowser(w, r, run.ID, r.URL, nil) }))
	defer func() { once.Do(func() { close(release) }); proxy.Close() }()
	type result struct {
		status int
		body   string
		err    error
	}
	response := make(chan result, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxy.URL+"/view", nil)
		if err != nil {
			response <- result{err: err}
			return
		}
		resp, err := proxy.Client().Do(req)
		if err != nil {
			response <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		response <- result{resp.StatusCode, string(body), err}
	}()
	select {
	case <-admitted:
	case <-time.After(3 * time.Second):
		t.Fatal("browser admission did not complete")
	}
	fresh := &config.AppsConfig{}
	require.NoError(t, fresh.AddApp("comfyui", config.ServiceConfig{Name: "comfyui", Mode: "external", Protocol: config.ProtocolOllama, Enabled: new(true), Runtime: &config.AppRuntimeConfig{Endpoint: replacement.URL, API: &config.APIConfig{AuthType: config.AuthTypeNone}}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
	worker.configStore = config.NewMemoryAppsConfigStore(fresh)
	worker.providers.appMgr.ReloadConfig(fresh)
	now, _, err := worker.providers.appMgr.Upstream(run)
	require.NoError(t, err)
	require.Equal(t, replacement.URL, now, "ordinary config-dependent upstream changed during admission")
	once.Do(func() { close(release) })
	select {
	case got := <-response:
		require.NoError(t, got.err)
		require.Equal(t, http.StatusOK, got.status)
		require.Equal(t, "original owned engine", got.body)
	case <-time.After(3 * time.Second):
		t.Fatal("browser forward did not finish")
	}
	require.Zero(t, replacementCalls.Load(), "a config replacement must receive no request from the old browser URL")
}
