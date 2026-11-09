package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/internal/server/mcpserver"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	provapps "github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stretchr/testify/require"
)

func installTemplateSource(t *testing.T, h *providerModelHarness, handler http.Handler) {
	t.Helper()
	source := httptest.NewServer(handler)
	t.Cleanup(source.Close)
	h.service.templates = provapps.NewWorkflowTemplateCatalog(&http.Client{Transport: downloadTargetTransport(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL.Scheme = "http"
		copy.URL.Host = strings.TrimPrefix(source.URL, "http://")
		return http.DefaultTransport.RoundTrip(copy)
	})})
}

func templateMCPServer(h *providerModelHarness) *Server {
	server := &Server{engine: h.public}
	h.public.POST("/zzrouter/v1/mcp", gin.WrapH(mcpserver.New(mcpserver.Config{Engine: h.public, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath, SnapshotAllowed: snapshotAllowed, SearchAPI: searchOpenAPI, EligibleAPIRoute: server.eligibleAPIRoute})))
	return server
}

func readMCPTemplateResolution(t *testing.T, server *Server) providerModelResolution {
	t.Helper()
	result := callMCPWireTool(t, server, "zz_api", map[string]any{"method": "POST", "path": "/zzrouter/v1/providers/studio/models/resolve", "idempotency_key": "template-1", "body": map[string]any{"node": "worker", "template": "turbo", "download": true}})
	require.NotEqual(t, true, result["isError"], result)
	raw, err := json.Marshal(result["structuredContent"].(map[string]any)["response"].(map[string]any)["data"])
	require.NoError(t, err)
	var out providerModelResolution
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestProviderTemplateResolveSharedDownloadAndReplay(t *testing.T) {
	h := newProviderModelHarness(t)
	declareTemplateCategories(t, h.worker)
	raw, err := os.ReadFile("testdata/comfyui/image_qwen_image_2_1_turbo_t2i.json")
	require.NoError(t, err)
	items, err := workflowRequirements(raw)
	require.NoError(t, err)
	for _, item := range items[1:] {
		writeWorkflowFile(t, providerModelRoot(t, h.worker), filepath.Join(item.Category, item.Name), "present")
	}
	var catalogReads atomic.Int32
	var upstreamChanged atomic.Bool
	installTemplateSource(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "index.json") {
			catalogReads.Add(1)
			_, _ = io.WriteString(w, `[{"templates":[{"name":"turbo","title":"Qwen Turbo"},{"name":"edit","title":"Edit"}]}]`)
			return
		}
		if upstreamChanged.Load() {
			_, _ = io.WriteString(w, `{"nodes":[]}`)
			return
		}
		_, _ = w.Write(raw)
	}))
	entered, release := make(chan struct{}), make(chan struct{})
	var fetched atomic.Int32
	installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"siblings": []any{map[string]any{"rfilename": "diffusion_models/" + items[0].Name, "size": 7}}})
			return
		}
		if fetched.Add(1) == 1 {
			close(entered)
		}
		<-release
		w.Header().Set("Content-Length", "7")
		_, _ = io.WriteString(w, "weights")
	}))
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	server := templateMCPServer(h)
	first := readMCPTemplateResolution(t, server)
	require.NotEmpty(t, first.Requirements)
	require.Equal(t, "downloading", first.Requirements[0].Status)
	require.NotEmpty(t, first.Requirements[0].JobID)
	ready, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ready.Done():
		t.Fatal("accepted model download did not start")
	}
	second := readResolution(t, h.request("POST", "studio/models/resolve", `{"node":"worker","template":"Edit","download":true}`, "template-2"), 202)
	require.Equal(t, first.Requirements[0].JobID, second.Requirements[0].JobID)
	require.Equal(t, "turbo", first.Template.ID)
	digest := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(digest[:]), first.Template.SHA256, "digest identifies fetched source bytes")
	require.Equal(t, "supported", first.Template.ModelProvisioning)
	upstreamChanged.Store(true)
	replay := readMCPTemplateResolution(t, server)
	require.Equal(t, first.Requirements[0].JobID, replay.Requirements[0].JobID)
	require.Equal(t, first.Template.SHA256, replay.Template.SHA256)
	require.EqualValues(t, 2, catalogReads.Load(), "accepted replay does not refetch upstream")
	upstreamChanged.Store(false)
	unblock()
	event := waitJob(t, h.worker.jobs, first.Requirements[0].JobID)
	require.Equal(t, jobs.PhaseDone, event.Phase, event.Err)
	require.EqualValues(t, 1, fetched.Load())
	require.Empty(t, h.coordinator.jobs.List(jobs.KindDownload))
	_, err = os.Stat(filepath.Join(providerModelRoot(t, h.coordinator), items[0].Category, items[0].Name))
	require.ErrorIs(t, err, os.ErrNotExist)
	resolved := readResolution(t, h.request("POST", "studio/models/resolve", `{"node":"worker","template":"Qwen Turbo"}`, ""), 200)
	for _, item := range resolved.Requirements {
		require.Equal(t, "present", item.Status)
	}
}

func TestProviderTemplateCatalogAndInputRefusals(t *testing.T) {
	h := newProviderModelHarness(t)
	var reads atomic.Int32
	installTemplateSource(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if !strings.HasSuffix(r.URL.Path, "index.json") {
			_, _ = io.WriteString(w, `{"nodes":[]}`)
			return
		}
		_, _ = io.WriteString(w, `[{"templates":[{"name":"b","title":"Turbo edit","openSource":true},{"name":"a","title":"Turbo image","openSource":true},{"name":"api","title":"API","openSource":false}]}]`)
	}))
	response := h.request("GET", "studio/templates?node=worker&search=TURBO&limit=1&offset=1", "", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	var out struct {
		Data  []provapps.WorkflowTemplate `json:"data"`
		Total int                         `json:"total"`
		More  bool                        `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	require.Equal(t, 2, out.Total)
	require.False(t, out.More)
	require.Equal(t, "b", out.Data[0].ID)
	require.Equal(t, "unverified", out.Data[0].ModelProvisioning)
	before := reads.Load()
	for _, fields := range []string{``, `,"template":null`, `,"template":""`, `,"template":"  "`, `,"template":{}`, `,"workflow":null`, `,"workflow":{},"template":"a"`, `,"workflow":{},"template":null`, `,"workflow":{},"include_workflow":true`} {
		response = h.request("POST", "studio/models/resolve", fmt.Sprintf(`{"node":"worker"%s}`, fields), "")
		require.Equal(t, 400, response.Code, response.Body.String())
	}
	require.Equal(t, before, reads.Load(), "invalid input does not fetch upstream")
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload))
	for _, tc := range []struct {
		selector string
		status   int
		code     httperr.RouteErrorCode
	}{{"absent", 404, httperr.CodeTemplateNotFound}, {"API", 400, httperr.CodeTemplateUnsupported}} {
		response = h.request("POST", "studio/models/resolve", fmt.Sprintf(`{"node":"worker","template":%q,"download":true}`, tc.selector), "")
		require.Equal(t, tc.status, response.Code, response.Body.String())
		var problem struct {
			Code string `json:"code"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
		require.Equal(t, string(tc.code), problem.Code)
	}
	require.Equal(t, 400, h.request("GET", "studio/templates?node=worker&limit=-1", "", "").Code)
	assertProviderModelProblem(t, h.request("GET", "remote/templates?node=worker", "", ""), 400, httperr.CodeProviderModelStoreUnsupported)
	assertProviderModelProblem(t, h.request("GET", "studio/templates?node=unknown", "", ""), 404, httperr.CodeUnknownNode)
}

func TestProviderTemplateReadAndProvisionExactGraph(t *testing.T) {
	h := newProviderModelHarness(t)
	const raw = "{\n\"nodes\":[{\"properties\":{\"models\":[{\"name\":\"model.safetensors\",\"url\":\"org/repo#model.safetensors\",\"directory\":\"vae\"}]}}],\"extra\":{\"label\":\"A < B\"}}"
	var current atomic.Value
	current.Store(raw)
	var workflowReads atomic.Int32
	installTemplateSource(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "index.json") {
			_, _ = io.WriteString(w, `[{"templates":[{"name":"one","title":"Image template"},{"name":"api","title":"API","openSource":false}]}]`)
			return
		}
		workflowReads.Add(1)
		_, _ = io.WriteString(w, current.Load().(string))
	}))
	read := h.request("GET", "studio/templates/one?node=worker", "", "")
	require.Equal(t, 200, read.Code, read.Body.String())
	var selected struct {
		Data providerTemplate `json:"data"`
	}
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &selected))
	require.JSONEq(t, raw, string(selected.Data.Workflow))
	digest := sha256.Sum256([]byte(raw))
	require.Equal(t, hex.EncodeToString(digest[:]), selected.Data.Template.SHA256)
	require.Equal(t, "unverified", selected.Data.Template.ModelProvisioning)
	require.Equal(t, 404, h.request("GET", "studio/templates/Image%20template?node=worker", "", "").Code, "GET resources use canonical IDs; title selection remains on POST")
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload))
	apiOnly := h.request("GET", "studio/templates/api?node=worker", "", "")
	require.Equal(t, 200, apiOnly.Code, apiOnly.Body.String())
	before := workflowReads.Load()
	current.Store(`{"nodes":[]}`)
	installProviderHTTPFixture(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			_, _ = io.WriteString(w, `{"siblings":[{"rfilename":"model.safetensors","size":7}]}`)
			return
		}
		w.Header().Set("Content-Length", "7")
		_, _ = io.WriteString(w, "weights")
	}))
	body, err := json.Marshal(providerModelResolveRequest{Node: "worker", Workflow: selected.Data.Workflow, Download: true})
	require.NoError(t, err)
	result := readResolution(t, h.request("POST", "studio/models/resolve", string(body), "read-then-provision"), 202)
	require.Equal(t, jobs.PhaseDone, waitJob(t, h.worker.jobs, result.Requirements[0].JobID).Phase)
	require.Equal(t, before, workflowReads.Load(), "provisioning the received graph must not refetch changed upstream")
	file, err := os.ReadFile(filepath.Join(providerModelRoot(t, h.worker), "vae", "model.safetensors"))
	require.NoError(t, err)
	require.Equal(t, "weights", string(file))
}

func TestProviderTemplateLargeMCPReadHasNoSideEffects(t *testing.T) {
	h := newProviderModelHarness(t)
	raw := `{"nodes":[],"extra":{"note":"` + strings.Repeat("x", mcpserver.MaxResponseBytes+1) + `"}}`
	installTemplateSource(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "index.json") {
			_, _ = io.WriteString(w, `[{"templates":[{"name":"large","title":"Large"}]}]`)
			return
		}
		_, _ = io.WriteString(w, raw)
	}))
	rest := h.request("GET", "studio/templates/large?node=worker", "", "")
	require.Equal(t, 200, rest.Code)
	require.Greater(t, rest.Body.Len(), idempotencyMaxEntryBytes)
	server := templateMCPServer(h)
	result := callMCPWireTool(t, server, "zz_api_get", map[string]any{"path": "/zzrouter/v1/providers/studio/templates/large", "query": map[string]any{"node": "worker"}})
	require.Equal(t, true, result["isError"])
	wire, err := json.Marshal(result)
	require.NoError(t, err)
	require.Contains(t, string(wire), "mcp_response_too_large")
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload))
	require.Empty(t, h.coordinator.jobs.List(jobs.KindDownload))
}

func TestProviderTemplateProvisioningEvidence(t *testing.T) {
	h := newProviderModelHarness(t)
	writeWorkflowFile(t, providerModelRoot(t, h.worker), "vae/model.safetensors", "weights")
	installTemplateSource(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "index.json"):
			_, _ = io.WriteString(w, `[{"templates":[{"name":"known","title":"Known"},{"name":"unsupported","title":"Unsupported"},{"name":"unknown","title":"Unknown"}]}]`)
		case strings.HasSuffix(r.URL.Path, "unknown.json"):
			_, _ = io.WriteString(w, `{"nodes":[]}`)
		default:
			category := "vae"
			if strings.HasSuffix(r.URL.Path, "unsupported.json") {
				category = "undeclared"
			}
			_, _ = fmt.Fprintf(w, `{"nodes":[{"properties":{"models":[{"name":"model.safetensors","url":"org/repo#model.safetensors","directory":%q}]}}]}`, category)
		}
	}))
	for _, selector := range []string{"known", "unsupported"} {
		result := readResolution(t, h.request("POST", "studio/models/resolve", fmt.Sprintf(`{"node":"worker","template":%q}`, selector), ""), 200)
		if selector == "known" {
			require.Equal(t, "supported", result.Template.ModelProvisioning)
			require.Equal(t, "present", result.Requirements[0].Status)
		} else {
			require.Equal(t, "unsupported", result.Template.ModelProvisioning)
			require.Equal(t, "unresolvable", result.Requirements[0].Status)
			require.Equal(t, "invalid_model_category", result.Requirements[0].Reason)
		}
	}
	response := h.request("POST", "studio/models/resolve", `{"node":"worker","template":"unknown","download":true}`, "")
	require.Equal(t, 400, response.Code, response.Body.String())
	var problem struct {
		Code              string `json:"code"`
		ModelProvisioning string `json:"model_provisioning"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	require.Equal(t, string(httperr.CodeWorkflowModelMetadataMissing), problem.Code)
	require.Equal(t, "unverified", problem.ModelProvisioning)
	require.Empty(t, h.worker.jobs.List(jobs.KindDownload), "unknown requirements never admit guesses")
}

func TestMCPAPITemplateCatalogAndResolution(t *testing.T) {
	h := newProviderModelHarness(t)
	declareTemplateCategories(t, h.worker)
	raw, err := os.ReadFile("testdata/comfyui/image_qwen_image_2_1_turbo_t2i.json")
	require.NoError(t, err)
	items, err := workflowRequirements(raw)
	require.NoError(t, err)
	for _, item := range items {
		writeWorkflowFile(t, providerModelRoot(t, h.worker), filepath.Join(item.Category, item.Name), "already installed")
	}
	installTemplateSource(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "index.json") {
			_, _ = io.WriteString(w, `[{"templates":[{"name":"turbo","title":"Qwen Turbo"}]}]`)
			return
		}
		_, _ = w.Write(raw)
	}))
	server := templateMCPServer(h)
	catalog := callMCPWireTool(t, server, "zz_api_get", map[string]any{"path": "/zzrouter/v1/providers/studio/templates", "query": map[string]string{"node": "worker", "search": "Turbo"}})
	require.NotEqual(t, true, catalog["isError"], catalog)
	response := catalog["structuredContent"].(map[string]any)["response"].(map[string]any)
	require.Len(t, response["data"], 1)
	result := callMCPWireTool(t, server, "zz_api", map[string]any{"method": "POST", "path": "/zzrouter/v1/providers/studio/models/resolve", "body": map[string]any{"node": "worker", "template": "Qwen Turbo", "download": true}})
	require.NotEqual(t, true, result["isError"], result)
	response = result["structuredContent"].(map[string]any)["response"].(map[string]any)["data"].(map[string]any)
	require.Equal(t, "turbo", response["template"].(map[string]any)["id"])
	for _, item := range response["requirements"].([]any) {
		require.Equal(t, "present", item.(map[string]any)["status"])
	}
}
