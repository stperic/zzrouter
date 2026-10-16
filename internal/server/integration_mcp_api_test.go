package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/internal/server/mcpserver"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

func callMCPWireTool(t *testing.T, s *Server, name string, args map[string]any) map[string]any {
	t.Helper()
	return mcpWireResponse(t, s, mcpWireRequest(t, "tools/call", "2026-07-28", map[string]any{"name": name, "arguments": args}), http.StatusOK)["result"].(map[string]any)
}

func TestMCPAPICatalogEligibility(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, tc := range []struct {
		method, path, operationID string
		allowed                   bool
	}{
		{"GET", "/zzrouter/v1/server/routes", "listServerRoutes", true},
		{"GET", "/zzrouter/v1/server/logs", "listServerLogs", true},
		{"GET", "/zzrouter/v1/providers/llamacpp/status", "getProviderStatus", true},
		{"PATCH", "/zzrouter/v1/providers/llamacpp/parameters", "patchProviderParameters", true},
		{"GET", "/zzrouter/v1/jobs/run_1", "getJob", true},
		{"PUT", "/zzrouter/v1/providers/llamacpp/parameters", "", false},
		{"GET", "/zzrouter/v1/runs/id/logs", "", false},
		{"GET", "/zzrouter/v1/jobs/id/stream", "", false},
		{"GET", "/zzrouter/v1/model-groups/events", "", false},
		{"GET", "/zzrouter/v1/spend/events", "", false},
		{"GET", "/zzrouter/v1/internal/version", "", false},
		{"POST", "/zzrouter/v1/mcp", "", false},
		{"GET", "/health", "", false},
		{"POST", "/zzrouter/v1/server/routes", "", false},
		{"GET", "/zzrouter/v1/nonexistent", "", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			operationID, err := s.eligibleAPIRoute(tc.method, tc.path)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, tc.operationID, operationID)
		})
	}
	// A static streaming route must outrank a parameter route with the same shape.
	engine := gin.New()
	engine.GET("/zzrouter/v1/example/:id", func(*gin.Context) {})
	engine.GET("/zzrouter/v1/example/stream", func(*gin.Context) {})
	engine.GET("/zzrouter/v1/files/*path", func(*gin.Context) {})
	engine.GET("/zzrouter/v1/internal", func(*gin.Context) {})
	engine.POST("/zzrouter/v1/mcp/child", func(*gin.Context) {})
	server := &Server{engine: engine}
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{"GET", "/zzrouter/v1/internal", false},
		{"POST", "/zzrouter/v1/mcp/child", false},
		{"GET", "/zzrouter/v1/example/ordinary", true},
		{"GET", "/zzrouter/v1/example/stream", false},
		{"GET", "/zzrouter/v1/files/nested/file", true},
		{"GET", "/zzrouter/v1/example/ordinary/extra", false},
		{"GET", "/zzrouter/v1/example/", false},
	} {
		// Routes the spec does not describe stay eligible with no operationId.
		operationID, err := server.eligibleAPIRoute(tc.method, tc.path)
		require.Equal(t, tc.allowed, err == nil, "%s %s: %v", tc.method, tc.path, err)
		require.Empty(t, operationID)
	}
}

func TestMCPAPIGetOutsideCuratedTools(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	result := callMCPWireTool(t, s, "zz_api_get", map[string]any{"path": "/zzrouter/v1/server/routes"})
	require.NotEqual(t, true, result["isError"], result)
	require.NotContains(t, result["structuredContent"], "task_tools")
	response := result["structuredContent"].(map[string]any)["response"].(map[string]any)
	require.Positive(t, response["count"])
	require.NotEmpty(t, response["routes"])
	// The real catalog names the operation and the registered bindings name its tools.
	covered := callMCPWireTool(t, s, "zz_api_get", map[string]any{"path": "/zzrouter/v1/nodes"})
	require.NotEqual(t, true, covered["isError"], covered)
	require.Equal(t, []any{"zz_nodes", "zz_overview"}, covered["structuredContent"].(map[string]any)["task_tools"])
	require.Contains(t, covered["content"].([]any)[0].(map[string]any)["text"], "zz_nodes or zz_overview serves this route")
	for _, args := range []map[string]any{
		{"path": "/zzrouter/v1/internal"},
		{"path": "/zzrouter/v1/internal/version"},
		{"path": "/zzrouter/v1/mcp", "method": "POST"},
		{"path": "/zzrouter/v1/mcp/child"},
		{"path": "/zzrouter/v1/jobs/id/stream"},
		{"path": "/zzrouter/v1/model-groups/events"},
		{"path": "/zzrouter/v1/providers/llamacpp/parameters", "method": "PUT"},
	} {
		name := "zz_api_get"
		if _, ok := args["method"]; ok {
			name = "zz_api"
		}
		require.Equal(t, true, callMCPWireTool(t, s, name, args)["isError"])
	}
}

func TestMCPAPIRealMergePatch(t *testing.T) {
	s := variantNode(t)
	for _, value := range []any{8192, nil} {
		result := callMCPWireTool(t, s, "zz_api", map[string]any{
			"method": "PATCH", "path": "/zzrouter/v1/providers/llamacpp/parameters", "content_type": "application/merge-patch+json",
			"body": map[string]any{"models": map[string]any{qwenModel: map[string]any{"parameters": map[string]any{"ctx-size": value}}}},
		})
		require.NotEqual(t, true, result["isError"], result)
		cfg, ok := s.configStore.Config().LookupApp("llamacpp")
		require.True(t, ok)
		if value == nil {
			require.NotContains(t, cfg.Models[qwenModel].Parameters, "ctx-size")
		} else {
			require.Contains(t, cfg.Models[qwenModel].Parameters, "ctx-size")
		}
	}
}

func TestMCPAPISearchServedSpec(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, tc := range []struct {
		query, id  string
		parameters []string
	}{
		{"GET getProviderResolvedParameters", "getProviderResolvedParameters", []string{"name", "node", "model"}},
		{"listRegistriesHuggingfaceVariants", "listRegistriesHuggingfaceVariants", []string{"id", "provider"}},
		{"getProvidersCatalog", "getProvidersCatalog", []string{"type", "mode", "kind"}},
	} {
		result := callMCPWireTool(t, s, "zz_api_search", map[string]any{"query": tc.query})
		require.NotEqual(t, true, result["isError"], result)
		operations := result["structuredContent"].(map[string]any)["operations"].([]any)
		require.Len(t, operations, 1)
		op := operations[0].(map[string]any)
		require.Equal(t, tc.id, op["operationId"])
		require.Equal(t, "GET", op["method"])
		require.NotEmpty(t, op["summary"])
		var names []string
		for _, value := range op["parameters"].([]any) {
			parameter := value.(map[string]any)
			names = append(names, parameter["name"].(string))
			require.NotEmpty(t, parameter["in"])
			_, ok := parameter["required"].(bool)
			require.True(t, ok)
			if parameter["name"] == "id" || parameter["name"] == "name" {
				require.Equal(t, true, parameter["required"])
			}
		}
		require.ElementsMatch(t, tc.parameters, names)
	}
	jobs := callMCPWireTool(t, s, "zz_api_search", map[string]any{"query": "GET getJob latest state"})["structuredContent"].(map[string]any)["operations"].([]any)
	require.Len(t, jobs, 1)
	require.Equal(t, []any{"zz_job"}, jobs[0].(map[string]any)["task_tools"])
	first := callMCPWireTool(t, s, "zz_api_search", map[string]any{"query": ""})["structuredContent"]
	require.Len(t, first.(map[string]any)["operations"], mcpserver.DefaultAPISearchLimit)
	require.Equal(t, first, callMCPWireTool(t, s, "zz_api_search", map[string]any{"query": ""})["structuredContent"])
	for _, limit := range []int{-1, mcpserver.MaxListLimit + 1} {
		require.Equal(t, true, callMCPWireTool(t, s, "zz_api_search", map[string]any{"query": "", "limit": limit})["isError"])
	}
	// Local references and shared path parameters come from the served spec, not a tool index.
	ops, err := searchOpenAPI("listNodes", 1)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	require.NotEmpty(t, ops[0]["parameters"])
}

func TestMCPProvidersFreshInventory(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: cfg})
	result := callMCPWireTool(t, s, "zz_providers", map[string]any{})
	require.NotEqual(t, true, result["isError"], result)
	providers := result["structuredContent"].(map[string]any)["providers"].([]any)
	require.NotEmpty(t, providers)
	found := false
	for _, value := range providers {
		provider := value.(map[string]any)
		require.NotEqual(t, "huggingface", provider["name"])
		require.NotEqual(t, "registry", provider["kind"])
		require.Equal(t, false, provider["enabled"], provider)
		if provider["name"] == "llamacpp" {
			found = true
			require.Equal(t, "on-demand", provider["kind"])
			require.Equal(t, "test-host", provider["node"])
			require.Equal(t, false, provider["managed"])
			require.Equal(t, false, provider["installed"])
		}
	}
	require.True(t, found)
	local := callMCPWireTool(t, s, "zz_providers", map[string]any{"node": "test-host"})
	require.Equal(t, result["structuredContent"], local["structuredContent"])
	// The node/overview projection still reports only enabled inference providers.
	require.Empty(t, s.GetLocalNodeInfo()["providers"])
	require.Empty(t, (&NodesService{}).endpointToNodeInfo(&mesh.Endpoint{Snapshot: s.buildSelfSnapshot()})["providers"])
}

func TestMCPPreviewProviderErrorsBelongToREST(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: cfg})
	for _, tc := range []struct{ provider, version, detail, next, absent string }{
		{"missing-provider", "", "not configured on this node", "GET /zzrouter/v1/providers", "enabled=true"},
		{"llamacpp", "", "configured but disabled on this node", "POST /zzrouter/v1/providers/llamacpp/install", "PATCH"},
		{"LLAMACPP", "1.2.3", "configured but disabled on this node", "PATCH /zzrouter/v1/providers/llamacpp with enabled=true", "POST"},
	} {
		s.providers.appMgr.SetProviderVersion("llamacpp", tc.version)
		resp := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/runs/preview", TestAdminKey, map[string]string{"provider": tc.provider, "model_name": "model"})
		require.Equal(t, http.StatusBadRequest, resp.Code, string(resp.Body))
		var problem map[string]any
		require.NoError(t, json.Unmarshal(resp.Body, &problem))
		require.Equal(t, "bad_request", problem["code"])
		require.Contains(t, problem["detail"], tc.detail)
		require.Contains(t, problem["detail"], tc.next)
		require.NotContains(t, problem["detail"], tc.absent)
		_, err := s.newLoadExecutor().PreviewLocalRun(t.Context(), &PreviewRunRequest{Provider: tc.provider, ModelName: "model"})
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, answeredStatus(err))
		require.Equal(t, problem["detail"], err.Error())
		// The worker handler also forwards the same code, rather than wrapping it as 500.
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/runs/preview", strings.NewReader(`{"provider":"`+tc.provider+`","model_name":"model"}`))
		s.newLoadExecutor().HandleInternalPreviewRun(c)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), `"code":"bad_request"`)
		result := callMCPWireTool(t, s, "zz_run_preview", map[string]any{"provider": tc.provider, "model": "model"})
		require.Equal(t, true, result["isError"])
		require.Contains(t, result["content"].([]any)[0].(map[string]any)["text"], problem["detail"])
	}
}

func TestMCPAPINoContentSuccess(t *testing.T) {
	s, dir := assetTestNode(t)
	result := callMCPWireTool(t, s, "zz_api", map[string]any{"method": "DELETE", "path": "/zzrouter/v1/providers/llamacpp/assets/t.jinja"})
	require.NotEqual(t, true, result["isError"], result)
	require.Equal(t, map[string]any{"response": nil}, result["structuredContent"])
	_, err := dir.Path("t.jinja")
	require.ErrorIs(t, err, assets.ErrNotFound)
	// The next call returns the ordinary REST Problem after the first call succeeded.
	result = callMCPWireTool(t, s, "zz_api", map[string]any{"method": "DELETE", "path": "/zzrouter/v1/providers/llamacpp/assets/t.jinja"})
	require.Equal(t, true, result["isError"])
	require.Contains(t, result["content"].([]any)[0].(map[string]any)["text"], "HTTP 404")
}

// previewWorkerRouter exercises the real worker executor without opening a listener.
type previewWorkerRouter struct {
	fakeRouter
	worker   http.Handler
	deadline time.Time
}

func (r *previewWorkerRouter) Unicast(ctx context.Context, node, path, method string, body []byte) (*routing.Response, error) {
	r.deadline, _ = ctx.Deadline()
	rec := httptest.NewRecorder()
	r.worker.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(body)))
	return &routing.Response{StatusCode: rec.Code, Body: rec.Body.Bytes(), Headers: rec.Header(), Node: node}, nil
}

func TestMCPPreviewRecoveryUsesTargetInventory(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	coordinator := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: cfg})
	cfg.ClusterMode = config.ClusterModeWorker
	cfg.NodeName = "preview-worker"
	worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: cfg})
	coordinator.services.Load.router = &previewWorkerRouter{worker: buildInternalEngine(worker)}
	for _, tc := range []struct{ localVersion, workerVersion, next, absent string }{
		{"1.2.3", "", "POST /zzrouter/v1/providers/llamacpp/install", "PATCH"},
		{"", "1.2.3", "PATCH /zzrouter/v1/providers/llamacpp with enabled=true", "POST"},
	} {
		coordinator.providers.appMgr.SetProviderVersion("llamacpp", tc.localVersion)
		worker.providers.appMgr.SetProviderVersion("llamacpp", tc.workerVersion)
		result := callMCPWireTool(t, coordinator, "zz_run_preview", map[string]any{"model": "model", "provider": "llamacpp", "node": "preview-worker"})
		require.Equal(t, true, result["isError"])
		text := result["content"].([]any)[0].(map[string]any)["text"]
		require.Contains(t, text, "HTTP 400 bad_request")
		require.Contains(t, text, tc.next)
		require.NotContains(t, text, tc.absent)
	}
}
