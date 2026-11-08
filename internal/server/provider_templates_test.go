package server

import (
	"context"
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
	installTemplateSource(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "index.json") {
			catalogReads.Add(1)
			_, _ = io.WriteString(w, `[{"templates":[{"name":"turbo","title":"Qwen Turbo"},{"name":"edit","title":"Edit"}]}]`)
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
	require.Len(t, first.Template.SHA256, 64)
	replay := readMCPTemplateResolution(t, server)
	require.Equal(t, first.Requirements[0].JobID, replay.Requirements[0].JobID)
	require.EqualValues(t, 2, catalogReads.Load(), "accepted replay does not refetch upstream")
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
	before := reads.Load()
	for _, fields := range []string{``, `,"template":null`, `,"template":""`, `,"template":"  "`, `,"template":{}`, `,"workflow":null`, `,"workflow":{},"template":"a"`, `,"workflow":{},"template":null`} {
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
