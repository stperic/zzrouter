package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/require"
)

type downloadTargetTransport func(*http.Request) (*http.Response, error)

func (f downloadTargetTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// downloadWorkerRouter models coordinator dispatch to a separate worker
// executor through the existing internal wire, without opening sockets.
type downloadWorkerRouter struct {
	fakeRouter
	executor *DeploymentsExecutor
}

func (r *downloadWorkerRouter) Route(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	if req.Node != "worker" || req.Path != "/zzrouter/v1/internal/deployments" {
		return nil, errors.New("wrong download dispatch")
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(req.Method, req.Path, bytes.NewReader(req.Body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	r.executor.HandleInternalDeploy(c) //nolint:contextcheck // Request context is on c.Request; download jobs intentionally detach.
	return &routing.Response{StatusCode: w.Code, Body: w.Body.Bytes()}, nil
}

func downloadTargetExecutor(t *testing.T) *DeploymentsExecutor {
	t.Helper()
	modelregistry.SetModelsRootDirOverride(t.TempDir())
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("studio", config.ServiceConfig{Name: "studio", Mode: constants.AppModeOnDemand, RunScope: config.RunScopeNode,
		Runtime: &config.AppRuntimeConfig{DataDir: "data", ModelStore: &config.ModelStoreConfig{Directory: "models", Categories: []string{"vae"}},
			BasePort: 8188, PortRange: []int{8188, 8188}, Execution: config.ExecutionConfig{Type: "python", Command: "python", WorkingDir: "source"}}}))
	registry, err := modelregistry.NewRegistry(func() *config.AppsConfig { return cfg })
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	tracker := modelregistry.NewDownloadTrackerWithPersistence("")
	t.Cleanup(tracker.Stop)
	jobRegistry, err := jobs.NewRegistry(jobs.Config{NodeName: "worker", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(jobRegistry.Stop)
	return &DeploymentsExecutor{getNodename: func() string { return "worker" }, appsConfig: func() *config.AppsConfig { return cfg },
		isCloudProviderFn: func(string) bool { return false }, isLocalNodeCompatible: func(string, string, string) bool { return false },
		buildIncompatibleError: func(string, string, string) string { return "LLM incompatible" },
		registry:               registry, downloads: tracker, jobs: jobRegistry, jobKeys: map[string]string{}}
}

func testHuggingFaceAdmission(t *testing.T, e *DeploymentsExecutor, request metadata.DownloadRequest, key, provider string, features []string) downloadAdmission {
	t.Helper()
	req := internalDownloadRequest{Model: request.Repo, Registry: constants.RepoHuggingFace, Download: &request,
		Provider: provider, Features: features, Force: request.Force}
	require.NoError(t, req.validateTarget())
	a, err := e.resolveDownloadAdmission(req, req.Model, req.Registry)
	require.NoError(t, err)
	a.key = key
	return a
}

func TestProviderDownloadUsesWorkerPipelineAndDestinationAdmission(t *testing.T) {
	var fetched atomic.Int32
	e := downloadTargetExecutor(t)
	e.registry.GetHuggingFaceConnector().SetHTTPTransport(downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/org/model/resolve/main/weights.gguf" {
			return nil, errors.New("unexpected source URL")
		}
		fetched.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": {strconv.Itoa(len("weights"))}},
			ContentLength: int64(len("weights")), Body: io.NopCloser(strings.NewReader("weights")), Request: r}, nil
	}))
	var publications atomic.Int32
	e.publishCatalog = func() error { publications.Add(1); return errors.New("catalog must not run") }
	e.ensureFeatures = func(context.Context, string, string, []string, []string, func(string)) error {
		return errors.New("features must not run")
	}
	svc, ok := e.appsConfig().LookupApp("studio")
	require.True(t, ok)
	root, err := svc.Runtime.ModelStorePath(fsroot.ProviderDir("studio"))
	require.NoError(t, err)
	root = filepath.Join(root, "vae")
	release, err := integrity.AcquireMaterialization(t.Context(), root)
	require.NoError(t, err)
	var once sync.Once
	unlock := func() { once.Do(release) }
	defer unlock()
	req := internalDownloadRequest{Model: "org/model", Registry: constants.RepoHuggingFace, Download: &metadata.DownloadRequest{Repo: "org/model", Weights: "weights.gguf",
		Target: metadata.DownloadTarget{Owner: metadata.OwnerProviderStore, Provider: "studio", Category: "vae", Name: "nested/chosen.gguf"},
		Files:  []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: "weights.gguf", Size: 7}}}}}
	router := &downloadWorkerRouter{executor: e}
	first, err := dispatchDownload(t.Context(), router, "worker", req)
	require.NoError(t, err)
	require.Equal(t, req.Download.Target, first.Target)
	req.Download.Revision = "main"
	duplicate, err := dispatchDownload(t.Context(), router, "worker", req)
	require.NoError(t, err)
	require.Equal(t, first.JobID, duplicate.JobID)
	req.Download.Revision = "other"
	_, err = dispatchDownload(t.Context(), router, "worker", req)
	var refusal *RoutedError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusConflict, refusal.StatusCode)
	require.Len(t, e.jobs.List(jobs.KindDownload), 1)
	unlock()
	event := waitJob(t, e.jobs, first.JobID)
	require.Equal(t, jobs.KindDownload, event.Kind)
	require.Equal(t, jobs.PhaseDone, event.Phase, event.Err)
	require.Zero(t, publications.Load())
	require.EqualValues(t, 1, fetched.Load())
	bytes, err := os.ReadFile(filepath.Join(root, "nested/chosen.gguf"))
	require.NoError(t, err)
	require.Equal(t, "weights", string(bytes))
	require.Equal(t, req.Download.Target, e.downloads.GetDownloadStatus(first.Key).Target)
	require.NotContains(t, event.Meta, "model")
	require.Contains(t, event.Meta, "target")
	models, err := e.registry.ListAllModels()
	require.NoError(t, err)
	require.Empty(t, models, "provider files must stay outside the LLM catalog")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	e.HandleInternalListDeployments(c)
	var listing listDeploymentsEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listing))
	require.Empty(t, listing.Data)
	req.Download.Revision = "main"
	_, err = dispatchDownload(t.Context(), router, "worker", req)
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusConflict, refusal.StatusCode)
	require.Len(t, e.jobs.List(jobs.KindDownload), 1)
}

func TestDownloadOwnerRefusalPrecedesCompatibilityAndAdmission(t *testing.T) {
	e := downloadTargetExecutor(t)
	var compatibility atomic.Int32
	e.isLocalNodeCompatible = func(string, string, string) bool { compatibility.Add(1); return false }
	for _, target := range []metadata.DownloadTarget{{Owner: "unknown"}, {Provider: "studio"}, {Owner: metadata.OwnerProviderStore}} {
		req := internalDownloadRequest{Model: "org/model", Download: &metadata.DownloadRequest{Repo: "org/model", Target: target}}
		_, err := dispatchDownload(t.Context(), &downloadWorkerRouter{executor: e}, "worker", req)
		var refusal *RoutedError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, http.StatusBadRequest, refusal.StatusCode)
		require.Zero(t, compatibility.Load())
		require.Empty(t, e.jobs.List(jobs.KindDownload))
	}
}

func TestProviderDownloadRefusesUnavailableRegistryBeforeAdmission(t *testing.T) {
	e := downloadTargetExecutor(t)
	e.registry = nil
	req := internalDownloadRequest{Model: "org/model", Registry: constants.RepoHuggingFace,
		Download: &metadata.DownloadRequest{Repo: "org/model", Weights: "weights.gguf",
			Target: metadata.DownloadTarget{Owner: metadata.OwnerProviderStore, Provider: "studio", Category: "vae", Name: "chosen.gguf"}}}
	_, err := dispatchDownload(t.Context(), &downloadWorkerRouter{executor: e}, "worker", req)
	var refusal *RoutedError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusServiceUnavailable, refusal.StatusCode)
	require.Empty(t, e.jobs.List(jobs.KindDownload))
	require.Empty(t, e.downloads.GetStatus())
}

func TestCatalogDownloadCompletionFailureStillFailsJob(t *testing.T) {
	e := downloadTargetExecutor(t)
	e.publishCatalog = func() error { return errors.New("publication failed") }
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	e.downloadHuggingFace(c, testHuggingFaceAdmission(t, e, metadata.DownloadRequest{Repo: "org/model", Files: []metadata.DownloadFile{}}, "catalog", "", nil))
	var result internalDeploymentResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	event := waitJob(t, e.jobs, result.JobID)
	require.Equal(t, jobs.PhaseFailed, event.Phase)
	require.Contains(t, event.Err, "publication failed")
}

func TestProviderDownloadReservesExactResolvedNames(t *testing.T) {
	for _, names := range [][2]string{{"chosen.gguf", "CHOSEN.gguf"}, {"chosen-é.gguf", "chosen-e\u0301.gguf"}} {
		t.Run(names[0], func(t *testing.T) {
			e := downloadTargetExecutor(t)
			svc, _ := e.appsConfig().LookupApp("studio")
			root, err := svc.Runtime.ModelStorePath(fsroot.ProviderDir("studio"))
			require.NoError(t, err)
			release, err := integrity.AcquireMaterialization(t.Context(), filepath.Join(root, "vae"))
			require.NoError(t, err)
			defer release()
			defer e.downloads.StopAll()
			req := internalDownloadRequest{Model: "org/model", Registry: constants.RepoHuggingFace,
				Download: &metadata.DownloadRequest{Repo: "org/model", Weights: "weights.gguf",
					Target: metadata.DownloadTarget{Owner: metadata.OwnerProviderStore, Provider: "studio", Category: "vae", Name: names[0]},
					Files:  []metadata.DownloadFile{{TreeFileEntry: metadata.TreeFileEntry{Name: "weights.gguf"}}}}}
			router := &downloadWorkerRouter{executor: e}
			first, err := dispatchDownload(t.Context(), router, "worker", req)
			require.NoError(t, err)
			req.Download.Target.Name = names[1]
			second, err := dispatchDownload(t.Context(), router, "worker", req)
			require.NoError(t, err)
			require.NotEqual(t, first.Key, second.Key)
			require.NotEqual(t, first.JobID, second.JobID)
			require.Len(t, e.jobs.List(jobs.KindDownload), 2, "Linux store names are case-sensitive and are not Unicode-folded")
			require.Equal(t, e.downloads.GetDownloadStatus(first.Key).SourceKey, e.downloads.GetDownloadStatus(second.Key).SourceKey, "source identity is independent of the destination")
			require.True(t, e.downloads.StopDownload(first.Key))
			require.True(t, e.downloads.StopDownload(second.Key))
			require.Equal(t, jobs.PhaseFailed, waitJob(t, e.jobs, first.JobID).Phase)
			require.Equal(t, jobs.PhaseFailed, waitJob(t, e.jobs, second.JobID).Phase)
		})
	}
}
