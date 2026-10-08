package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

// replicaWireClient publishes synthetic worker inventories through the actual catalog refresh.
type replicaWireClient struct {
	mesh.ClusterClient
	models      []map[string]any
	calls       int
	failure     bool
	unscoped    bool
	unavailable []cache.InventoryUnavailable
}

func (c *replicaWireClient) Broadcast(context.Context, string, *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	c.calls++
	if c.failure && c.unscoped {
		return nil, errors.New("broadcast unavailable")
	}
	if c.failure {
		return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: "worker", Error: errors.New("worker unavailable")}}}, errors.New("partial broadcast")
	}
	body, _ := json.Marshal(map[string]any{"models": c.models, "inventory_unavailable": c.unavailable})
	return &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{NodeName: "worker", Response: &mesh.Response{StatusCode: 200, Body: body}}}}, nil
}
func routingCatalog(t *testing.T, s *Server, models []map[string]any) *replicaWireClient {
	t.Helper()
	client := &replicaWireClient{models: models}
	s.model.Cache = cache.New(cache.Config{
		Node: s.node, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: func() *config.AppsConfig { return s.appsConfig },
		ClusterClient: func() mesh.ClusterClient { return client }, ResolveEndpointToNodename: func(string) string { return "" },
	})
	return client
}
func replicaItem(node, provider string) map[string]any {
	return map[string]any{"name": "qwen", "node": node, "assigned_app": provider}
}

func TestReplicaRoutingInvalidatedWorkerChat(t *testing.T) {
	coordinator := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord"})
	worker := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker, NodeName: "worker", SeedProvidersDir: true})
	require.NoError(t, worker.configStore.SetProviderEnabled("llamacpp", true))
	client := routingCatalog(t, coordinator, []map[string]any{replicaItem("worker", "llamacpp")})
	routingCatalog(t, worker, []map[string]any{replicaItem("worker", "llamacpp")})
	inst := instance.NewInstance("worker-run", "llamacpp", "qwen", 8100, 0, 0)
	inst.WireModel = "qwen"
	inst.MarkRunning()
	require.NoError(t, worker.providers.appMgr.Instances().Register(inst))
	t.Cleanup(func() { _ = worker.providers.appMgr.Instances().Remove(inst.ID) })
	var engineCalls int
	previousTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	http.DefaultTransport = mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		engineCalls++
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "qwen", body["model"])
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"qwen","choices":[{"message":{"role":"assistant","content":"worker reply"}}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`))}, nil
	})
	compat := buildWorkerCompatEngine(worker)
	require.NoError(t, coordinator.cluster.coordinator.RegisterEndpoint(&mesh.Endpoint{Name: "worker", URL: "http://worker.test:9090", ClusterURL: "https://worker.test:9091"}))
	var hops int
	coordinator.clusterMTLSClient = &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		hops++
		require.Equal(t, "worker.test:9091", r.URL.Host)
		require.Equal(t, "llamacpp", r.Header.Get(constants.HeaderServingProvider))
		w := httptest.NewRecorder()
		compat.ServeHTTP(w, r)
		return w.Result(), nil
	})}
	for _, invalidated := range []bool{false, true} {
		if invalidated {
			coordinator.model.Cache.Invalidate()
		}
		result := callMCPWireTool(t, coordinator, "zz_chat", map[string]any{"model": "qwen", "message": "hi"})
		require.NotEqual(t, true, result["isError"], result)
		require.Equal(t, "worker reply", result["structuredContent"].(map[string]any)["reply"])
	}
	require.Equal(t, 2, hops)
	require.Equal(t, 2, engineCalls)
	require.Equal(t, 2, client.calls)
	require.Empty(t, coordinator.providers.appMgr.Instances().List())
	require.Equal(t, instance.StatusRunning, inst.GetStatus(), "the serving run stays on the worker")
}

func TestReplicaRoutingSelectionAndSurfaceErrors(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord", SeedProvidersDir: true})
	require.NoError(t, s.configStore.SetProviderEnabled("ollama", true))
	for _, tc := range []struct {
		name                string
		models              []map[string]any
		ref, node, provider string
		ambiguous           bool
	}{
		{name: "coordinator wins", models: []map[string]any{replicaItem("worker", "llamacpp"), replicaItem("coord", "llamacpp")}, ref: "qwen", provider: "llamacpp"},
		{name: "two workers", models: []map[string]any{replicaItem("two", "llamacpp"), replicaItem("one", "vllm")}, ref: "qwen", ambiguous: true},
		{name: "provider narrows", models: []map[string]any{replicaItem("two", "llamacpp"), replicaItem("one", "vllm")}, ref: "LLAMACPP/QWEN", node: "two", provider: "llamacpp"},
		{name: "case", models: []map[string]any{replicaItem("worker", "llamacpp")}, ref: "QwEn", node: "worker", provider: "llamacpp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routingCatalog(t, s, tc.models)
			resolved, err := s.ResolveModel(t.Context(), tc.ref, "")
			if tc.ambiguous {
				var wire httperr.Error
				require.ErrorAs(t, err, &wire)
				require.Equal(t, 409, wire.Status)
				require.Equal(t, "model_node_ambiguous", wire.Code)
				require.Contains(t, wire.Message, "qwen@one, qwen@two")
				for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/messages", "/v1/responses", "/api/chat", "/api/generate", "/api/embed"} {
					body := map[string]any{"model": "qwen", "messages": []map[string]string{{"role": "user", "content": "hi"}}, "prompt": "hi", "input": "hi", "max_tokens": 4, "stream": false}
					response := makeAuthRequest(t, s, http.MethodPost, path, TestAdminKey, body)
					require.Equal(t, 409, response.Code, path+": "+string(response.Body))
					require.Contains(t, string(response.Body), "qwen@one, qwen@two")
					if !strings.HasPrefix(path, "/api/") && path != "/v1/messages" {
						require.Contains(t, string(response.Body), "model_node_ambiguous")
					}
				}
				response := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/models/instance/qwen", TestAdminKey, nil)
				require.Equal(t, 409, response.Code)
				require.Contains(t, string(response.Body), "model_node_ambiguous")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.node, resolved.Node)
			require.Equal(t, tc.provider, resolved.Provider)
			require.Equal(t, "qwen", resolved.ModelName)
		})
	}
}

func TestModelInstanceUsesWorkerRunInventory(t *testing.T) {
	coordinator := createTestNodeWithDefaults(t)
	worker := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker, NodeName: "worker"})
	routingCatalog(t, coordinator, []map[string]any{replicaItem("worker", "llamacpp")})
	inst := instance.NewInstance("worker-run", "llamacpp", "qwen", 8100, 0, 0)
	inst.MarkRunning()
	require.NoError(t, worker.providers.appMgr.Instances().Register(inst))
	t.Cleanup(func() { _ = worker.providers.appMgr.Instances().Remove(inst.ID) })
	router := &previewWorkerRouter{worker: buildInternalEngine(worker)}
	coordinator.services.Model.router = router
	response := makeAuthRequest(t, coordinator, http.MethodGet, "/zzrouter/v1/models/instance/qwen", TestAdminKey, nil)
	require.Equal(t, 200, response.Code, string(response.Body))
	require.False(t, router.deadline.IsZero())
	require.Positive(t, time.Until(router.deadline))
	require.LessOrEqual(t, time.Until(router.deadline), constants.ClusterQueryTimeout)
	require.Contains(t, string(response.Body), "worker-run")
	require.Contains(t, string(response.Body), "8100")
	inst.SetStatus(instance.StatusStopped)
	response = makeAuthRequest(t, coordinator, http.MethodGet, "/zzrouter/v1/models/instance/qwen", TestAdminKey, nil)
	require.Equal(t, 404, response.Code)
	require.Contains(t, string(response.Body), "GET /zzrouter/v1/runs")
}

func TestReplicaRoutingPrefixedCloudRequiresAuthentication(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord", SeedProvidersDir: true})
	require.NoError(t, s.appsConfig.AddApp("paid", config.ServiceConfig{Enabled: new(true), Mode: "cloud", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: "https://paid.test", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-token"}}, Capabilities: &config.AppCapabilities{Models: []string{"cloud-model"}, WireEndpoints: []string{"chat_completions"}}}))
	client := routingCatalog(t, s, nil)
	for _, name := range []string{"cloud-model", "paid/cloud-model"} {
		resolved, err := s.ResolveModel(t.Context(), name, "")
		require.NoError(t, err)
		require.Empty(t, resolved.Node)
		require.Equal(t, "paid", resolved.Provider)
		response := makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", "", map[string]any{"model": name, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
		require.Equal(t, http.StatusUnauthorized, response.Code, string(response.Body))
	}
	require.Equal(t, 1, client.calls)
}

func TestReplicaRoutingPrefixedSharedVariantKeepsDefaults(t *testing.T) {
	s := variantNode(t)
	defineVariants(t, s)
	inst := instance.NewInstance("base-run", "llamacpp", qwenModel, 8100, 0, 0)
	inst.WireModel = qwenModel
	inst.MarkRunning()
	require.NoError(t, s.providers.appMgr.Instances().Register(inst))
	t.Cleanup(func() { _ = s.providers.appMgr.Instances().Remove(inst.ID) })
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	var sent map[string]any
	http.DefaultTransport = mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llamacpp/`+variantChat+`","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("X-API-Key", TestAdminKey)
	request.Header.Set("Content-Type", "application/json")
	response := &replicaRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.engine.ServeHTTP(response, request)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Equal(t, 0.7, sent["temperature"])
	require.Equal(t, qwenModel, sent["model"])
	require.Len(t, s.providers.appMgr.Instances().List(), 1)
}

// replicaRecorder supplies ReverseProxy's legacy cancellation interface for in-process tests.
type replicaRecorder struct{ *httptest.ResponseRecorder }

func (*replicaRecorder) CloseNotify() <-chan bool { return make(chan bool) }

func TestReplicaRoutingUnknownNameWithWorkerDown(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	client := routingCatalog(t, s, []map[string]any{replicaItem("worker", "llamacpp")})
	_, err := s.ResolveModel(t.Context(), "qwen", "")
	require.NoError(t, err)
	client.failure = true
	s.model.Cache.Invalidate()
	require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
	start := time.Now()
	for _, path := range []string{"/v1/chat/completions", "/zzrouter/v1/models/instance/unknown"} {
		method, body := http.MethodPost, any(map[string]any{"model": "unknown", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
		if strings.Contains(path, "/instance/") {
			method, body = http.MethodGet, nil
		}
		response := makeAuthRequest(t, s, method, path, TestAdminKey, body)
		require.Equal(t, http.StatusNotFound, response.Code, string(response.Body))
	}
	require.Less(t, time.Since(start), time.Second, "negative discovery must not block for ClusterQueryTimeout")
	require.Equal(t, 2, client.calls)
	_, err = s.ResolveModel(t.Context(), "qwen", "")
	var problem httperr.Error
	require.ErrorAs(t, err, &problem)
	require.Equal(t, http.StatusServiceUnavailable, problem.Status)
	require.Equal(t, string(httperr.CodeClusterUnavailable), problem.Code)
	require.Equal(t, 2, client.calls)
}

func TestReplicaRoutingDecoratedCloudGroupRequiresAuthentication(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord", SeedProvidersDir: true})
	require.NoError(t, s.appsConfig.AddApp("paid", config.ServiceConfig{Enabled: new(true), Mode: "cloud", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: "https://paid.test", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-token"}}, Capabilities: &config.AppCapabilities{Models: []string{"cloud-model"}, WireEndpoints: []string{"chat_completions"}}}))
	require.NoError(t, s.model.Groups.Set("cloud-group", modelgroup.ModelGroup{Replicas: []modelgroup.Replica{{Name: "paid", App: "paid", Model: "cloud-model"}}}))
	for _, model := range []string{"cloud-group", "cloud-group@worker"} {
		response := makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", "", map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
		require.Equal(t, http.StatusUnauthorized, response.Code, string(response.Body))
	}
}

func TestReplicaRoutingKnownStoppedProviderKeepsRecoveryProblem(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	client := routingCatalog(t, s, []map[string]any{replicaItem("worker", "generic-daemon")})
	client.unavailable = []cache.InventoryUnavailable{stoppedInventory("worker")}
	response := makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", TestAdminKey, map[string]any{"model": "qwen", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	require.Equal(t, http.StatusServiceUnavailable, response.Code, string(response.Body))
	require.Contains(t, string(response.Body), "provider_not_running")
	require.Contains(t, string(response.Body), "/service/start?node=worker")
	require.NotContains(t, string(response.Body), "cluster_unavailable")
}

func TestModelInstanceProviderMatchIsShared(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "worker"}[remote], func(t *testing.T) {
			coordinator := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord", SeedProvidersDir: true})
			target := coordinator
			if remote {
				target = createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker, NodeName: "worker"})
				coordinator.services.Model.router = &previewWorkerRouter{worker: buildInternalEngine(target)}
			}
			inst := instance.NewInstance("actual-run", "vllm", "qwen", 8100, 0, 0)
			inst.MarkRunning()
			require.NoError(t, target.providers.appMgr.Instances().Register(inst))
			t.Cleanup(func() { _ = target.providers.appMgr.Instances().Remove(inst.ID) })
			for _, tc := range []struct {
				provider string
				status   int
			}{{"llamacpp", 404}, {"VLLM", 200}} {
				routingCatalog(t, coordinator, []map[string]any{replicaItem(target.node.Nodename(), tc.provider)})
				response := makeAuthRequest(t, coordinator, http.MethodGet, "/zzrouter/v1/models/instance/"+strings.ToLower(tc.provider)+"/qwen", TestAdminKey, nil)
				require.Equal(t, tc.status, response.Code, string(response.Body))
				if tc.status == 200 {
					require.Contains(t, string(response.Body), "actual-run")
				} else {
					require.NotContains(t, string(response.Body), "actual-run")
				}
			}
		})
	}
}

// launchBroadcastRouter records ordinary routing, without starting a model process.
type launchBroadcastRouter struct{ fakeRouter }

func (r *launchBroadcastRouter) Broadcast(_ context.Context, path, method string, body []byte) (*routing.Response, error) {
	r.calls = append(r.calls, routing.Request{Path: path, Method: method, Body: body})
	response := `{"model":"unknown","provider":"llamacpp","instance_id":"fixture-run","status":"loading"}`
	return &routing.Response{StatusCode: http.StatusOK, Body: []byte(response)}, nil
}

func TestLoadServiceDegradedCatalogUsesInferenceRule(t *testing.T) {
	for _, unscoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "named node failure", true: "unscoped broadcast failure"}[unscoped], func(t *testing.T) {
			s := createTestNodeWithDefaults(t)
			client := routingCatalog(t, s, []map[string]any{replicaItem("worker", "llamacpp")})
			require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
			client.failure, client.unscoped = true, unscoped
			s.model.Cache.Invalidate()
			require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
			router := &launchBroadcastRouter{}
			s.cluster.router = router
			svc := NewLoadService(s, router).WithTierResolution(func() *config.AppsConfig { return s.appsConfig })
			for _, name := range []string{"unknown", "qwen"} {
				wantError := name == "qwen" && !unscoped
				resolved, inferenceErr := s.ResolveModel(t.Context(), name, "")
				target, targetErr := svc.launchTarget(t.Context(), "", name)
				req := &LoadModelRequest{ModelName: name, Provider: "llamacpp"}
				sources, mergeErr := svc.preMerge(t.Context(), req)
				_, previewErr := svc.PreviewRun(t.Context(), req)
				_, loadErr := svc.LoadModel(t.Context(), req)
				for _, err := range []error{inferenceErr, targetErr, mergeErr, previewErr, loadErr} {
					if wantError {
						response := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(response)
						c.Request = httptest.NewRequest(http.MethodPost, "/zzrouter/v1/runs/preview", nil)
						RespondToError(c, err)
						require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
						require.Contains(t, response.Body.String(), string(httperr.CodeClusterUnavailable))
					} else {
						require.NoError(t, err)
					}
				}
				if !wantError {
					require.Empty(t, resolved.Node)
					require.Empty(t, target)
					require.Nil(t, sources)
					require.Empty(t, req.Parameters)
				}
			}
			expected := 2
			if unscoped {
				expected = 4
			}
			require.Len(t, router.calls, expected, "only ordinary unknown-name routing reaches broadcast")
			require.Equal(t, 2, client.calls, "preview/load must not retry catalog failure or invent a new rule")
		})
	}
}
