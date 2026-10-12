package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stperic/zzrouter/internal/server/mcpserver"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/observability"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type mcpRoundTripper func(*http.Request) (*http.Response, error)

func (f mcpRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func exerciseMCPClient(t *testing.T, endpoint, protocol string, client *http.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sdk := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	session, err := sdk.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
	require.NoError(t, err)
	defer session.Close()
	require.Equal(t, protocol, session.InitializeResult().ProtocolVersion)
	require.Less(t, len(session.InitializeResult().Instructions), 2<<10)
	require.Contains(t, session.InitializeResult().Instructions, "zz_api_search")
	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, tools.Tools, 26)
	if protocol == "2026-07-28" {
		require.Equal(t, int((5 * time.Minute).Milliseconds()), tools.TTLMs)
	}
	names := []string{}
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		require.NotNil(t, tool.Annotations)
	}
	require.Contains(t, names, "zz_overview")
	require.Contains(t, names, "zz_job")
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "zz_overview", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result.Content)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var overview struct {
		Health        string                                `json:"health"`
		Nodes         []struct{ Name, Role, Health string } `json:"nodes"`
		EndpointCount int                                   `json:"endpoint_count"`
		OpenAPI       string                                `json:"openapi"`
	}
	require.NoError(t, json.Unmarshal(data, &overview))
	require.Equal(t, "healthy", overview.Health)
	require.NotEmpty(t, overview.Nodes)
	require.Equal(t, "test-host", overview.Nodes[0].Name)
	require.NotEmpty(t, overview.Nodes[0].Role)
	require.Equal(t, "healthy", overview.Nodes[0].Health)
	require.Positive(t, overview.EndpointCount)
	require.Equal(t, "/openapi.yaml", overview.OpenAPI)
	require.NotContains(t, string(data), "uptime_seconds")
	missing, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "zz_job", Arguments: map[string]any{"job_id": "missing-job"}})
	require.NoError(t, err)
	require.True(t, missing.IsError)
	require.Contains(t, missing.Content[0].(*mcp.TextContent).Text, "HTTP 404")
}

func TestIntegrationMCPClientHTTP(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, protocol := range []string{"2026-07-28"} {
		t.Run(protocol, func(t *testing.T) {
			ts := httptest.NewServer(s.engine)
			defer ts.Close()
			client := ts.Client()
			base := client.Transport
			client.Transport = mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
				r.Header.Set("Authorization", "Bearer "+TestAdminKey)
				return base.RoundTrip(r)
			})
			exerciseMCPClient(t, ts.URL+"/zzrouter/v1/mcp", protocol, client)
		})
	}
}

// Recorder transport exercises SDK HTTP semantics without a TCP listener.
func TestIntegrationMCPClientRecorder(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	for _, protocol := range []string{"2026-07-28"} {
		t.Run(protocol, func(t *testing.T) {
			client := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
				r.Header.Set("X-API-Key", TestAdminKey)
				r.RemoteAddr = "198.51.100.9:55121"
				w := httptest.NewRecorder()
				s.engine.ServeHTTP(w, r)
				return w.Result(), nil
			})}
			exerciseMCPClient(t, "http://coordinator/zzrouter/v1/mcp", protocol, client)
		})
	}
}

func TestIntegrationMCPAuthAndTransport(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey})
	for _, key := range []string{"", TestUserKey} {
		w := makeRequest(t, s, TestRequest{Method: "POST", Path: "/zzrouter/v1/mcp", Headers: map[string]string{"X-API-Key": key}})
		if key == "" {
			require.Equal(t, http.StatusUnauthorized, w.Code)
		} else {
			require.Equal(t, http.StatusForbidden, w.Code)
		}
		rest := makeRequest(t, s, TestRequest{Method: "GET", Path: "/zzrouter/v1/nodes", Headers: map[string]string{"X-API-Key": key}})
		require.Equal(t, rest.Code, w.Code)
	}
	for _, method := range []string{"GET", "DELETE"} {
		w := makeAuthRequest(t, s, method, "/zzrouter/v1/mcp", TestAdminKey, nil)
		require.Equal(t, http.StatusMethodNotAllowed, w.Code)
	}
	r := httptest.NewRequest("POST", "/zzrouter/v1/mcp", strings.NewReader(strings.Repeat("x", mcpserver.MaxRequestBytes+1)))
	r.Header.Set("X-API-Key", TestAdminKey)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	r = httptest.NewRequest("POST", "http://coordinator/zzrouter/v1/mcp", strings.NewReader(`{}`))
	r.Header.Set("X-API-Key", TestAdminKey)
	r.Header.Set("Origin", "https://untrusted.example")
	w = httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestIntegrationMCPInternalMarkerRefused(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	r := httptest.NewRequest("POST", "/zzrouter/v1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	r.Header.Set("X-API-Key", TestAdminKey)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r = r.WithContext(context.WithValue(r.Context(), internalRequestKey, true))
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "application/problem+json")
	var p utils.ProblemDetails
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
	require.Equal(t, "forbidden", p.Code)
}

func TestIntegrationMCPWorkerNotMounted(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: pkgConfig.ClusterModeWorker})
	for _, route := range s.engine.Routes() {
		require.NotEqual(t, "/zzrouter/v1/mcp", route.Path)
	}
	w := makeAuthRequest(t, s, "POST", "/zzrouter/v1/mcp", TestAdminKey, nil)
	require.NotEqual(t, http.StatusOK, w.Code)
}

func TestMCPOperationBindings(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	operations := mcpBoundOperations(t)
	bindings := mcpserver.OperationBindings()
	require.Equal(t, []string{"listJobs", "getJob"}, bindings["zz_job"])
	require.Equal(t, []string{"listModels", "showModels"}, bindings["zz_models"])
	require.Equal(t, []string{"deleteModels"}, bindings["zz_model_delete"])
	require.Equal(t, []string{"openaiCreateChatCompletion", "openaiCreateEmbedding"}, bindings["zz_chat"])
	require.Equal(t, []string{"listRegistriesHuggingfaceVariants"}, bindings["zz_model_files"])
	require.Equal(t, []string{"listProviders", "getProviderStatus"}, bindings["zz_providers"])
	for _, name := range []string{"zz_api_search", "zz_api_get", "zz_api"} {
		require.Contains(t, bindings, name)
		require.Empty(t, bindings[name], "escape hatches bind dynamically through the catalog/spec")
	}
	for tool, ids := range mcpserver.OperationBindings() {
		for _, id := range ids {
			route, ok := operations[id]
			require.True(t, ok, "%s binds missing operationId %s", tool, id)
			found := false
			for _, mounted := range s.engine.Routes() {
				if mounted.Method == route.method && normalizeGinPath(mounted.Path) == route.path {
					found = true
					catalog := RouteSpec{Retired: isRetiredRoute(mounted.Handler)}
					require.False(t, catalog.Retired, "%s binds retired operationId %s", tool, id)
				}
			}
			require.True(t, found, "%s binds unmounted operationId %s", tool, id)
		}
	}
	require.Equal(t, "mcp", surfaceForPath("/zzrouter/v1/mcp"))
	require.Equal(t, "admin", authTierForPath("/zzrouter/v1/mcp"))
	discovery := buildAPIDiscovery(s)
	require.Equal(t, "POST /zzrouter/v1/mcp", discovery.MCP)
}

func TestMCPAccountingOnce(t *testing.T) {
	// Metrics instruments initialize once, so isolate provider ownership from other tests.
	if os.Getenv("ZZROUTER_MCP_ACCOUNTING_CHILD") != "1" {
		binary, err := os.Executable()
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := host.CommandContext(ctx, binary, "-test.run=^TestMCPAccountingOnce$")
		cmd.Env = []string{"ZZROUTER_MCP_ACCOUNTING_CHILD=1"}
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		return
	}

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer func() { otel.SetMeterProvider(previous); require.NoError(t, provider.Shutdown(context.Background())) }()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var count atomic.Int32
	engine.Use(skipMCPAccounting(func(c *gin.Context) { count.Add(1); c.Next() }), skipMCPAccounting(observability.MetricsMiddleware()))
	engine.GET("/zzrouter/v1", func(c *gin.Context) { c.JSON(200, gin.H{"data": gin.H{"endpoint_groups": gin.H{}}}) })
	engine.GET("/zzrouter/v1/nodes", func(c *gin.Context) { c.JSON(200, gin.H{"data": []any{}}) })
	engine.GET("/zzrouter/v1/health", func(c *gin.Context) { c.JSON(200, gin.H{"data": gin.H{"cluster_health": "healthy"}}) })
	handler := mcpserver.New(mcpserver.Config{Engine: engine, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath})
	engine.POST("/zzrouter/v1/mcp", RequestIDMiddleware(), gin.WrapH(handler))
	r := mcpOverviewRequest(context.Background())
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "Cluster healthy")
	require.EqualValues(t, 1, count.Load())
	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))
	var requests int64
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == "zzrouter.http.requests.total" {
				for _, point := range metric.Data.(metricdata.Sum[int64]).DataPoints {
					requests += point.Value
				}
			}
		}
	}
	require.EqualValues(t, 1, requests, "HTTP metrics counted inner REST calls")
}

func TestIntegrationMCPJobResult(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	handle, err := s.jobs.Start(context.Background(), jobs.KindRun, "test-agent", jobs.Meta{"model": "small"})
	require.NoError(t, err)
	handle.Progress(50, "launching", jobs.Bytes{})
	handle.Meta(jobs.Meta{"run_id": "run-1"})
	handle.Done()
	client := &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("X-API-Key", TestAdminKey)
		r.RemoteAddr = "198.51.100.9:55121"
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		return w.Result(), nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://coordinator/zzrouter/v1/mcp", HTTPClient: client}, &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	require.NoError(t, err)
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "zz_job", Arguments: map[string]any{"job_id": handle.ID(), "wait_seconds": 45}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(data), `"state":"done"`)
	require.Contains(t, string(data), `"run_id":"run-1"`)
}

func mcpOverviewRequest(ctx context.Context) *http.Request {
	r := httptest.NewRequest("POST", "http://coordinator/zzrouter/v1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"zz_overview","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	r.Header.Set("Mcp-Method", "tools/call")
	r.Header.Set("Mcp-Name", "zz_overview")
	r.RemoteAddr = "198.51.100.9:55121"
	return r.WithContext(ctx)
}

func TestMCPDispatcherRealContextAndLoopbackGate(t *testing.T) {
	engine := gin.New()
	reached := false
	engine.GET("/zzrouter/v1", loopbackOrAdmin(func(c *gin.Context) {
		assert.Nil(t, c.Request.Context().Value(internalRequestKey), "trusted internal auth marker leaked")
		assert.Equal(t, "198.51.100.9:55121", c.Request.RemoteAddr)
		assert.Empty(t, c.GetHeader("X-API-Key"))
		assert.Empty(t, c.GetHeader("Authorization"))
		reached = true
		Unauthorized(c, "API key required")
		c.Abort()
	}), func(c *gin.Context) { c.JSON(200, gin.H{"data": gin.H{}}) })
	handler := mcpserver.New(mcpserver.Config{Engine: engine, KeyHeaders: callerKeyHeaders, Streaming: streamingForPath})
	r := mcpOverviewRequest(context.WithValue(context.Background(), internalRequestKey, true))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	require.True(t, reached, "remote caller bypassed loopbackOrAdmin")
	require.Contains(t, w.Body.String(), `"isError":true`)
	require.Contains(t, w.Body.String(), "HTTP 401")
}

func TestMCPDispatcherCatalogStreaming(t *testing.T) {
	called := false
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; <-r.Context().Done() })
	handler := mcpserver.New(mcpserver.Config{Engine: engine, KeyHeaders: callerKeyHeaders, Streaming: func(method, _ string) string {
		return streamingForPath(method, "/zzrouter/v1/jobs/job-1/stream")
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, mcpOverviewRequest(ctx))
	require.False(t, called)
	require.Contains(t, w.Body.String(), "streaming route")
}

func TestIntegrationMCPAccessLogOnce(t *testing.T) {
	previous := gin.DefaultWriter
	var logs bytes.Buffer
	gin.DefaultWriter = &logs
	defer func() { gin.DefaultWriter = previous }()
	s := createTestNodeWithDefaults(t)
	logs.Reset()
	r := mcpOverviewRequest(context.Background())
	r.Header.Set("X-API-Key", TestAdminKey)
	r.Header.Set("X-Request-ID", "agent-call")
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "Cluster healthy")
	require.Equal(t, 1, strings.Count(logs.String(), "[GIN]"), logs.String())
}

func TestMCPProblemToolError(t *testing.T) {
	handler := mcpserver.New(mcpserver.Config{KeyHeaders: callerKeyHeaders, Streaming: streamingForPath, Engine: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, err := w.Write([]byte(`{"status":400,"code":"bad_parameter","detail":"Fix provider parameters","errors":[{"key":"ctx","message":"too large"},{"key":"gpu","message":"unknown"}]}`))
		require.NoError(t, err)
	})})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, mcpOverviewRequest(context.Background()))
	require.Equal(t, http.StatusOK, w.Code)
	var response struct {
		Result mcp.CallToolResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.True(t, response.Result.IsError)
	require.Equal(t, "HTTP 400 bad_parameter: Fix provider parameters\nctx: too large\ngpu: unknown", response.Result.Content[0].(*mcp.TextContent).Text)
}
