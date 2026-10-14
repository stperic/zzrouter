package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

func acceptedProviderModel(t *testing.T, resp *httptest.ResponseRecorder) providerModelDownload {
	t.Helper()
	require.Equal(t, http.StatusAccepted, resp.Code, resp.Body.String())
	var out struct {
		Data providerModelDownload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &out))
	return out.Data
}

func TestProviderModelsCategoryEscapeAfterAdmission(t *testing.T) {
	h := newProviderModelHarness(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/models/org/repo", r.URL.Path, "refusal must precede the transfer")
		close(entered)
		<-resume
		_, _ = io.WriteString(w, `{"siblings":[{"rfilename":"file","size":7}]}`)
	}))
	job := acceptedProviderModel(t, h.request("POST", "studio/models", `{"node":"worker","category":"vae","source":"org/repo#file","name":"nested/file"}`, ""))
	<-entered // Admission has finished; metadata is still pending.
	root, outside := providerModelRoot(t, h.worker), t.TempDir()
	require.NoError(t, os.MkdirAll(root, 0700))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "vae")))
	unblock()
	event := waitJob(t, h.worker.jobs, job.JobID)
	require.Equal(t, jobs.PhaseFailed, event.Phase)
	require.Equal(t, string(httperr.CodeInvalidFileName), event.Meta["error_code"])
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries, "neither files, parent directories nor a manifest may escape")
	assertProviderModelProblem(t, h.request("GET", "studio/models?node=worker", "", ""), 400, httperr.CodeInvalidFileName)
	assertProviderModelProblem(t, h.request("DELETE", "studio/models?node=worker&category=vae&name=nested/file", "", ""), 400, httperr.CodeInvalidFileName)
}

func TestProviderModelsNestedRedirectCannotChangeReservation(t *testing.T) {
	h := newProviderModelHarness(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/models/org/repo", r.URL.Path, "changed admission identity must refuse before fetching")
		close(entered)
		<-resume
		_, _ = io.WriteString(w, `{"siblings":[{"rfilename":"file","size":7}]}`)
	}))
	job := acceptedProviderModel(t, h.request("POST", "studio/models", `{"node":"worker","category":"vae","source":"org/repo#file","name":"nested/file"}`, ""))
	<-entered
	root := filepath.Join(providerModelRoot(t, h.worker), "vae")
	other := filepath.Join(root, "other")
	require.NoError(t, os.MkdirAll(other, 0700))
	require.NoError(t, os.Symlink(other, filepath.Join(root, "nested")))
	unblock()
	event := waitJob(t, h.worker.jobs, job.JobID)
	require.Equal(t, jobs.PhaseFailed, event.Phase)
	require.Equal(t, string(httperr.CodeInvalidFileName), event.Meta["error_code"])
	entries, err := os.ReadDir(other)
	require.NoError(t, err)
	require.Empty(t, entries, "the admitted key cannot redirect work into another reservation")
}

type pausedProviderBody struct {
	io.Reader
	entered, resume chan struct{}
	once            sync.Once
}

func (b *pausedProviderBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.resume
	return b.Reader.Read(p)
}

func TestProviderModelsCopiedFinalSurvivesTransfer(t *testing.T) {
	h := newProviderModelHarness(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	h.worker.registry.GetHuggingFaceConnector().SetHTTPTransport(downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
		body := io.NopCloser(strings.NewReader(`{"siblings":[{"rfilename":"file.gguf","size":7}]}`))
		if r.URL.Path == "/org/repo/resolve/main/file.gguf" {
			body = io.NopCloser(&pausedProviderBody{Reader: strings.NewReader("weights"), entered: entered, resume: resume})
		} else {
			require.Equal(t, "/api/models/org/repo", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: -1, Request: r}, nil
	}))
	job := acceptedProviderModel(t, h.request("POST", "studio/models", `{"node":"worker","category":"vae","source":"org/repo#file.gguf"}`, ""))
	<-entered
	root := filepath.Join(providerModelRoot(t, h.worker), "vae")
	final := filepath.Join(root, "file.gguf")
	require.NoError(t, os.WriteFile(final, []byte("operator-owned"), 0600))
	unblock()
	event := waitJob(t, h.worker.jobs, job.JobID)
	require.Equal(t, jobs.PhaseFailed, event.Phase)
	require.Equal(t, string(httperr.CodeFileExists), event.Meta["error_code"])
	data, err := os.ReadFile(final)
	require.NoError(t, err)
	require.Equal(t, "operator-owned", string(data))
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no partial or success manifest survives the refusal")
	manifest, err := integrity.ReadManifest(root)
	require.NoError(t, err)
	require.Nil(t, manifest)
}

func TestProviderModelsRefuseCatalogRootOverlap(t *testing.T) {
	for _, relation := range []string{"equal", "inside", "contains", "symlink"} {
		t.Run(relation, func(t *testing.T) {
			h := newProviderModelHarness(t)
			h.worker.registry.GetHuggingFaceConnector().SetHTTPTransport(downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				w.WriteHeader(http.StatusNotFound)
				return w.Result(), nil
			}))
			store := providerModelRoot(t, h.worker)
			require.NoError(t, os.MkdirAll(filepath.Join(store, "vae"), 0700))
			final := filepath.Join(store, "vae/file")
			require.NoError(t, os.WriteFile(final, []byte("operator"), 0600))
			catalog := store
			switch relation {
			case "inside":
				catalog = filepath.Dir(store)
			case "contains":
				catalog = filepath.Join(store, "catalog")
			case "symlink":
				catalog = filepath.Join(t.TempDir(), "catalog")
				require.NoError(t, os.Symlink(store, catalog))
			}
			modelregistry.SetModelsRootDirOverride(catalog)
			for _, request := range []struct{ method, path, body string }{
				{"POST", "studio/models", `{"node":"worker","category":"vae","source":"org/repo#missing"}`},
				{"GET", "studio/models?node=worker", ""},
				{"DELETE", "studio/models?node=worker&category=vae&name=file", ""},
			} {
				assertProviderModelProblem(t, h.request(request.method, request.path, request.body, ""), 400, httperr.CodeProviderModelStoreUnsupported)
			}
			require.Empty(t, h.worker.jobs.List(jobs.KindDownload), "overlap refuses before a job is admitted")
			data, err := os.ReadFile(final)
			require.NoError(t, err)
			require.Equal(t, "operator", string(data))
		})
	}
}

func TestProviderModelsInterruptedPersistenceRetryDeleteAndCatalogIsolation(t *testing.T) {
	for _, state := range []string{"pending", "downloading"} {
		t.Run(state, func(t *testing.T) {
			h := newProviderModelHarness(t)
			path := filepath.Join(t.TempDir(), "downloads.json")
			h.worker.downloads.Stop()
			tracker := modelregistry.NewDownloadTrackerWithPersistence(path)
			h.worker.downloads = tracker
			body := `{"node":"worker","category":"vae","source":"org/repo#file.gguf"}`
			req := metadata.DownloadRequest{Repo: "org/repo", Weights: "file.gguf", Target: metadata.DownloadTarget{Owner: metadata.OwnerProviderStore, Provider: "studio", Category: "vae", Name: "file.gguf"}}
			wire := internalDownloadRequest{Model: req.Repo, Registry: constants.RepoHuggingFace, Download: &req}
			require.NoError(t, wire.validateTarget())
			admission, err := h.worker.resolveDownloadAdmission(wire, wire.Model, wire.Registry)
			require.NoError(t, err)
			// Persist actual admission fields with no live job or producer after restart.
			gen := tracker.StartDownload(admission.key, admission.model, nil, admission.options)
			if state == "downloading" {
				require.NoError(t, tracker.UpdateDownloadFullGen(admission.key, gen, state, req.Weights, 0, 1, 30, 2, 7, 1))
			}
			tracker.Stop()
			reloaded := modelregistry.NewDownloadTrackerWithPersistence(path)
			t.Cleanup(reloaded.Stop)
			h.worker.downloads = reloaded
			restored := reloaded.GetDownloadStatus(admission.key)
			require.Equal(t, "failed", restored.Status)
			require.Equal(t, "interrupted (node restarted)", restored.Error)
			require.Equal(t, req.Target, restored.Target)
			require.Equal(t, admission.options.SourceKey, restored.SourceKey)
			require.Empty(t, restored.ModelName)
			require.Empty(t, restored.RequestedFile)
			c, w := mutationContext(t, http.MethodGet, "/zzrouter/v1/internal/deployments", nil)
			h.worker.HandleInternalListDeployments(c)
			require.JSONEq(t, `{"data":[]}`, w.Body.String(), "provider persistence never enters LLM deployment listings")
			// A terminal interrupted entry cannot reserve or hide an operator's file.
			require.NoError(t, os.MkdirAll(filepath.Dir(admission.path), 0700))
			require.NoError(t, os.WriteFile(admission.path, []byte("manual"), 0600))
			deleted := h.request("DELETE", "studio/models?node=worker&category=vae&name=file.gguf", "", "")
			require.Equal(t, http.StatusOK, deleted.Code, deleted.Body.String())
			installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/models/org/repo" {
					_, _ = io.WriteString(w, `{"siblings":[{"rfilename":"file.gguf","size":7}]}`)
				} else {
					require.Equal(t, "/org/repo/resolve/main/file.gguf", r.URL.Path)
					_, _ = io.WriteString(w, "weights")
				}
			}))
			job := acceptedProviderModel(t, h.request("POST", "studio/models", body, ""))
			require.Equal(t, jobs.PhaseDone, waitJob(t, h.worker.jobs, job.JobID).Phase)
			data, err := os.ReadFile(admission.path)
			require.NoError(t, err)
			require.Equal(t, "weights", string(data))
			models, err := h.worker.registry.ListAllModels()
			require.NoError(t, err)
			require.Empty(t, models, "provider files remain outside scans after reloaded work is retried")
		})
	}
}
