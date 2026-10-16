package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/require"
)

type providerCancelBody struct {
	ctx                      context.Context
	reading, closing, unwind chan struct{}
}

func (b *providerCancelBody) Read(p []byte) (int, error) {
	close(b.reading)
	<-b.ctx.Done()
	// Cross the HF progress throttle so buffered bytes report cancellation.
	<-time.After(200 * time.Millisecond)
	return copy(p, "part"), context.Canceled
}

func (b *providerCancelBody) Close() error {
	close(b.closing)
	<-b.unwind
	return nil
}

type providerModelCluster struct {
	mesh.ClusterClient
	worker  http.Handler
	calls   atomic.Int32
	offline bool
}

func (c *providerModelCluster) Unicast(ctx context.Context, node, path string, params *mesh.QueryParams) (*mesh.Response, error) {
	c.calls.Add(1)
	if c.offline || node != "worker" {
		return nil, errors.New("fixture worker unavailable")
	}
	r := httptest.NewRequest(params.Method, path, bytes.NewReader(params.Body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c.worker.ServeHTTP(w, r)
	return &mesh.Response{StatusCode: w.Code, Body: w.Body.Bytes(), Headers: w.Header(), SourceNode: node}, nil
}

type providerModelHarness struct {
	public              *gin.Engine
	workerEngine        *gin.Engine
	worker, coordinator *DeploymentsExecutor
	cluster             *providerModelCluster
}

func newProviderModelHarness(t *testing.T) *providerModelHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	modelregistry.SetModelsRootDirOverride(t.TempDir())
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	newNode := func(node string) (*DeploymentsExecutor, *gin.Engine) {
		cfg := &config.AppsConfig{}
		require.NoError(t, cfg.AddApp("studio", config.ServiceConfig{Name: "studio", Enabled: new(false), Mode: constants.AppModeOnDemand, RunScope: config.RunScopeNode,
			Runtime: &config.AppRuntimeConfig{DataDir: node + "-data", ModelStore: &config.ModelStoreConfig{Directory: "models", Categories: []string{"vae", "loras"}},
				BasePort: 8188, PortRange: []int{8188, 8188}, Execution: config.ExecutionConfig{Type: "python", Command: "python", WorkingDir: "source"}}}))
		require.NoError(t, cfg.AddApp("remote", config.ServiceConfig{Name: "remote", Protocol: config.ProtocolOpenAI, Mode: constants.AppModeExternal, Runtime: &config.AppRuntimeConfig{Endpoint: "http://fixture.invalid"}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
		registry, err := modelregistry.NewRegistry(func() *config.AppsConfig { return cfg })
		require.NoError(t, err)
		t.Cleanup(registry.Stop)
		tracker := modelregistry.NewDownloadTrackerWithPersistence("")
		t.Cleanup(tracker.Stop)
		jobRegistry, err := jobs.NewRegistry(jobs.Config{NodeName: node, Clock: clock.System()})
		require.NoError(t, err)
		t.Cleanup(jobRegistry.Stop)
		e := &DeploymentsExecutor{getNodename: func() string { return node }, appsConfig: func() *config.AppsConfig { return cfg },
			isCloudProviderFn: func(string) bool { return false }, isLocalNodeCompatible: func(string, string, string) bool { t.Error("provider download used LLM admission"); return false },
			publishCatalog: func() error { t.Error("provider operation published the LLM catalog"); return nil },
			registry:       registry, downloads: tracker, jobs: jobRegistry, jobKeys: map[string]string{}}
		r := gin.New()
		group := r.Group("/zzrouter/v1/internal")
		group.POST("/deployments", e.HandleInternalDeploy)
		group.GET("/providers/:name/models", e.HandleInternalListProviderModels)
		group.DELETE("/providers/:name/models", e.HandleInternalDeleteProviderModel)
		return e, r
	}
	h := &providerModelHarness{}
	var coordinatorEngine *gin.Engine
	h.worker, h.workerEngine = newNode("worker")
	h.coordinator, coordinatorEngine = newNode("coordinator")
	h.cluster = &providerModelCluster{worker: h.workerEngine}
	local := func(ctx context.Context, req *routing.Request) (*routing.Response, error) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(req.Method, req.Path, bytes.NewReader(req.Body)).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		coordinatorEngine.ServeHTTP(w, r)
		return &routing.Response{StatusCode: w.Code, Body: w.Body.Bytes(), Headers: w.Header()}, nil
	}
	router := routing.NewClusterAwareRouterWithClient("coordinator", h.cluster, "0", local, http.DefaultClient)
	ctrl := &ProviderModelsController{service: NewProvidersService(router), nodes: &NodeIdentity{name: "coordinator", endpoints: []string{"worker"}}}
	s := &Server{idempotency: newIdempotencyStore()}
	h.public = gin.New()
	h.public.Use(RequestIDMiddleware(), func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "fixture-agent"}})
	})
	ctrl.RegisterPublicRoutes(h.public.Group("/zzrouter/v1"), s.idempotent)
	return h
}

func (h *providerModelHarness) request(method, suffix, body, idem string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/zzrouter/v1/providers/"+suffix, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", idem)
	w := httptest.NewRecorder()
	h.public.ServeHTTP(w, r)
	return w
}

func providerModelRoot(t *testing.T, e *DeploymentsExecutor) string {
	t.Helper()
	cfg, ok := e.appsConfig().LookupApp("studio")
	require.True(t, ok)
	root, err := cfg.Runtime.ModelStorePath(fsroot.ProviderDir("studio"))
	require.NoError(t, err)
	return root
}

// The HTTP fixture uses a loopback listener when permitted. Restricted test
// environments still drive its HTTP handler and Fetch via a RoundTripper.
func installProviderHTTPFixture(t *testing.T, h *providerModelHarness, handler http.Handler) {
	t.Helper()
	workerRegistry, coordinatorRegistry := h.worker.registry, h.coordinator.registry
	var transport downloadTargetTransport
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		s := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
		done := make(chan struct{})
		go func() { defer close(done); _ = s.Serve(listener) }()
		t.Cleanup(func() { _ = s.Close(); <-done })
		transport = downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
			copy := r.Clone(r.Context())
			copy.URL.Scheme, copy.URL.Host = "http", listener.Addr().String()
			return http.DefaultTransport.RoundTrip(copy)
		})
	} else {
		t.Logf("loopback HF fixture unavailable; using in-process HTTP fixture: %v", err)
		transport = downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w.Result(), nil
		})
	}
	workerRegistry.GetHuggingFaceConnector().SetHTTPTransport(transport)
	coordinatorRegistry.GetHuggingFaceConnector().SetHTTPTransport(transport)
	t.Cleanup(func() {
		// Wait for producers even when an assertion ends the test early.
		h.worker.jobs.Stop()
		h.coordinator.jobs.Stop()
		workerRegistry.Stop()
		coordinatorRegistry.Stop()
	})
}

func TestProviderModelsTwoNodeDownloadListDelete(t *testing.T) {
	h := newProviderModelHarness(t)
	const content = "fixture-vae-weights"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	var fetched atomic.Int32
	installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/org/repo/revision/v2":
			require.Equal(t, "true", r.URL.Query().Get("blobs"))
			_ = json.NewEncoder(w).Encode(map[string]any{"siblings": []map[string]any{{"rfilename": "nested/vae.safetensors", "size": len(content), "lfs": map[string]any{"oid": hash}}, {"rfilename": "other.safetensors", "size": 9}}})
		case "/org/repo/resolve/v2/nested/vae.safetensors":
			fetched.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(len(content)))
			_, _ = io.WriteString(w, content)
		default:
			t.Errorf("unexpected HF request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	body := `{"node":"worker","category":"vae","source":"https://huggingface.co/org/repo/resolve/v2/nested/vae.safetensors"}`
	first := h.request("POST", "studio/models", body, "request-1")
	require.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
	var out struct {
		Data providerModelDownload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &out))
	require.Equal(t, "worker", out.Data.Node)
	require.Equal(t, "v2", out.Data.Revision)
	require.Equal(t, "vae.safetensors", out.Data.Name)
	require.NotEmpty(t, out.Data.JobID)
	require.Equal(t, jobs.PhaseDone, waitJob(t, h.worker.jobs, out.Data.JobID).Phase)
	replay := h.request("POST", "studio/models", body, "request-1")
	require.Equal(t, first.Body.String(), replay.Body.String())
	require.EqualValues(t, 1, fetched.Load())
	require.EqualValues(t, 1, h.cluster.calls.Load(), "idempotency replays before worker dispatch")
	require.Equal(t, http.StatusUnprocessableEntity, h.request("POST", "studio/models", strings.Replace(body, "\"vae\"", "\"loras\"", 1), "request-1").Code)
	root := providerModelRoot(t, h.worker)
	file := filepath.Join(root, "vae/vae.safetensors")
	bytes, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, content, string(bytes))
	manifest, err := integrity.ReadManifest(filepath.Dir(file))
	require.NoError(t, err)
	require.Equal(t, hash, manifest.Files[0].SHA256)
	_, err = os.Stat(providerModelRoot(t, h.coordinator))
	require.ErrorIs(t, err, os.ErrNotExist, "worker download must not touch coordinator storage")
	list := h.request("GET", "studio/models?node=worker", "", "")
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var inventory struct {
		Data providerModelList `json:"data"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &inventory))
	require.Equal(t, "worker", inventory.Data.Node)
	require.Len(t, inventory.Data.Categories, 2)
	require.Empty(t, inventory.Data.Categories[0].Files)
	require.Equal(t, "vae.safetensors", inventory.Data.Categories[1].Files[0].Name)
	for _, e := range []*DeploymentsExecutor{h.worker, h.coordinator} {
		models, err := e.registry.ListAllModels()
		require.NoError(t, err)
		require.Empty(t, models)
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		want := map[string]string{"GET": "listProviderModels", "POST": "downloadProviderModel", "DELETE": "deleteProviderModel"}[method]
		catalog := catalogRoute(gin.RouteInfo{Method: method, Path: "/zzrouter/v1/providers/:name/models"})
		require.Equal(t, "admin", catalog.Auth)
		require.Equal(t, method == "POST", catalog.Async)
		id, err := (&Server{engine: h.public}).eligibleAPIRoute(method, "/zzrouter/v1/providers/studio/models")
		require.NoError(t, err)
		require.Equal(t, want, id)
	}
	require.Eventually(t, func() bool {
		h.worker.jobKeysMu.Lock()
		defer h.worker.jobKeysMu.Unlock()
		return len(h.worker.jobKeys) == 0
	}, time.Second, time.Millisecond)
	assertProviderModelProblem(t, h.request("POST", "studio/models", body, ""), 409, httperr.CodeFileExists)
	h.worker.registry = nil // inventory/deletion need only filesystem and reservation ownership.
	deleted := h.request("DELETE", "studio/models?node=worker&category=vae&name=vae.safetensors", "", "")
	require.Equal(t, http.StatusOK, deleted.Code, deleted.Body.String())
	_, err = os.Lstat(file)
	require.ErrorIs(t, err, os.ErrNotExist)
	assertProviderModelProblem(t, h.request("DELETE", "studio/models?node=worker&category=vae&name=vae.safetensors", "", ""), 404, httperr.CodeFileNotFound)
}

func assertProviderModelProblem(t *testing.T, w *httptest.ResponseRecorder, status int, code httperr.ParamErrorCode) {
	t.Helper()
	require.Equal(t, status, w.Code, w.Body.String())
	require.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
	require.Contains(t, codesFromProblem(t, w), code)
}

func TestProviderModelsRefusalsAndContainment(t *testing.T) {
	h := newProviderModelHarness(t)
	for _, node := range []string{"", "*", "/*", " ", "https://*/", "https:///"} {
		assertProviderModelProblem(t, h.request("GET", "studio/models?node="+url.QueryEscape(node), "", ""), 400, httperr.CodeRequired)
	}
	assertProviderModelProblem(t, h.request("GET", "studio/models?node=unknown", "", ""), 404, httperr.CodeUnknownNode)
	require.Zero(t, h.cluster.calls.Load())
	assertProviderModelProblem(t, h.request("GET", "studio/models?node=worker&category=bad", "", ""), 400, httperr.CodeInvalidModelCategory)
	assertProviderModelProblem(t, h.request("GET", "remote/models?node=worker", "", ""), 400, httperr.CodeProviderModelStoreUnsupported)
	require.Equal(t, 404, h.request("GET", "ghost/models?node=worker", "", "").Code)
	for _, name := range []string{"../outside", "/absolute", `dir\file`, "C:/drive", ".zzrouter-manifest.json", "dir/file.zzpart"} {
		body, err := json.Marshal(providerModelDownloadRequest{Node: "worker", Category: "vae", Source: "org/repo#weights", Name: name})
		require.NoError(t, err)
		assertProviderModelProblem(t, h.request("POST", "studio/models", string(body), ""), 400, httperr.CodeInvalidFileName)
		assertProviderModelProblem(t, h.request("DELETE", "studio/models?node=worker&category=vae&name="+url.QueryEscape(name), "", ""), 400, httperr.CodeInvalidFileName)
	}
	assertProviderModelProblem(t, h.request("POST", "studio/models", `{"node":"worker","category":"vae","source":"https://example.com/file"}`, ""), 400, httperr.CodeInvalidValue)
	require.Equal(t, 400, h.request("POST", "studio/models", `{"node":"worker","category":"vae","source":"org/repo#file","force":true}`, "").Code)
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload))
	root := filepath.Join(providerModelRoot(t, h.worker), "vae")
	require.NoError(t, os.MkdirAll(root, 0700))
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "file"), filepath.Join(root, "link")))
	for _, name := range []string{"escape/file", "link"} {
		assertProviderModelProblem(t, h.request("DELETE", "studio/models?node=worker&category=vae&name="+name, "", ""), 400, httperr.CodeInvalidFileName)
	}
	list := h.request("GET", "studio/models?node=worker&category=vae", "", "")
	require.Equal(t, 200, list.Code)
	require.Contains(t, list.Body.String(), `"files":[]`)
	bytes, err := os.ReadFile(filepath.Join(outside, "file"))
	require.NoError(t, err)
	require.Equal(t, "outside", string(bytes))
	h.cluster.offline = true
	require.Equal(t, 502, h.request("GET", "studio/models?node=worker", "", "").Code)
	require.Equal(t, 502, h.request("DELETE", "studio/models?node=worker&category=vae&name=file", "", "").Code)
	require.Equal(t, 502, h.request("POST", "studio/models", `{"node":"worker","category":"vae","source":"org/repo#file"}`, "").Code)
}

func TestProviderModelsCancellationKeepsDestinationReserved(t *testing.T) {
	h := newProviderModelHarness(t)
	entered, cancelled, unwind := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unwind) }) }
	defer release()
	h.worker.registry.GetHuggingFaceConnector().SetHTTPTransport(downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
		<-unwind
		return nil, r.Context().Err()
	}))
	body := `{"node":"worker","category":"vae","source":"org/repo#file"}`
	first := h.request("POST", "studio/models", body, "")
	require.Equal(t, 202, first.Code, first.Body.String())
	var out struct {
		Data providerModelDownload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &out))
	<-entered
	duplicate := h.request("POST", "studio/models", body, "")
	require.Equal(t, 202, duplicate.Code)
	var same struct {
		Data providerModelDownload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(duplicate.Body.Bytes(), &same))
	require.Equal(t, out.Data.JobID, same.Data.JobID)
	assertProviderModelProblem(t, h.request("DELETE", "studio/models?node=worker&category=vae&name=file", "", ""), 409, httperr.CodeDownloadInProgress)
	require.Equal(t, jobs.KindDownload, waitJobKind(t, h.worker.jobs, out.Data.JobID))
	entries := h.worker.downloads.GetStatus()
	require.Len(t, entries, 1)
	key := entries[0].Key
	stop := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(stop)
	c.Request = httptest.NewRequest("DELETE", "/?key="+url.QueryEscape(key), nil)
	h.worker.HandleInternalStopDownload(c)
	<-cancelled
	require.Nil(t, h.worker.downloads.GetDownloadStatus(key))
	// Even a janitor-terminal job still has a producer-owned reservation.
	assertProviderModelProblem(t, h.request("DELETE", "studio/models?node=worker&category=vae&name=file", "", ""), 409, httperr.CodeDownloadInProgress)
	assertProviderModelProblem(t, h.request("POST", "studio/models", body, ""), 409, httperr.CodeDownloadInProgress)
	require.Len(t, h.worker.jobs.List(jobs.KindDownload), 1)
	release()
	require.Equal(t, jobs.PhaseFailed, waitJob(t, h.worker.jobs, out.Data.JobID).Phase)
	require.Eventually(t, func() bool {
		h.worker.jobKeysMu.Lock()
		defer h.worker.jobKeysMu.Unlock()
		return !h.worker.downloadReserved(key)
	}, time.Second, time.Millisecond)
	assertProviderModelProblem(t, h.request("DELETE", "studio/models?node=worker&category=vae&name=file", "", ""), 404, httperr.CodeFileNotFound)
	h.worker.registry.GetHuggingFaceConnector().SetHTTPTransport(downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
		body := "weights"
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			body = `{"siblings":[{"rfilename":"file","size":7}]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": {fmt.Sprint(len(body))}}, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	retry := h.request("POST", "studio/models", body, "")
	require.Equal(t, 202, retry.Code, retry.Body.String())
	var next struct {
		Data providerModelDownload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(retry.Body.Bytes(), &next))
	require.NotEqual(t, out.Data.JobID, next.Data.JobID)
	require.Equal(t, jobs.PhaseDone, waitJob(t, h.worker.jobs, next.Data.JobID).Phase)
	bytes, err := os.ReadFile(filepath.Join(providerModelRoot(t, h.worker), "vae/file"))
	require.NoError(t, err)
	require.Equal(t, "weights", string(bytes))
	require.Empty(t, h.coordinator.jobs.List(jobs.KindDownload))
}

func TestProviderModelsCancellationDuringBufferedBodyClose(t *testing.T) {
	h := newProviderModelHarness(t)
	reading, closing, unwind := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unwind) }) }
	defer release()
	h.worker.registry.GetHuggingFaceConnector().SetHTTPTransport(downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			body := `{"siblings":[{"rfilename":"file","size":7}]}`
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": {fmt.Sprint(len(body))}}, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": {"7"}}, ContentLength: 7,
			Body: &providerCancelBody{ctx: r.Context(), reading: reading, closing: closing, unwind: unwind}, Request: r}, nil
	}))
	body := `{"node":"worker","category":"vae","source":"org/repo#file"}`
	first := h.request("POST", "studio/models", body, "")
	require.Equal(t, 202, first.Code, first.Body.String())
	var out struct {
		Data providerModelDownload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &out))
	<-reading
	key := h.worker.downloads.GetStatus()[0].Key
	w := httptest.NewRecorder()
	downloadCancelAPI(t, h.worker, true).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/zzrouter/v1/jobs/"+out.Data.JobID, nil))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	<-closing
	event, err := h.worker.jobs.Get(out.Data.JobID)
	require.NoError(t, err)
	require.False(t, event.Phase.IsTerminal(), "terminal status must follow buffered body cleanup")
	assertProviderModelProblem(t, h.request("DELETE", "studio/models?node=worker&category=vae&name=file", "", ""), 409, httperr.CodeDownloadInProgress)
	retry := h.request("POST", "studio/models", body, "")
	require.Equal(t, http.StatusAccepted, retry.Code, retry.Body.String())
	var repeated struct {
		Data providerModelDownload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(retry.Body.Bytes(), &repeated))
	require.Equal(t, out.Data.JobID, repeated.Data.JobID, "the unsettled transfer remains the deduplication owner")
	release()
	event = waitJob(t, h.worker.jobs, out.Data.JobID)
	require.Equal(t, jobs.PhaseFailed, event.Phase)
	require.Equal(t, "cancelled", event.Meta["transfer_status"])
	require.Eventually(t, func() bool {
		h.worker.jobKeysMu.Lock()
		defer h.worker.jobKeysMu.Unlock()
		return !h.worker.downloadReserved(key)
	}, time.Second, time.Millisecond)
	root := filepath.Join(providerModelRoot(t, h.worker), "vae")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "cancelled transfer removes final and partial entries before releasing its reservation")
}

func waitJobKind(t *testing.T, r *jobs.Registry, id string) jobs.Kind {
	t.Helper()
	event, err := r.Get(id)
	require.NoError(t, err)
	return event.Kind
}

func providerModelParamCodeCase(code httperr.ParamErrorCode) paramCodeCase {
	return paramCodeCase{how: "a provider model request rejected by storage or admission", provoke: func(t *testing.T) []httperr.ParamErrorCode {
		h := newProviderModelHarness(t)
		var w *httptest.ResponseRecorder
		switch code {
		case httperr.CodeInvalidModelCategory:
			w = h.request("GET", "studio/models?node=worker&category=ghost", "", "")
		case httperr.CodeInvalidFileName:
			w = h.request("DELETE", "studio/models?node=worker&category=vae&name=../escape", "", "")
		case httperr.CodeProviderModelStoreUnsupported:
			w = h.request("GET", "remote/models?node=worker", "", "")
		case httperr.CodeFileNotFound:
			w = h.request("DELETE", "studio/models?node=worker&category=vae&name=file", "", "")
		case httperr.CodeFileExists:
			root := filepath.Join(providerModelRoot(t, h.worker), "vae")
			require.NoError(t, os.MkdirAll(root, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "file"), []byte("manual"), 0600))
			w = h.request("POST", "studio/models", `{"node":"worker","category":"vae","source":"org/repo#file"}`, "")
		case httperr.CodeDownloadInProgress:
			req := internalDownloadRequest{Model: "org/repo", Registry: constants.RepoHuggingFace, Download: &metadata.DownloadRequest{Repo: "org/repo", Weights: "file", Target: metadata.DownloadTarget{Owner: metadata.OwnerProviderStore, Provider: "studio", Category: "vae", Name: "file"}}}
			a, err := h.worker.resolveDownloadAdmission(req, req.Model, req.Registry)
			require.NoError(t, err)
			c, _ := testGinContext("POST", "/")
			attempt := h.worker.beginDownload(c, a)
			require.NotNil(t, attempt)
			defer h.worker.updateDownloadJob(attempt, a.key, "cancelled", "", 0, 0, 0, 0, 0, 0)
			w = h.request("DELETE", "studio/models?node=worker&category=vae&name=file", "", "")
		default:
			t.Fatalf("missing provider fixture for %s", code)
		}
		return codesFromProblem(t, w)
	}}
}

func TestProviderModelsDiskPreflightAndUnknownSize(t *testing.T) {
	const tooLargeSize = int64(1 << 60)
	for _, size := range []int64{tooLargeSize, 0, 7} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			h := newProviderModelHarness(t)
			var fetched atomic.Int32
			installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/models/") {
					_ = json.NewEncoder(w).Encode(map[string]any{"siblings": []map[string]any{{"rfilename": "file", "size": size}}})
					return
				}
				fetched.Add(1)
				w.Header().Set("Content-Length", "7")
				_, _ = io.WriteString(w, "weights")
			}))
			w := h.request("POST", "studio/models", `{"node":"worker","category":"vae","source":"org/repo#file"}`, "")
			require.Equal(t, 202, w.Code, w.Body.String())
			var out struct {
				Data providerModelDownload `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
			event := waitJob(t, h.worker.jobs, out.Data.JobID)
			file := filepath.Join(providerModelRoot(t, h.worker), "vae/file")
			if size == tooLargeSize {
				require.Equal(t, jobs.PhaseFailed, event.Phase)
				require.Contains(t, event.Err, "insufficient disk space")
				require.Zero(t, fetched.Load())
				_, err := os.Lstat(file)
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.Equal(t, jobs.PhaseDone, event.Phase, event.Err)
				bytes, err := os.ReadFile(file)
				require.NoError(t, err)
				require.Equal(t, "weights", string(bytes))
				require.EqualValues(t, 1, fetched.Load())
			}
		})
	}
}

func TestProviderModelsActualSurfaceAuthAndWorkerIsolation(t *testing.T) {
	for _, mode := range []config.ClusterMode{config.ClusterModeDisabled, config.ClusterModeWorker} {
		t.Run(string(mode), func(t *testing.T) {
			s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, ClusterMode: mode})
			for _, operation := range []struct{ method, suffix string }{{"GET", ""}, {"POST", ""}, {"DELETE", ""}, {"POST", "/resolve"}} {
				for _, key := range []string{"", TestUserKey, TestAdminKey} {
					body := `{"node":"unknown","category":"vae","source":"org/repo#file"}`
					if operation.suffix == "/resolve" {
						body = `{"node":"unknown","workflow":{"nodes":[]}}`
					}
					r := httptest.NewRequest(operation.method, "/zzrouter/v1/providers/ghost/models"+operation.suffix+"?node=unknown", strings.NewReader(body))
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("X-API-Key", key)
					w := httptest.NewRecorder()
					s.ServeHTTP(w, r)
					want := 404
					if mode == config.ClusterModeWorker {
						want = http.StatusMisdirectedRequest
					}
					if mode != config.ClusterModeWorker && key != TestAdminKey {
						want = 401
						if key == TestUserKey {
							want = 403
						}
					}
					require.Equal(t, want, w.Code, w.Body.String())
				}
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/zzrouter/v1/internal/providers/ghost/models", nil)
			r.Header.Set("X-API-Key", TestAdminKey)
			s.ServeHTTP(w, r)
			require.Equal(t, 404, w.Code, "admin credentials must not confer internal trust; the surface is hidden")
		})
	}
}

func TestProviderModelsReservationSpansTrustedMounts(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker, SeedProvidersDir: true})
	fsroot.SetProviderRootOverride(root)
	mtls := buildInternalEngine(s)
	e := s.newDeploymentsExecutor()
	req := internalDownloadRequest{Model: "org/repo", Registry: constants.RepoHuggingFace, Download: &metadata.DownloadRequest{Repo: "org/repo", Weights: "file", Target: metadata.DownloadTarget{Owner: metadata.OwnerProviderStore, Provider: "comfyui", Category: "vae", Name: "file"}}}
	a, err := e.resolveDownloadAdmission(req, req.Model, req.Registry)
	require.NoError(t, err)
	c, _ := testGinContext("POST", "/")
	attempt := e.beginDownload(c, a)
	require.NotNil(t, attempt)
	defer func() {
		attempt.cancel()
		attempt.job.Fail(errors.New("fixture complete"))
		e.forgetJobKey(a.key, attempt.job.ID())
	}()
	require.NoError(t, os.MkdirAll(filepath.Dir(a.path), 0700))
	require.NoError(t, os.WriteFile(a.path, []byte("landed"), 0600))
	require.True(t, e.downloads.StopDownload(a.key))
	e.downloads.RemoveDownload(a.key)
	path := "/zzrouter/v1/internal/providers/comfyui/models?category=vae&name=file"
	local, err := s.ServeClusterRequest(t.Context(), &mesh.Request{Method: "DELETE", Path: path})
	require.NoError(t, err)
	require.Equal(t, 409, local.StatusCode, string(local.Body))
	w := httptest.NewRecorder()
	mtls.ServeHTTP(w, httptest.NewRequest("DELETE", path, nil))
	assertProviderModelProblem(t, w, 409, httperr.CodeDownloadInProgress)
	bytes, err := os.ReadFile(a.path)
	require.NoError(t, err)
	require.Equal(t, "landed", string(bytes))
	e.forgetJobKey(a.key, attempt.job.ID()) // Fixture producer has now unwound.
	w = httptest.NewRecorder()
	mtls.ServeHTTP(w, httptest.NewRequest("DELETE", path, nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	_, err = os.Lstat(a.path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
