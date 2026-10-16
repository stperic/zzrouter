package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

// Synthetic daemon replies exercise real registry, REST and MCP paths.
func detailDaemon(t *testing.T, s *Server, name string, templateBody ...string) *atomic.Int32 {
	t.Helper()
	mode := &atomic.Int32{}
	template := "{{ messages }}"
	if len(templateBody) > 0 {
		template = templateBody[0]
	}
	oversizedParameters := strings.Repeat("p", 1<<17)
	require.Greater(t, len(oversizedParameters), 64<<10, "overflow fixture must exceed the compact body limit")
	tags := []byte(`{"models":[{"name":"m","model":"m","size":123,"details":{"format":"gguf"}}]}`)
	ps := []byte(`{"models":[]}`)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			if mode.Load() == 5 {
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":"inventory unavailable"}`))
				return
			}
			_, _ = w.Write(tags)
		case "/api/ps":
			_, _ = w.Write(ps)
		case "/api/show":
			var request struct {
				Name    string `json:"name"`
				Verbose bool   `json:"verbose"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				w.WriteHeader(400)
				return
			}
			switch mode.Load() {
			case 1:
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":"synthetic authentication refusal"}`))
			case 2:
				_, _ = w.Write([]byte(`null`))
			case 3:
				_, _ = w.Write([]byte(`{} {}`))
			default:
				parameters := name
				if mode.Load() == 4 {
					parameters = oversizedParameters
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"parameters": parameters, "verbose": request.Verbose, "provider_model": request.Name, "template": template, "license": "discard in compact mode", "model_info": map[string]any{"general.architecture": "llama", "llama.context_length": 8192, "tensor_data": []any{1, 2}}, "capabilities": []string{"completion", "tools"}})
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(up.Close)
	response := makeAuthRequest(t, s, http.MethodPost, "/zzrouter/v1/providers/instances", TestAdminKey, map[string]any{"type": "ollama-connect", "name": name, "endpoint": up.URL})
	require.Contains(t, []int{200, 201}, response.Code, string(response.Body))
	return mode
}

func detailData(t *testing.T, response *TestResponse) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body, &body), string(response.Body))
	data, ok := body["data"].(map[string]any)
	require.True(t, ok, body)
	return data
}

func TestModelDetailBackendAndCompactContract(t *testing.T) {
	largeTemplate := strings.Repeat("template", 1<<18)
	require.Equal(t, 2<<20, len(largeTemplate), "preserve the original 2 MiB fixture")
	require.Greater(t, len(largeTemplate), 1<<20)
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: DefaultTestNodeConfig(), providers: []string{"ollama", "huggingface"}})
	detailDaemon(t, s, "ollama-detail-a", largeTemplate)
	detailDaemon(t, s, "ollama-detail-b", largeTemplate)
	listed := makeAuthRequest(t, s, "GET", "/zzrouter/v1/models?model=m", TestAdminKey, nil)
	require.Equal(t, 200, listed.Code, string(listed.Body))
	var collection struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(listed.Body, &collection))
	require.Equal(t, 2, collection.Total, "same names on different daemons must remain distinct")
	for _, provider := range []string{"ollama-detail-a", "ollama-detail-b"} {
		path := "/zzrouter/v1/models/show?model=m&provider=" + provider
		full := makeAuthRequest(t, s, "GET", path+"&verbose=true", TestAdminKey, nil)
		require.Equal(t, 200, full.Code, string(full.Body))
		fullDetails := detailData(t, full)
		require.Equal(t, provider, fullDetails["parameters"])
		require.Equal(t, true, fullDetails["verbose"])
		require.Equal(t, "m", fullDetails["provider_model"])
		require.Greater(t, len(fullDetails["template"].(string)), 1<<20)
		require.Equal(t, largeTemplate, fullDetails["template"], "full detail preserves every template byte")
		compact := makeAuthRequest(t, s, "GET", path+"&compact=true", TestAdminKey, nil)
		require.Equal(t, 200, compact.Code, string(compact.Body))
		require.Less(t, len(compact.Body), 64<<10)
		show := detailData(t, compact)
		require.Equal(t, provider, show["parameters"])
		require.Equal(t, true, show["template_present"])
		require.NotContains(t, show, "template")
		require.NotContains(t, show, "license")
		require.NotContains(t, show["model_info"], "tensor_data")
		require.EqualValues(t, 8192, show["model_info"].(map[string]any)["llama.context_length"])
		for _, version := range []string{"2026-07-28", "2025-06-18"} {
			result := mcpWireResponse(t, s, mcpWireRequest(t, "tools/call", version, map[string]any{"name": "zz_models", "arguments": map[string]any{"model": "M", "provider": provider}}), 200)["result"].(map[string]any)
			require.NotEqual(t, true, result["isError"], result)
			entry := result["structuredContent"].(map[string]any)["models"].([]any)[0].(map[string]any)
			require.Equal(t, provider, entry["show"].(map[string]any)["parameters"])
		}
	}
}

func TestModelDetailFailureStatusContract(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: DefaultTestNodeConfig(), providers: []string{"ollama", "huggingface"}})
	mode := detailDaemon(t, s, "ollama-detail")
	for _, tc := range []struct {
		name string
		mode int32
		want int
	}{
		{"upstream auth", 1, 401},
		{"null", 2, 502},
		{"trailing", 3, 502},
		{"compact overflow", 4, 502},
		{"inventory unavailable", 5, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode.Store(tc.mode)
			if tc.mode == 5 {
				s.model.Cache.Invalidate()
			}
			response := makeAuthRequest(t, s, "GET", "/zzrouter/v1/models/show?model=m&provider=ollama-detail&compact=true", TestAdminKey, nil)
			require.Equal(t, tc.want, response.Code, string(response.Body))
			if tc.mode == 1 || tc.mode == 2 || tc.mode == 3 {
				response = makeAuthRequest(t, s, "POST", "/api/show", TestAdminKey, map[string]any{"model": "ollama-detail/m"})
				require.Equal(t, tc.want, response.Code, string(response.Body))
			}
		})
	}
}

func TestNativeModelDetailUncatalogedContract(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: DefaultTestNodeConfig(), providers: []string{"ollama", "huggingface"}})
	detailDaemon(t, s, "ollama-detail")
	response := makeAuthRequest(t, s, "POST", "/api/show", TestAdminKey, map[string]any{"model": "ollama-detail/hidden", "verbose": true})
	require.Equal(t, 200, response.Code, string(response.Body))
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body, &body))
	require.Equal(t, "ollama-detail", body["parameters"])
	require.Equal(t, "hidden", body["name"])
	require.Equal(t, "hidden", body["provider_model"])
	require.Equal(t, true, body["verbose"])
	require.Equal(t, s.node.Nodename(), body["node"])
	require.NotContains(t, body, "source_id", "uncataloged detail cannot manufacture registry evidence")
	require.Contains(t, body, "template")
	response = makeAuthRequest(t, s, "GET", "/zzrouter/v1/models/show?model=hidden&provider=ollama-detail", TestAdminKey, nil)
	require.Equal(t, 404, response.Code, string(response.Body))
	response = makeAuthRequest(t, s, "GET", "/zzrouter/v1/models/show?model=hidden&provider=ollama-detail&allow_uncataloged=true", TestAdminKey, nil)
	require.Equal(t, 404, response.Code, "public callers cannot enable the native fallback")
}

type detailWorkerClient struct {
	deploymentCatalogClient
	t      *testing.T
	engine *gin.Engine
	calls  int
}

func (c *detailWorkerClient) Unicast(ctx context.Context, node, path string, params *mesh.QueryParams) (*mesh.Response, error) {
	c.calls++
	require.Equal(c.t, "worker", node)
	request := httptest.NewRequest(params.Method, path, nil).WithContext(ctx)
	require.Equal(c.t, "llamacpp", request.URL.Query().Get("provider"))
	require.Equal(c.t, "huggingface", request.URL.Query().Get("registry"))
	require.Equal(c.t, "org/worker", request.URL.Query().Get("source_id"))
	require.Equal(c.t, "true", request.URL.Query().Get("compact"))
	recorder := httptest.NewRecorder()
	c.engine.ServeHTTP(recorder, request)
	return &mesh.Response{StatusCode: recorder.Code, Body: recorder.Body.Bytes(), Headers: recorder.Header()}, nil
}

func TestModelDetailWorkerContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/worker", "served", 32768)
	worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker, NodeName: "worker"}, modelsDir: root, providers: []string{"llamacpp", "vllm", "ollama", "huggingface"}, enabled: []string{"llamacpp"}})
	coordinator := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator, NodeName: "coord"}, providers: []string{"llamacpp", "vllm", "ollama", "huggingface"}})
	coordinator.model.Cache = cache.New(cache.Config{Node: coordinator.node, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: func() *config.AppsConfig { return worker.appsConfig }, ClusterClient: func() mesh.ClusterClient { return deploymentCatalogClient{worker: worker} }, ResolveEndpointToNodename: func(string) string { return "" }})
	client := &detailWorkerClient{deploymentCatalogClient: deploymentCatalogClient{worker: worker}, t: t, engine: buildInternalEngine(worker)}
	coordinator.services.Model.router = routing.NewClusterAwareRouterWithClient(coordinator.node.Nodename(), client, "9090", coordinator.ServeLocalRequest, coordinator.httpClient)

	for _, version := range []string{"2026-07-28", "2025-06-18"} {
		result := mcpWireResponse(t, coordinator, mcpWireRequest(t, "tools/call", version, map[string]any{"name": "zz_models", "arguments": map[string]any{"model": "org/worker", "provider": "llamacpp"}}), 200)["result"].(map[string]any)
		require.NotEqual(t, true, result["isError"], result)
		entry := result["structuredContent"].(map[string]any)["models"].([]any)[0].(map[string]any)
		require.Equal(t, "worker", entry["show"].(map[string]any)["node"])
		require.EqualValues(t, 32768, entry["show"].(map[string]any)["model_info"].(map[string]any)["max_position_embeddings"])
	}
	require.Equal(t, 2, client.calls, "one selected worker request per detail")
}

func TestNativeModelDetailLocalDoesNotBroadcastContract(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeCoordinator}, providers: []string{"ollama", "huggingface"}})
	detailDaemon(t, s, "ollama-detail")
	require.NoError(t, s.cluster.coordinator.RegisterEndpoint(&mesh.Endpoint{Name: "other", URL: "http://other.test:9090", ClusterURL: "https://other.test:9091"}))
	s.clusterMTLSClient = &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("local show attempted a remote request: %s", r.URL)
		return nil, nil
	})}
	response := makeAuthRequest(t, s, "POST", "/api/show", TestAdminKey, map[string]any{"model": "ollama-detail/hidden", "verbose": true})
	require.Equal(t, 200, response.Code, string(response.Body))
	response = makeAuthRequest(t, s, "POST", "/api/show", TestAdminKey, map[string]any{"model": "ollama-detail/hidden@*"})
	require.Equal(t, 400, response.Code, string(response.Body))
}

func detailWeights(t *testing.T, root, repo, file string, contextLength int) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(repo))
	require.NoError(t, os.MkdirAll(dir, 0700))
	body, err := json.Marshal(map[string]any{"max_position_embeddings": contextLength, "architectures": []string{"VisionCausalLM"}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), body, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, file+".gguf"), []byte("GGUF"), 0600))
}

func TestModelDetailSourceIdentityContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/a", "shared", 4096)
	detailWeights(t, root, "org/b", "shared", 8192)
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, NodeName: "detail-node"}, modelsDir: root, providers: []string{"llamacpp", "vllm", "ollama", "huggingface"}, enabled: []string{"llamacpp"}})
	for _, tc := range []struct {
		repo string
		ctx  int
	}{{"org/a", 4096}, {"org/b", 8192}} {
		response := makeAuthRequest(t, s, "GET", "/zzrouter/v1/models/show?provider=llamacpp&compact=true&model="+url.QueryEscape(tc.repo), TestAdminKey, nil)
		require.Equal(t, 200, response.Code, string(response.Body))
		show := detailData(t, response)
		require.Contains(t, show, "model_info")
		require.EqualValues(t, tc.ctx, show["model_info"].(map[string]any)["max_position_embeddings"])
		require.NotContains(t, show, "capabilities")
	}
	response := makeAuthRequest(t, s, "GET", "/zzrouter/v1/models/show?provider=llamacpp&model=shared", TestAdminKey, nil)
	require.Equal(t, 409, response.Code, string(response.Body))
	require.Contains(t, string(response.Body), `"code":"model_source_ambiguous"`)
}

func TestModelDetailVariantAndGroupConstraintContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "org/base", "base", 16384)
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, NodeName: "detail-node"}, modelsDir: root, providers: []string{"llamacpp", "vllm", "ollama", "huggingface"}})
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(sc *config.ServiceConfig) error {
		sc.Enabled = new(bool)
		*sc.Enabled = true
		sc.Models = map[string]config.ModelSpec{"base+agent": {From: "base"}}
		return nil
	}))
	require.NoError(t, s.model.Groups.Set("base+agent", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{{Name: "other", Model: "not-on-disk", App: "vllm"}}}))
	response := makeAuthRequest(t, s, "GET", "/zzrouter/v1/models/show?provider=llamacpp&model=base%2Bagent&compact=true", TestAdminKey, nil)
	require.Equal(t, 200, response.Code, string(response.Body))
	data := detailData(t, response)
	require.Equal(t, "base+agent", data["name"])
	require.Contains(t, data, "model_info")
	require.EqualValues(t, 16384, data["model_info"].(map[string]any)["max_position_embeddings"])
	s.model.Cache.Invalidate()
	response = makeAuthRequest(t, s, "GET", "/zzrouter/v1/models/show?node=detail-node&model=base%2Bagent&compact=true", TestAdminKey, nil)
	require.Equal(t, 200, response.Code, "a pinned variant must populate its provider evidence after invalidation: %s", response.Body)
	require.Equal(t, "base+agent", detailData(t, response)["name"])
}

func TestModelDetailPublisherNamespaceContract(t *testing.T) {
	root := t.TempDir()
	detailWeights(t, root, "llamacpp/publisher", "publisher-weight", 65536)
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: TestNodeConfig{AdminKey: TestAdminKey, NodeName: "detail-node"}, modelsDir: root, providers: []string{"llamacpp", "vllm", "ollama", "huggingface"}, enabled: []string{"llamacpp"}})
	response := makeAuthRequest(t, s, "GET", "/zzrouter/v1/models/show?provider=llamacpp&registry=huggingface&model=llamacpp%2Fpublisher&compact=true", TestAdminKey, nil)
	require.Equal(t, 200, response.Code, string(response.Body))
	require.EqualValues(t, 65536, detailData(t, response)["model_info"].(map[string]any)["max_position_embeddings"])
	for _, version := range []string{"2026-07-28", "2025-06-18"} {
		result := mcpWireResponse(t, s, mcpWireRequest(t, "tools/call", version, map[string]any{"name": "zz_models", "arguments": map[string]any{"model": "llamacpp/publisher", "provider": "llamacpp"}}), 200)["result"].(map[string]any)
		require.NotEqual(t, true, result["isError"], result)
		entry := result["structuredContent"].(map[string]any)["models"].([]any)[0].(map[string]any)
		require.EqualValues(t, 65536, entry["show"].(map[string]any)["model_info"].(map[string]any)["max_position_embeddings"])
	}
}

func TestNativeModelDetailGroupContract(t *testing.T) {
	s := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: DefaultTestNodeConfig(), providers: []string{"ollama", "huggingface"}})
	detailDaemon(t, s, "ollama-detail")
	require.NoError(t, s.model.Groups.Set("group-alias", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: []modelgroup.Replica{{Name: "m", Model: "m", App: "ollama-detail"}}}))
	response := makeAuthRequest(t, s, "POST", "/api/show", TestAdminKey, map[string]any{"model": "group-alias"})
	require.Equal(t, 501, response.Code, string(response.Body))
	require.Contains(t, string(response.Body), "exact catalog model")
}
