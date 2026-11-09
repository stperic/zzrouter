package server

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	modelsync "github.com/stperic/zzrouter/pkg/model/sync"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

func transportNode(t *testing.T, mode clusternode.Mode, name string, handler func(*clusternode.Node) http.Handler) *clusternode.Node {
	t.Helper()
	dir := t.TempDir()
	n, err := clusternode.New(clusternode.Config{Mode: mode, BindHost: "127.0.0.1", NodeName: name, IdentityDir: filepath.Join(dir, "identity"), CADir: filepath.Join(dir, "ca"), ClusterDir: filepath.Join(dir, "cluster"), PairingPath: filepath.Join(dir, "pairing.txt")})
	require.NoError(t, err)
	if handler != nil {
		inner := handler(n)
		mux := http.NewServeMux()
		mux.HandleFunc("/zzrouter/v1/internal/version", func(w http.ResponseWriter, r *http.Request) {
			info := version.GetCurrentVersionInfo(version.ServiceTypeHost, version.ServerCapabilities)
			info.Version = version.ClusterCompatibility.MinSupportedVersion
			_ = json.NewEncoder(w).Encode(info)
		})
		mux.Handle("/", inner)
		require.NoError(t, n.SetAdminAPIHandler(mux))
		require.NoError(t, n.SetWorkerCompatHandler(http.NotFoundHandler()))
	}
	require.NoError(t, n.Start(t.Context()))
	t.Cleanup(func() { _ = n.Stop(context.Background()) })
	return n
}
func enrollTransportNode(t *testing.T, coordinator, worker *clusternode.Node) string {
	t.Helper()
	// Deliberately unreachable: no worker operation may need this address.
	owner := "https://127.0.0.1:1"
	window, err := worker.BeginPairing(t.Context(), clusternode.BeginPairingOptions{CoordinatorURL: owner, CAFingerprint: coordinator.CAFingerprint()})
	require.NoError(t, err)
	_, err = coordinator.Enroll(t.Context(), window.WorkerURL, window.Fingerprint, window.Code, owner)
	require.NoError(t, err)
	_, err = worker.DialClient(clusterid.RoleCoordinator.OU())
	require.ErrorIs(t, err, clusternode.ErrInvalidConfig)
	return window.WorkerURL
}
func registerTestExchange(engine *gin.Engine, n *clusternode.Node) {
	engine.POST("/zzrouter/v1/internal/signals/poll", func(c *gin.Context) {
		var poll clustersignal.Poll
		if err := c.ShouldBindJSON(&poll); err != nil {
			c.Status(400)
			return
		}
		batch, err := n.Signals().Wait(c.Request.Context(), poll)
		if err != nil {
			c.Status(503)
			return
		}
		c.JSON(200, batch)
	})
	engine.POST("/zzrouter/v1/internal/signals/ack", func(c *gin.Context) {
		var ack clustersignal.Acknowledgment
		if err := c.ShouldBindJSON(&ack); err != nil {
			c.Status(400)
			return
		}
		if !n.Signals().Acknowledge(ack) {
			c.Status(409)
			return
		}
		c.Status(200)
	})
}
func TestCoordinatorOnlyCheckedTransferWaitsForPublication(t *testing.T) {
	testCheckedTransfer(t, "publication")
}
func TestCoordinatorOnlyCheckedTransferPublicationFailureAfterReceipt(t *testing.T) {
	testCheckedTransfer(t, "publication-failure")
}
func TestCoordinatorOnlySyncConflictKeepsOriginalUploader(t *testing.T) {
	testCheckedTransfer(t, "dedup")
}
func TestCoordinatorOnlyCheckedTransferRejectsCorruption(t *testing.T) {
	testCheckedTransfer(t, "corrupt")
}
func TestCoordinatorOnlyCheckedTransferCancellationClosesSource(t *testing.T) {
	testCheckedTransfer(t, "cancel")
}

func testCheckedTransfer(t *testing.T, mode string) {
	publication := mode == "publication" || mode == "publication-failure"
	sourceEntered, sourceExited := make(chan struct{}), make(chan struct{})
	releaseSource := make(chan struct{})
	var uploads atomic.Int32
	sourceRoot, destinationRoot := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(sourceRoot, "org/model"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sourceRoot, "org/model/fresh.gguf"), []byte("GGUF"), 0o600))
	modelregistry.SetModelsRootDirOverride(destinationRoot)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	coordinator := transportNode(t, clusternode.Coordinator, "coordinator", nil)
	source := transportNode(t, clusternode.Unclaimed, "source", func(n *clusternode.Node) http.Handler {
		engine := gin.New()
		executor := NewSyncExecutor(func() *http.Client { return nil }, 0, func() context.Context { return t.Context() }, nil, nil)
		executor.security = modelsync.NewSecurityFromRoot(sourceRoot)
		engine.POST("/zzrouter/v1/internal/sync/manifest", executor.HandleGetManifest)
		engine.GET("/zzrouter/v1/internal/sync/file", func(c *gin.Context) {
			if mode == "cancel" {
				close(sourceEntered)
				defer close(sourceExited)
				<-c.Request.Context().Done()
				return
			}
			if mode == "dedup" {
				close(sourceEntered)
				select {
				case <-releaseSource:
				case <-c.Request.Context().Done():
					return
				}
			}
			executor.HandleDownloadFile(c)
		})
		if mode == "corrupt" {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/zzrouter/v1/internal/sync/file" {
					engine.ServeHTTP(corruptTransferWriter{w}, r)
				} else {
					engine.ServeHTTP(w, r)
				}
			})
		}
		return engine
	})
	registry, err := jobs.NewRegistry(jobs.Config{NodeName: "destination", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	destination := transportNode(t, clusternode.Unclaimed, "destination", func(n *clusternode.Node) http.Handler {
		engine := gin.New()
		registerTestExchange(engine, n)
		executor := NewSyncExecutor(func() *http.Client { return nil }, 0, func() context.Context { return t.Context() }, func() error { return n.Signals().NotifyCacheRefresh(t.Context()) }, nil).WithJobs(registry)
		executor.acceptUploads = func() bool { return true }
		engine.POST("/zzrouter/v1/internal/sync/deploy", executor.HandleSyncPull)
		engine.PUT("/zzrouter/v1/internal/sync/deploy/:id", func(c *gin.Context) { uploads.Add(1); executor.HandleSyncUpload(c) })
		engine.POST("/zzrouter/v1/internal/jobs/:id/cancel", func(c *gin.Context) {
			if err := registry.Cancel(c.Param("id")); err != nil {
				c.Status(404)
				return
			}
			c.Status(200)
		})
		return engine
	})
	sourceURL := enrollTransportNode(t, coordinator, source)
	destinationURL := enrollTransportNode(t, coordinator, destination)
	client, err := coordinator.DialClient(clusterid.RoleWorker.OU())
	require.NoError(t, err)
	published, release := make(chan struct{}), make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	observer := clustersignal.NewCoordinator(clustersignal.CoordinatorConfig{Client: func() *http.Client { return client }, Member: func(url, node string) bool { return url == destinationURL && node == "destination" }, Handle: func(ctx context.Context, cert *x509.Certificate, op clustersignal.Operation) clustersignal.Result {
		if err := coordinator.ValidateWorker(cert); err != nil {
			return clustersignal.Result{Error: &clustersignal.StatusError{Status: 403, Detail: err.Error()}}
		}
		if op.Kind != clustersignal.Catalog {
			return clustersignal.Result{Error: &clustersignal.StatusError{Status: 400, Detail: "unexpected operation"}}
		}
		if !publication {
			return clustersignal.Result{}
		}
		close(published)
		select {
		case <-release:
			if mode == "publication-failure" {
				return clustersignal.Result{Error: &clustersignal.StatusError{Status: 503, Detail: "publication refused"}}
			}
			return clustersignal.Result{}
		case <-ctx.Done():
			return clustersignal.Result{Error: &clustersignal.StatusError{Status: 503, Detail: ctx.Err().Error()}}
		}
	}})
	observer.Start(t.Context())
	t.Cleanup(func() { observer.Stop(context.Background()) })
	observer.Observe(destinationURL, "destination")
	payload := SyncPullRequest{Model: "org/model", Format: "gguf", SourceNode: "source", SourceURL: sourceURL}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, destinationURL+"/zzrouter/v1/internal/sync/deploy", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, 202, response.StatusCode)
	var accepted syncDeployAsyncResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&accepted))
	_ = response.Body.Close()
	require.NotEmpty(t, accepted.LocalJobID)
	service := NewDeploymentsService(nil, &NodeIdentity{name: "coordinator"}, nil)
	service.peerClient = func() *http.Client { return client }
	service.resolveNodeURL = func(name string) string {
		switch name {
		case "source":
			return sourceURL
		case "destination":
			return destinationURL
		}
		return ""
	}
	service.peerURL = service.resolveNodeURL
	service.router = &transferFixtureRouter{client: client, target: destinationURL}
	service.jobs = &jobAccess{registry: registry}
	deployment := service.tracker.CreateDeployment("org/model", "gguf", "hf", "", []string{"destination"})
	service.acceptNode(t.Context(), deployment.ID, DeploymentNode{Node: "destination", Source: "source", JobID: accepted.LocalJobID, Status: constants.Status(syncStatusLabel)})
	t.Cleanup(func() { service.Stop(context.Background()) })
	uploaded := make(chan error, 1)
	go func() {
		uploaded <- service.uploadModel(t.Context(), DeploymentNode{Node: "destination", Source: "source", JobID: accepted.LocalJobID}, metadata.DownloadRequest{Repo: "org/model"}, "gguf")
	}()
	if mode == "dedup" {
		<-sourceEntered
		require.True(t, service.dispatchSyncDeploy(t.Context(), deployment.ID, "destination", "source", &DeployRequest{Model: "org/model", Format: "gguf"}, deployPlan{}))
		close(releaseSource)
		require.NoError(t, <-uploaded)
		require.Eventually(t, func() bool {
			return service.tracker.GetDeployment(deployment.ID).Nodes[0].Status == constants.StatusCompleted
		}, 5*time.Second, time.Millisecond)
		service.Stop(context.Background())
		require.Equal(t, int32(1), uploads.Load(), "conflict observation must not start or cancel another upload")
		return
	}
	if !publication {
		if mode == "cancel" {
			<-sourceEntered
			require.NoError(t, registry.Cancel(accepted.LocalJobID))
		}
		select {
		case err := <-uploaded:
			require.Error(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("failed destination did not unwind the coordinator upload")
		}
		event, err := registry.Reuse(accepted.LocalJobID)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			event, err = registry.Reuse(accepted.LocalJobID)
			return err == nil && event.Phase == jobs.PhaseFailed
		}, 5*time.Second, time.Millisecond)
		_, err = os.Stat(filepath.Join(destinationRoot, "org/model/fresh.gguf"))
		require.ErrorIs(t, err, os.ErrNotExist)
		if mode == "cancel" {
			select {
			case <-sourceExited:
			case <-time.After(5 * time.Second):
				t.Fatal("source request outlived upload cancellation")
			}
		}
		return
	}
	<-published
	event, err := registry.Reuse(accepted.LocalJobID)
	require.NoError(t, err)
	require.NotEqual(t, jobs.PhaseDone, event.Phase)
	manifest, err := integrity.ReadManifest(filepath.Join(destinationRoot, "org/model"))
	require.NoError(t, err)
	require.NotNil(t, manifest)
	data, err := os.ReadFile(filepath.Join(destinationRoot, "org/model/fresh.gguf"))
	require.NoError(t, err)
	require.Equal(t, []byte("GGUF"), data)
	// Receipt completes independently; the detached sync remains pending publication.
	require.NoError(t, <-uploaded)
	require.False(t, service.tracker.GetDeployment(deployment.ID).Nodes[0].Status.IsTerminal())
	close(release)
	released = true
	expectedPhase := map[string]jobs.Phase{"publication": jobs.PhaseDone, "publication-failure": jobs.PhaseFailed}[mode]
	require.Eventually(t, func() bool {
		event, err = registry.Reuse(accepted.LocalJobID)
		return err == nil && event.Phase == expectedPhase
	}, 5*time.Second, time.Millisecond)
	expected := map[jobs.Phase]constants.Status{jobs.PhaseDone: constants.StatusCompleted, jobs.PhaseFailed: constants.StatusFailed}[expectedPhase]
	require.Eventually(t, func() bool { return service.tracker.GetDeployment(deployment.ID).Nodes[0].Status == expected }, 5*time.Second, time.Millisecond)
}

type corruptTransferWriter struct{ http.ResponseWriter }

func (w corruptTransferWriter) Write(p []byte) (int, error) {
	copy := bytes.Clone(p)
	if len(copy) > 0 {
		copy[0] ^= 1
	}
	return w.ResponseWriter.Write(copy)
}

func TestCoordinatorUploadBoundsStalls(t *testing.T) {
	for _, mode := range []string{"write", "headers", "ack"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &http.Client{Transport: mcpRoundTripper(func(req *http.Request) (*http.Response, error) {
					if req.URL.Host == "source" {
						var text string
						if strings.Contains(req.URL.Path, "manifest") {
							data, err := json.Marshal(modelsync.SyncManifest{Model: "org/model", Format: "gguf", Files: []modelsync.FileChecksum{{Name: "test.gguf", Size: 4, SHA256: strings.Repeat("a", 64)}}})
							require.NoError(t, err)
							text = string(data)
						} else {
							text = "GGUF"
						}
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(text))}, nil
					}
					if mode != "write" {
						_, err := io.Copy(io.Discard, req.Body)
						require.NoError(t, err)
						require.NoError(t, req.Body.Close())
					}
					if mode == "ack" {
						body := &blockedAck{done: req.Context().Done()}
						return &http.Response{StatusCode: 200, Body: body}, nil
					}
					<-req.Context().Done()
					return nil, req.Context().Err()
				})}
				service := NewDeploymentsService(nil, &NodeIdentity{name: "coordinator"}, nil)
				service.peerClient = func() *http.Client { return client }
				service.peerURL = func(name string) string { return "https://" + name }
				service.resolveNodeURL = service.peerURL
				require.Error(t, service.uploadModel(t.Context(), DeploymentNode{Node: "destination", Source: "source", JobID: "job"}, metadata.DownloadRequest{Repo: "org/model"}, "gguf"))
			})
		})
	}
}

type blockedAck struct{ done <-chan struct{} }

func (b *blockedAck) Read([]byte) (int, error) { <-b.done; return 0, io.ErrClosedPipe }
func (b *blockedAck) Close() error             { return nil }

func TestCoordinatorOnlyProductionProviderAndCatalogExchange(t *testing.T) {
	root := t.TempDir()
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, NodeName: "worker", ClusterMode: config.ClusterModeWorker}, providers: []string{"llamacpp", "vllm"}, enabled: []string{"llamacpp"}})
	coordinator := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, NodeName: "coordinator", ClusterMode: config.ClusterModeCoordinator}, providers: []string{"llamacpp", "vllm"}, enabled: []string{"llamacpp"}})
	require.NoError(t, worker.appsConfig().SaveToDir(worker.configStore.DirPath()))
	require.NoError(t, coordinator.appsConfig().SaveToDir(coordinator.configStore.DirPath()))
	owner := transportNode(t, clusternode.Coordinator, "coordinator", nil)
	workerNode := transportNode(t, clusternode.Unclaimed, "worker", func(n *clusternode.Node) http.Handler {
		worker.cluster.listener = n
		worker.cluster.signaler = n.Signals()
		return testWorkerInternalHandler(t, worker)
	})
	workerURL := enrollTransportNode(t, owner, workerNode)
	client, err := owner.DialClient(clusterid.RoleWorker.OU())
	require.NoError(t, err)
	coordinator.cluster.listener = owner
	coordinator.clusterMTLSClient = client
	if coordinator.cluster.coordinator != nil {
		coordinator.cluster.coordinator.Stop()
	}
	cluster, err := mesh.NewCluster(&mesh.Config{NodeName: "coordinator"}, "http://coordinator.invalid", coordinator, nil, mesh.NewConnector(mesh.ConnectorConfig{HTTPClient: client, ClusterPort: workerNode.Addr().(*net.TCPAddr).Port}))
	require.NoError(t, err)
	t.Cleanup(cluster.Stop)
	coordinator.cluster.coordinator = cluster
	require.NoError(t, cluster.RegisterEndpoint(&mesh.Endpoint{URL: workerURL, ClusterURL: workerURL, NodeName: "worker", Status: mesh.StatusUp}))
	coordinator.model.Cache = cache.New(cache.Config{Node: coordinator.node, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: worker.appsConfig, ClusterClient: coordinator.getClusterClient, ResolveEndpointToNodename: func(string) string { return "worker" }})
	require.NoError(t, coordinator.model.Cache.RefreshCacheSync(t.Context()))
	poller := clustersignal.NewCoordinator(clustersignal.CoordinatorConfig{Client: func() *http.Client { return client }, Member: coordinator.signalMember, Handle: coordinator.handleNodeOperation})
	poller.Start(t.Context())
	t.Cleanup(func() { poller.Stop(context.Background()) })
	poller.Observe(workerURL, "worker")
	require.NoError(t, worker.FinalizeOnboarding("vllm"))
	require.True(t, runsProvider(t, worker, "vllm"))
	require.False(t, runsProvider(t, coordinator, "vllm"))
	require.Equal(t, providerFile(t, coordinator, "on-demand", "vllm"), providerFile(t, worker, "on-demand", "vllm"))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "org/model"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "org/model/fresh.gguf"), []byte("GGUF"), 0o600))
	require.NoError(t, worker.publishCatalog())
	published, err := coordinator.model.Cache.LookupTarget(t.Context(), "fresh", "llamacpp", "worker")
	require.NoError(t, err)
	require.NotNil(t, published, "publication acknowledgment must expose the worker's new model")
}

func TestLocalTransferTargetResolvesAliases(t *testing.T) {
	coordinator := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, NodeName: "coordinator", ClusterMode: config.ClusterModeCoordinator})
	require.NoError(t, coordinator.cluster.coordinator.RegisterEndpoint(&mesh.Endpoint{URL: "http://192.0.2.10:9090", NodeName: "worker", Name: "worker", Status: mesh.StatusUp}))
	service := NewDeploymentsService(nil, coordinator.node, nil)
	defer service.Stop(context.Background())
	service.resolveNodeURL = coordinator.resolveNodePublicURL
	for _, name := range []string{"coordinator", "localhost", "@master", "127.0.0.1", "127.0.0.1:9999", "::1", "[::1]:9999"} {
		require.True(t, service.localTransferTarget(name), name)
	}
	require.False(t, service.localTransferTarget("worker"))
	require.False(t, service.localTransferTarget("unknown"))
}

type transferFixtureRouter struct {
	routing.Router
	client *http.Client
	target string
}

func (r *transferFixtureRouter) Route(ctx context.Context, input *routing.Request) (*routing.Response, error) {
	req, err := http.NewRequestWithContext(ctx, input.Method, r.target+input.Path, bytes.NewReader(input.Body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	return &routing.Response{StatusCode: resp.StatusCode, Body: body}, err
}
