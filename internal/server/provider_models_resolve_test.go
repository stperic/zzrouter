package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stretchr/testify/require"
)

type workflowFixtureModel struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Directory string `json:"directory"`
}

func templateWorkflow(t *testing.T, envelope bool) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/comfyui/flux_schnell_ui.json")
	require.NoError(t, err)
	if !envelope {
		return raw
	}
	wrapped, err := json.Marshal(map[string]any{
		"client_id":  "fixture-client",
		"prompt":     map[string]any{"39": map[string]any{"class_type": "VAELoader", "inputs": map[string]any{"vae_name": "ignored-api-input.safetensors"}}},
		"extra_data": map[string]any{"extra_pnginfo": map[string]any{"workflow": json.RawMessage(raw)}},
	})
	require.NoError(t, err)
	return wrapped
}

func workflowWithModels(t *testing.T, models []workflowFixtureModel) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"nodes": []any{map[string]any{
		"type": "VAELoader", "widgets_values": []string{"ignored-widget.safetensors"},
		"inputs":     map[string]any{"vae_name": "ignored-input.safetensors"},
		"properties": map[string]any{"models": models},
	}}, "models": []any{map[string]any{"name": "ignored-top-level.safetensors"}}})
	require.NoError(t, err)
	return raw
}

func resolveBody(t *testing.T, workflow json.RawMessage, download bool) string {
	t.Helper()
	raw, err := json.Marshal(providerModelResolveRequest{Node: "worker", Workflow: workflow, Download: download})
	require.NoError(t, err)
	return string(raw)
}

func readResolution(t *testing.T, w *httptest.ResponseRecorder, status int) providerModelResolution {
	t.Helper()
	require.Equal(t, status, w.Code, w.Body.String())
	var out struct {
		Data providerModelResolution `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Equal(t, "studio", out.Data.Provider)
	require.Equal(t, "worker", out.Data.Node)
	return out.Data
}

func declareTemplateCategories(t *testing.T, e *DeploymentsExecutor) {
	t.Helper()
	// Install a new config snapshot rather than mutating the published one.
	next := &config.AppsConfig{}
	e.appsConfig().RangeApps(func(name string, sc config.ServiceConfig) bool {
		if name == "studio" {
			runtime, store := *sc.Runtime, *sc.Runtime.ModelStore
			store.Categories = []string{"vae", "text_encoders", "diffusion_models", "loras"}
			runtime.ModelStore, sc.Runtime = &store, &runtime
		}
		require.NoError(t, next.AddApp(name, sc))
		return true
	})
	e.appsConfig = func() *config.AppsConfig { return next }
}

func writeWorkflowFile(t *testing.T, root, name, content string) {
	t.Helper()
	file := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0700))
	require.NoError(t, os.WriteFile(file, []byte(content), 0600))
}

func TestProviderWorkflowRealTemplateUIAndEnvelope(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		t.Run(fmt.Sprint(envelope), func(t *testing.T) {
			h := newProviderModelHarness(t)
			declareTemplateCategories(t, h.worker)
			root := providerModelRoot(t, h.worker)
			writeWorkflowFile(t, root, "vae/ae.safetensors", "manual weights")
			writeWorkflowFile(t, root, "text_encoders/clip_l.safetensors", "")
			writeWorkflowFile(t, providerModelRoot(t, h.coordinator), "text_encoders/t5xxl_fp16.safetensors", "coordinator-only")
			outside := filepath.Join(t.TempDir(), "outside")
			require.NoError(t, os.WriteFile(outside, []byte("outside weights"), 0600))
			require.NoError(t, os.MkdirAll(filepath.Join(root, "diffusion_models"), 0700))
			require.NoError(t, os.Symlink(outside, filepath.Join(root, "diffusion_models/flux1-schnell.safetensors")))
			out := readResolution(t, h.request("POST", "studio/models/resolve", resolveBody(t, templateWorkflow(t, envelope), false), ""), 200)
			require.Len(t, out.Requirements, 4)
			for i, item := range out.Requirements {
				want := "missing"
				if i == 0 {
					want = "present"
				}
				require.Equal(t, want, item.Status)
				require.Empty(t, item.Reason)
				require.Empty(t, item.JobID)
			}
			require.Equal(t, "clip_l.safetensors", out.Requirements[1].Name)
			require.Equal(t, "t5xxl_fp16.safetensors", out.Requirements[2].Name)
			require.Equal(t, "diffusion_models", out.Requirements[3].Category)
			require.EqualValues(t, 1, h.cluster.calls.Load(), "one inventory call for all files")
			require.Empty(t, h.worker.jobs.List(jobs.KindDownload))
		})
	}
}

func TestProviderWorkflowDedupeSourcesAndStatuses(t *testing.T) {
	h := newProviderModelHarness(t)
	root := providerModelRoot(t, h.worker)
	writeWorkflowFile(t, root, "vae/manual", "copied")
	writeWorkflowFile(t, root, "vae/conflict", "copied")
	writeWorkflowFile(t, root, "vae/empty", "")
	models := []workflowFixtureModel{
		{"same", "org/repo#nested/file", "vae"},
		{"same", "https://huggingface.co/org/repo/resolve/main/nested/file", "vae"},
		{"same", "org/repo#nested/file", "loras"},
		{"conflict", "org/repo#file", "vae"},
		{"conflict", "https://huggingface.co/org/repo/resolve/v2/file", "vae"},
		{"manual", "", "vae"},
		{"manual", "https://example.org/model", "vae"},
		{"supplied", "", "vae"},
		{"supplied", "org/repo#file", "vae"},
		{"supplied", "", "vae"},
		{"no-source", "", "vae"},
		{"unsupported", "https://example.org/model", "vae"},
		{"bad-category", "org/repo#file", "../outside"},
		{"bad-category", "org/other#file", "../outside"},
		{"empty", "org/repo#file", "vae"},
		{"case", "org/repo#file", "vae"},
		{"Case", "org/repo#file", "vae"},
		{"unsupported-conflict", "https://example.org/one", "vae"},
		{"unsupported-conflict", "https://example.org/two", "vae"},
	}
	out := readResolution(t, h.request("POST", "studio/models/resolve", resolveBody(t, workflowWithModels(t, models), false), ""), 200)
	wants := []struct{ status, reason string }{
		{"missing", ""}, {"missing", ""}, {"unresolvable", "conflicting_source"}, {"present", ""},
		{"missing", ""}, {"unresolvable", "missing_source"}, {"unresolvable", "source_unsupported"},
		{"unresolvable", "invalid_model_category"}, {"missing", ""}, {"missing", ""}, {"missing", ""},
		{"unresolvable", "conflicting_source"},
	}
	require.Len(t, out.Requirements, len(wants))
	for i, want := range wants {
		require.Equal(t, want.status, out.Requirements[i].Status, "item %d", i)
		require.Equal(t, want.reason, out.Requirements[i].Reason, "item %d", i)
	}
	require.Equal(t, "org/repo#file", out.Requirements[4].Source)
	require.EqualValues(t, 1, h.cluster.calls.Load())
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload))
}

func TestProviderWorkflowDownloadsThroughExistingService(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		t.Run(fmt.Sprint(envelope), func(t *testing.T) {
			h := newProviderModelHarness(t)
			declareTemplateCategories(t, h.worker)
			root := providerModelRoot(t, h.worker)
			for _, file := range []string{"text_encoders/clip_l.safetensors", "text_encoders/t5xxl_fp16.safetensors", "diffusion_models/flux1-schnell.safetensors"} {
				writeWorkflowFile(t, root, file, "already present")
			}
			const content = "fixture-vae"
			var fetched atomic.Int32
			installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/models/Comfy-Org/Lumina_Image_2.0_Repackaged":
					_ = json.NewEncoder(w).Encode(map[string]any{"siblings": []any{map[string]any{"rfilename": "split_files/vae/ae.safetensors", "size": len(content)}}})
				case "/Comfy-Org/Lumina_Image_2.0_Repackaged/resolve/main/split_files/vae/ae.safetensors":
					fetched.Add(1)
					w.Header().Set("Content-Length", fmt.Sprint(len(content)))
					_, _ = io.WriteString(w, content)
				default:
					t.Errorf("unexpected HF request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			body := resolveBody(t, templateWorkflow(t, envelope), true)
			first := h.request("POST", "studio/models/resolve", body, "resolve-1")
			out := readResolution(t, first, 202)
			require.Len(t, out.Requirements, 4)
			item := out.Requirements[0]
			require.Equal(t, "downloading", item.Status)
			require.NotEmpty(t, item.JobID)
			for _, other := range out.Requirements[1:] {
				require.Equal(t, "present", other.Status)
				require.Empty(t, other.JobID)
			}
			require.EqualValues(t, 2, h.cluster.calls.Load(), "one list and one shared download dispatch")
			event := waitJob(t, h.worker.jobs, item.JobID)
			require.Equal(t, jobs.PhaseDone, event.Phase, event.Err)
			require.Equal(t, jobs.KindDownload, event.Kind)
			require.NotContains(t, event.Meta, "workflow")
			bytes, err := os.ReadFile(filepath.Join(root, "vae/ae.safetensors"))
			require.NoError(t, err)
			require.Equal(t, content, string(bytes))
			manifest, err := integrity.ReadManifest(filepath.Join(root, "vae"))
			require.NoError(t, err)
			require.Equal(t, "ae.safetensors", manifest.Files[0].RelativePath)
			_, err = os.Stat(providerModelRoot(t, h.coordinator))
			require.ErrorIs(t, err, os.ErrNotExist)
			replay := h.request("POST", "studio/models/resolve", body, "resolve-1")
			require.Equal(t, first.Body.String(), replay.Body.String())
			require.EqualValues(t, 2, h.cluster.calls.Load())
			require.EqualValues(t, 1, fetched.Load())
			allPresent := readResolution(t, h.request("POST", "studio/models/resolve", body, ""), 200)
			for _, item := range allPresent.Requirements {
				require.Equal(t, "present", item.Status)
			}
			require.EqualValues(t, 3, h.cluster.calls.Load())
			require.Len(t, h.worker.jobs.List(jobs.KindDownload), 1)
			catalog := catalogRoute(gin.RouteInfo{Method: "POST", Path: "/zzrouter/v1/providers/:name/models/resolve"})
			require.Equal(t, "admin", catalog.Auth)
			require.True(t, catalog.Async)
			id, err := (&Server{engine: h.public}).eligibleAPIRoute("POST", "/zzrouter/v1/providers/studio/models/resolve")
			require.NoError(t, err)
			require.Equal(t, "resolveProviderModels", id)
		})
	}
}

func TestProviderWorkflowSharesLiveDownloadAndSkipsUnresolvable(t *testing.T) {
	h := newProviderModelHarness(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			close(entered)
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{"siblings": []any{map[string]any{"rfilename": "file", "size": 7}}})
			return
		}
		w.Header().Set("Content-Length", "7")
		_, _ = io.WriteString(w, "weights")
	}))
	first := h.request("POST", "studio/models", `{"node":"worker","category":"vae","source":"org/repo#file"}`, "")
	require.Equal(t, 202, first.Code, first.Body.String())
	var direct struct {
		Data providerModelDownload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &direct))
	<-entered
	workflow := workflowWithModels(t, []workflowFixtureModel{
		{"file", "https://huggingface.co/org/repo/resolve/main/file", "vae"},
		{"file", "org/repo#file", "vae"},
		{"no-source", "", "vae"}, {"unsupported", "https://example.org/model", "vae"}, {"invalid-category", "org/repo#file", "invalid"},
		{"conflict", "org/repo#file", "vae"}, {"conflict", "org/other#file", "vae"},
	})
	out := readResolution(t, h.request("POST", "studio/models/resolve", resolveBody(t, workflow, true), ""), 202)
	require.Equal(t, direct.Data.JobID, out.Requirements[0].JobID)
	require.Equal(t, "downloading", out.Requirements[0].Status)
	for _, item := range out.Requirements[1:] {
		require.Equal(t, "unresolvable", item.Status)
		require.Empty(t, item.JobID)
	}
	require.Len(t, h.worker.jobs.List(jobs.KindDownload), 1)
	require.EqualValues(t, 3, h.cluster.calls.Load(), "direct download, one inventory, shared admission")
	finish()
	require.Equal(t, jobs.PhaseDone, waitJob(t, h.worker.jobs, direct.Data.JobID).Phase)
}

func TestProviderWorkflowStartFailureKeepsAcceptedJobIDs(t *testing.T) {
	for _, transportFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(transportFailure), func(t *testing.T) {
			h := newProviderModelHarness(t)
			root := providerModelRoot(t, h.worker)
			if transportFailure {
				worker := h.cluster.worker
				h.cluster.worker = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					worker.ServeHTTP(w, r)
					if r.Method == http.MethodPost {
						h.cluster.offline = true
					}
				})
			} else {
				// Inventory considers a zero-byte file missing; admission refuses overwrite.
				writeWorkflowFile(t, root, "vae/second", "")
			}
			installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/models/") {
					_ = json.NewEncoder(w).Encode(map[string]any{"siblings": []any{map[string]any{"rfilename": "file", "size": 7}}})
					return
				}
				w.Header().Set("Content-Length", "7")
				_, _ = io.WriteString(w, "weights")
			}))
			workflow := workflowWithModels(t, []workflowFixtureModel{
				{"first", "org/repo#file", "vae"}, {"second", "org/repo#file", "vae"}, {"third", "org/repo#file", "vae"},
			})
			out := readResolution(t, h.request("POST", "studio/models/resolve", resolveBody(t, workflow, true), ""), 202)
			require.Len(t, out.Requirements, 3)
			first, second, third := out.Requirements[0], out.Requirements[1], out.Requirements[2]
			require.Equal(t, "downloading", first.Status)
			require.NotEmpty(t, first.JobID, "a later refusal must retain the accepted job id")
			require.Equal(t, "failed", second.Status)
			require.Empty(t, second.JobID)
			wantCode := string(httperr.CodeFileExists)
			if transportFailure {
				wantCode = string(httperr.CodeNodeUnreachable)
				require.Equal(t, "failed", third.Status)
				require.Equal(t, wantCode, third.Reason)
				require.Empty(t, third.JobID)
				require.Len(t, h.worker.jobs.List(jobs.KindDownload), 1)
			} else {
				require.Equal(t, "downloading", third.Status, "a refusal must not stop later starts")
				require.NotEmpty(t, third.JobID)
				require.NotEqual(t, first.JobID, third.JobID)
				require.Equal(t, jobs.PhaseDone, waitJob(t, h.worker.jobs, third.JobID).Phase)
				require.Len(t, h.worker.jobs.List(jobs.KindDownload), 2)
			}
			require.Equal(t, wantCode, second.Reason)
			require.EqualValues(t, 4, h.cluster.calls.Load(), "one inventory read and all three start attempts")
			require.Equal(t, jobs.PhaseDone, waitJob(t, h.worker.jobs, first.JobID).Phase)
			bytes, err := os.ReadFile(filepath.Join(root, "vae/first"))
			require.NoError(t, err)
			require.Equal(t, "weights", string(bytes))
			require.Empty(t, h.coordinator.jobs.List(jobs.KindDownload))
		})
	}
}

func TestProviderWorkflowKeepsOriginalRoutingSelector(t *testing.T) {
	h := newProviderModelHarness(t)
	// A reported label must not replace the selected endpoint's routing identity.
	h.worker.getNodename = func() string { return "coordinator" }
	installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"siblings": []any{map[string]any{"rfilename": "file", "size": 7}}})
			return
		}
		w.Header().Set("Content-Length", "7")
		_, _ = io.WriteString(w, "weights")
	}))
	workflow := workflowWithModels(t, []workflowFixtureModel{{"file", "org/repo#file", "vae"}})
	w := h.request("POST", "studio/models/resolve", resolveBody(t, workflow, true), "")
	require.Equal(t, 202, w.Code, w.Body.String())
	require.EqualValues(t, 2, h.cluster.calls.Load(), "both inventory and download must use the original worker selector")
	workerJobs := h.worker.jobs.List(jobs.KindDownload)
	require.Len(t, workerJobs, 1)
	require.Empty(t, h.coordinator.jobs.List(jobs.KindDownload))
	require.Equal(t, jobs.PhaseDone, waitJob(t, h.worker.jobs, workerJobs[0].JobID).Phase)
	bytes, err := os.ReadFile(filepath.Join(providerModelRoot(t, h.worker), "vae/file"))
	require.NoError(t, err)
	require.Equal(t, "weights", string(bytes))
	_, err = os.Stat(providerModelRoot(t, h.coordinator))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestProviderWorkflowRefusalsBeforeDownloads(t *testing.T) {
	h := newProviderModelHarness(t)
	for _, raw := range []string{
		`{"39":{"class_type":"VAELoader","inputs":{"vae_name":"ae.safetensors"}}}`,
		`{"prompt":{"39":{"class_type":"VAELoader","inputs":{"vae_name":"ae.safetensors"}}}}`,
		`{"nodes":[{"type":"VAELoader","widgets_values":["ae.safetensors"]}]}`,
		`{"extra_data":{"extra_pnginfo":{"workflow":{"nodes":[]}}}}`,
	} {
		w := h.request("POST", "studio/models/resolve", resolveBody(t, json.RawMessage(raw), true), "")
		require.Equal(t, 400, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "workflow carries no model metadata")
	}
	for _, raw := range []string{`[]`, `"bad"`, `{"nodes":{}}`, `{"nodes":[{"properties":{"models":{}}}]}`, `{"nodes":[{"properties":{"models":[{"name":42}]}}]}`} {
		w := h.request("POST", "studio/models/resolve", resolveBody(t, json.RawMessage(raw), true), "")
		require.Equal(t, 400, w.Code, w.Body.String())
	}
	for _, badName := range []string{"", "../outside", "/absolute", `dir\file`, "C:/drive", ".zzrouter-manifest.json", "dir/file.zzpart"} {
		workflow := workflowWithModels(t, []workflowFixtureModel{{"safe", "org/repo#file", "vae"}, {badName, "org/repo#file", "vae"}})
		assertProviderModelProblem(t, h.request("POST", "studio/models/resolve", resolveBody(t, workflow, true), ""), 400, httperr.CodeInvalidFileName)
	}
	require.Zero(t, h.cluster.calls.Load(), "all structure and names validated before even inventory")
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload))
	valid := resolveBody(t, templateWorkflow(t, false), true)
	assertProviderModelProblem(t, h.request("POST", "studio/models/resolve", strings.Replace(valid, `"worker"`, `"unknown"`, 1), ""), 404, httperr.CodeUnknownNode)
	assertProviderModelProblem(t, h.request("POST", "studio/models/resolve", strings.Replace(valid, `"worker"`, `"*"`, 1), ""), 400, httperr.CodeRequired)
	assertProviderModelProblem(t, h.request("POST", "remote/models/resolve", valid, ""), 400, httperr.CodeProviderModelStoreUnsupported)
	require.Equal(t, 404, h.request("POST", "ghost/models/resolve", valid, "").Code)
	h.cluster.offline = true
	require.Equal(t, 502, h.request("POST", "studio/models/resolve", valid, "").Code)
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload))
}

func TestProviderWorkflowZeroSizeCannotOverwrite(t *testing.T) {
	h := newProviderModelHarness(t)
	root := providerModelRoot(t, h.worker)
	writeWorkflowFile(t, root, "vae/empty", "")
	workflow := workflowWithModels(t, []workflowFixtureModel{{"empty", "org/repo#file", "vae"}})
	out := readResolution(t, h.request("POST", "studio/models/resolve", resolveBody(t, workflow, false), ""), 200)
	require.Equal(t, "missing", out.Requirements[0].Status)
	failed := readResolution(t, h.request("POST", "studio/models/resolve", resolveBody(t, workflow, true), ""), 200)
	require.Equal(t, "failed", failed.Requirements[0].Status)
	require.Equal(t, string(httperr.CodeFileExists), failed.Requirements[0].Reason)
	require.Empty(t, failed.Requirements[0].JobID)
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload))
	info, err := os.Stat(filepath.Join(root, "vae/empty"))
	require.NoError(t, err)
	require.Zero(t, info.Size())
}

func TestProviderWorkflowBodyBoundBeforeIdempotencyAndStrictBinding(t *testing.T) {
	h := newProviderModelHarness(t)
	valid := resolveBody(t, workflowWithModels(t, []workflowFixtureModel{{"file", "org/repo#file", "vae"}}), true)
	for _, idem := range []string{"", "bound-1"} {
		for _, unknownLength := range []bool{false, true} {
			for _, trailing := range []bool{false, true} {
				t.Run(fmt.Sprintf("idem=%t/unknown=%t/trailing=%t", idem != "", unknownLength, trailing), func(t *testing.T) {
					body := valid + strings.Repeat(" ", providerWorkflowMaxBytes)
					if !trailing {
						body = strings.Replace(valid, `"workflow":{`, `"workflow":{"ignored":"`+strings.Repeat("a", providerWorkflowMaxBytes)+`",`, 1)
					}
					r := httptest.NewRequest("POST", "/zzrouter/v1/providers/studio/models/resolve", strings.NewReader(body))
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Idempotency-Key", idem)
					if unknownLength {
						r.ContentLength = -1
					}
					w := httptest.NewRecorder()
					h.public.ServeHTTP(w, r)
					require.Equal(t, 413, w.Code, w.Body.String())
					require.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
					require.Contains(t, w.Body.String(), fmt.Sprint(providerWorkflowMaxBytes))
				})
			}
		}
	}
	require.Zero(t, h.cluster.calls.Load())
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload))
}
