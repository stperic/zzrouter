package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capacityRouter struct {
	fakeRouter
	serve func(*routing.Request) *httptest.ResponseRecorder
}

func (r capacityRouter) Route(_ context.Context, req *routing.Request) (*routing.Response, error) {
	rec := r.serve(req)
	return &routing.Response{StatusCode: rec.Code, Body: rec.Body.Bytes()}, nil
}

func capacityService(executor *NodesExecutor) *NodesService {
	return NewNodesService(&mockClusterState{localNode: map[string]any{"name": "node"}}, &capacityRouter{serve: func(req *routing.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(req.Method, req.Path, nil)
		executor.HandleInternalListCompatibleNodes(c)
		return rec
	}}, nil)
}

func TestCompatibleNodesCachedOversizeNotAffirmative(t *testing.T) {
	s := featureRoutingServer(t)
	require.True(t, s.model.Cache.HasModel("same-model"))
	assert.True(t, s.isLocalNodeCompatible("same-model", "huggingface", "vllm"), "cached weights retain download eligibility")
	executor := NewNodesExecutor(func() map[string]any { return map[string]any{"name": "node"} }, func(string, string, string, string, bool) []string { return []string{"vllm"} }, func(context.Context, prov_apps.EnsureRequest) process.MemoryAssessment {
		return process.MemoryAssessment{ContextMode: "numeric", Status: "no_fit", Reason: "weight lower bound", RequiredMiB: 10001, CapacityMiB: 10000}
	})
	result, err := capacityService(executor).ListCompatibleNodes(t.Context(), &ListCompatibleNodesRequest{Model: "same-model"})
	require.NoError(t, err)
	assert.Empty(t, result.Data)
	require.Len(t, result.Assessments, 1)
	assert.Equal(t, "no_fit", result.Assessments[0].Status)
}

func TestCachedModelDownloadEligibilityIgnoresUnsupportedFormat(t *testing.T) {
	s := featureRoutingServer(t)
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("cloud", config.ServiceConfig{Name: "cloud", Mode: "cloud", Enabled: new(true), Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: "https://example.test", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-token"}}, Capabilities: &config.AppCapabilities{Models: []string{"cached.safetensors"}, WireEndpoints: []string{"chat_completions"}}}))
	require.NoError(t, cfg.AddApp("gguf-only", config.ServiceConfig{Name: "gguf-only", Mode: "external", Enabled: new(true), Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: "https://example.test"}, Capabilities: &config.AppCapabilities{Formats: []string{"gguf"}, WireEndpoints: []string{"chat_completions"}}}))
	manager, err := prov_apps.NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	s.providers.appMgr = manager
	s.model.Cache = cache.New(cache.Config{Node: s.node, NodeConfig: &config.NodeConfig{}, Registry: func() *modelregistry.Registry { return nil }, AppsConfig: func() *config.AppsConfig { return cfg }, ClusterClient: s.getClusterClient})
	require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
	require.True(t, s.model.Cache.HasModel("cached.safetensors"))
	require.Empty(t, s.eligibleDeployProviders("cached.safetensors", "huggingface", "", "gguf-only", false))
	assert.True(t, s.isLocalNodeCompatible("cached.safetensors", "huggingface", "gguf-only"))
	assert.False(t, s.isLocalNodeCompatible("uncached.safetensors", "huggingface", "gguf-only"))
}

func TestCompatibleNodesPublicProvidersIncludeOnlyFits(t *testing.T) {
	providers := []string{"fit", "no-fit", "unknown"}
	executor := NewNodesExecutor(func() map[string]any { return map[string]any{"name": "node"} }, func(string, string, string, string, bool) []string { return providers }, func(_ context.Context, req prov_apps.EnsureRequest) process.MemoryAssessment {
		status := map[string]string{"fit": "fit", "no-fit": "no_fit", "unknown": "unknown"}[req.Provider]
		return process.MemoryAssessment{ContextMode: "numeric", Status: status, Reason: status}
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("GET", "/?model=m", nil)
	executor.HandleInternalListCompatibleNodes(c)
	var internal struct {
		Data []struct {
			EligibleProviders []string `json:"eligible_providers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &internal))
	require.Len(t, internal.Data, 1)
	assert.Equal(t, providers, internal.Data[0].EligibleProviders, "storage eligibility keeps all providers")
	engine := gin.New()
	NewNodesController(capacityService(executor)).RegisterPublicRoutes(engine.Group(""))
	public := httptest.NewRecorder()
	engine.ServeHTTP(public, httptest.NewRequest("GET", "/nodes/compatible?model=m", nil))
	require.Equal(t, http.StatusOK, public.Code)
	var reply struct {
		Data []struct {
			EligibleProviders []string `json:"eligible_providers"`
		} `json:"data"`
		Metadata struct {
			Assessments []NodeMemoryAssessment `json:"assessments"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(public.Body.Bytes(), &reply))
	require.Len(t, reply.Data, 1)
	assert.Equal(t, []string{"fit"}, reply.Data[0].EligibleProviders)
	require.Len(t, reply.Metadata.Assessments, 3)
	assert.Equal(t, "fit", reply.Metadata.Assessments[0].Status)
	assert.Equal(t, "no_fit", reply.Metadata.Assessments[1].Status)
	assert.Equal(t, "unknown", reply.Metadata.Assessments[2].Status)
}

func TestCompatibleNodesInventoryErrorPreservesAssessments(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	router := &capacityRouter{serve: func(*routing.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		_, err := rec.WriteString(`{"data":[{"name":"worker","assessments":[{"node":"worker","provider":"p","context_mode":"numeric","status":"fit","reason":"sized"}]}]}`)
		require.NoError(t, err)
		cancel()
		return rec
	}}
	service := NewNodesService(&mockClusterState{localNode: map[string]any{"name": "coord"}}, router, nil)
	result, err := service.ListCompatibleNodes(ctx, &ListCompatibleNodesRequest{Model: "m"})
	require.NoError(t, err)
	require.Len(t, result.Data, 1)
	assert.Equal(t, "worker", result.Data[0]["name"])
	require.Len(t, result.Assessments, 2)
	assert.Empty(t, result.Assessments[0].Node)
	assert.Equal(t, "unknown", result.Assessments[0].Status)
	assert.Equal(t, process.AssessmentFailed, result.Assessments[0].UnknownCause)
	assert.Equal(t, "worker", result.Assessments[1].Node)
	assert.Equal(t, "fit", result.Assessments[1].Status)
	_, err = service.ListNodes(ctx, &ListNodesRequest{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestCompatibleNodesProviderIsolation(t *testing.T) {
	cfg := &config.AppsConfig{}
	for _, name := range []string{"small", "large"} {
		require.NoError(t, cfg.AddApp(name, config.ServiceConfig{Name: name, Mode: "external", Enabled: new(true), Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: "https://example.test"}, Capabilities: &config.AppCapabilities{Formats: []string{"gguf"}, WireEndpoints: []string{"chat_completions"}}}))
	}
	manager, err := prov_apps.NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	nc := &config.NodeConfig{}
	nc.Node.Name = "node"
	s := &Server{node: NewNodeIdentity(nc)}
	s.providers.appMgr = manager
	assessed := []string{}
	executor := NewNodesExecutor(func() map[string]any { return map[string]any{"name": "node"} }, s.eligibleDeployProviders, func(_ context.Context, req prov_apps.EnsureRequest) process.MemoryAssessment {
		assessed = append(assessed, req.Provider)
		status := "no_fit"
		if req.Provider == "small" {
			status = "fit"
		}
		return process.MemoryAssessment{ContextMode: "numeric", Status: status, Reason: status}
	})
	for _, provider := range []string{"large", "LARGE", "small", "missing"} {
		t.Run(provider, func(t *testing.T) {
			assessed = nil
			result, err := capacityService(executor).ListCompatibleNodes(t.Context(), &ListCompatibleNodesRequest{Model: "model.gguf", Registry: "huggingface", Provider: provider})
			require.NoError(t, err)
			if provider == "small" {
				require.Len(t, result.Data, 1)
			} else {
				assert.Empty(t, result.Data)
			}
			if provider == "missing" {
				assert.Empty(t, assessed)
			} else if provider == "small" {
				assert.Equal(t, []string{"small"}, assessed)
			} else {
				assert.Equal(t, []string{"large"}, assessed)
			}
		})
	}
}

func TestCompatibleNodesWorkerAssessment(t *testing.T) {
	state := &mockClusterState{localNode: map[string]any{"name": "coord"}, endpoints: []*mesh.Endpoint{{NodeName: "z-worker"}, {NodeName: "a-old"}, {NodeName: "timeout"}}}
	for _, body := range []string{
		`{"data":[{"name":"z-worker","assessments":[{"node":"z-worker","provider":"z","context_mode":"numeric","status":"fit","reason":"sized","required_mib":11,"capacity_mib":12},{"node":"z-worker","provider":"a","status":"unknown","reason":"missing geometry"}]},{"name":"coord","assessments":[{"node":"coord","provider":"mlx","status":"unknown","reason":"unified"}]},{"name":"a-old","compatible":true}]}`,
		`{"data":[{"name":"a-old","compatible":true},{"name":"coord","assessments":[{"node":"coord","provider":"mlx","status":"unknown","reason":"unified"}]},{"name":"z-worker","assessments":[{"node":"z-worker","provider":"a","status":"unknown","reason":"missing geometry"},{"node":"z-worker","provider":"z","context_mode":"numeric","status":"fit","reason":"sized","required_mib":11,"capacity_mib":12}]}]}`,
	} {
		router := &capacityRouter{serve: func(*routing.Request) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			rec.WriteHeader(200)
			_, err := rec.WriteString(body)
			require.NoError(t, err)
			return rec
		}}
		service := NewNodesService(state, router, nil)
		engine := gin.New()
		NewNodesController(service).RegisterPublicRoutes(engine.Group(""))
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest("GET", "/nodes/compatible?name=m&limit=1&offset=1", nil))
		require.Equal(t, 200, rec.Code)
		var reply struct {
			Data     []map[string]any `json:"data"`
			Total    int              `json:"total"`
			Metadata struct {
				Assessments []NodeMemoryAssessment `json:"assessments"`
			} `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply))
		assert.Equal(t, 1, reply.Total)
		assert.Empty(t, reply.Data, "pagination follows capacity filtering")
		require.Len(t, reply.Metadata.Assessments, 5)
		got := []string{}
		for _, a := range reply.Metadata.Assessments {
			got = append(got, a.Node+":"+a.Provider+":"+a.Status)
			assert.NotEmpty(t, a.Reason)
		}
		assert.Equal(t, []string{"a-old::unknown", "coord:mlx:unknown", "timeout::unknown", "z-worker:a:unknown", "z-worker:z:fit"}, got)
		assert.Equal(t, int64(11), reply.Metadata.Assessments[4].RequiredMiB)
		assert.Equal(t, process.NodeOutdated, reply.Metadata.Assessments[0].UnknownCause)
		assert.Equal(t, process.NodeUnreachable, reply.Metadata.Assessments[2].UnknownCause)
		assert.Equal(t, process.NodeOutdated, reply.Metadata.Assessments[3].UnknownCause)
	}
}

func TestCompatibleNodesCurrentProviderNotEligible(t *testing.T) {
	executor := NewNodesExecutor(func() map[string]any { return map[string]any{"name": "node"} }, func(string, string, string, string, bool) []string { return nil }, func(context.Context, prov_apps.EnsureRequest) process.MemoryAssessment {
		t.Fatal("ineligible provider must not be assessed")
		return process.MemoryAssessment{}
	})
	result, err := capacityService(executor).ListCompatibleNodes(t.Context(), &ListCompatibleNodesRequest{Model: "m", Provider: "missing"})
	require.NoError(t, err)
	assert.Empty(t, result.Data)
	require.Len(t, result.Assessments, 1)
	a := result.Assessments[0]
	assert.Equal(t, "node", a.Node)
	assert.Equal(t, "missing", a.Provider)
	assert.Equal(t, "unknown", a.Status)
	assert.Equal(t, process.ProviderNotEligible, a.UnknownCause)
}

func TestDeploymentEligibilityIgnoresCapacityUnknown(t *testing.T) {
	executor := NewNodesExecutor(func() map[string]any { return map[string]any{"name": "worker"} }, func(string, string, string, string, bool) []string { return []string{"llamacpp"} }, func(context.Context, prov_apps.EnsureRequest) process.MemoryAssessment {
		return process.MemoryAssessment{ContextMode: "numeric", Status: "unknown", Reason: "missing weights"}
	})
	router := &capacityRouter{serve: func(req *routing.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(req.Method, req.Path, nil)
		executor.HandleInternalListCompatibleNodes(c)
		return rec
	}}
	cfg := featureTestConfig(t)
	s := NewDeploymentsService(router, nil, func() *config.AppsConfig { return cfg })
	s.repoFiles = func(context.Context, string) ([]metadata.TreeFileEntry, error) {
		return []metadata.TreeFileEntry{{Name: "model.gguf", Size: 7}, {Name: "mmproj.gguf", Size: 9}}, nil
	}
	plans, err := s.planDeploy(t.Context(), &DeployRequest{Model: "org/model-GGUF", Provider: "llamacpp"}, []string{"worker"})
	require.NoError(t, err)
	assert.Equal(t, "llamacpp", plans["worker"].Provider)
}

func TestPreviewWorkerWithoutAssessmentIsUnknown(t *testing.T) {
	s := NewLoadService(nil, &loadResultRouter{status: http.StatusOK, body: `{"model":"m","provider":"vllm","full_command":"engine"}`})
	preview, err := s.PreviewRun(t.Context(), &PreviewRunRequest{ModelName: "m", Provider: "vllm", Node: "worker"})
	require.NoError(t, err)
	require.NotNil(t, preview.Assessment)
	assert.Equal(t, "unknown", preview.Assessment.Status)
	assert.Equal(t, process.NodeOutdated, preview.Assessment.UnknownCause)
	assert.NotEmpty(t, preview.Assessment.Reason)
}

func TestCompatibleNodesSortFits(t *testing.T) {
	router := &capacityRouter{serve: func(*routing.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		_, err := rec.WriteString(`{"data":[{"name":"z","assessments":[{"node":"z","provider":"p","context_mode":"numeric","status":"fit","reason":"sized"}]},{"name":"a","assessments":[{"node":"a","provider":"p","context_mode":"numeric","status":"fit","reason":"sized"}]}]}`)
		require.NoError(t, err)
		return rec
	}}
	service := NewNodesService(&mockClusterState{localNode: map[string]any{"name": "coord"}}, router, nil)
	result, err := service.ListCompatibleNodes(t.Context(), &ListCompatibleNodesRequest{Model: "m"})
	require.NoError(t, err)
	require.Len(t, result.Data, 2)
	assert.Equal(t, "a", result.Data[0]["name"])
	assert.Equal(t, "z", result.Data[1]["name"])
}

func TestLegacyAssessmentDegradesToUnknown(t *testing.T) {
	for _, status := range []string{"fit", "no_fit", "unknown"} {
		t.Run(status, func(t *testing.T) {
			assessment := `{"status":"` + status + `","reason":"old sizing","required_mib":17}`
			router := &capacityRouter{serve: func(*routing.Request) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				_, err := rec.WriteString(`{"data":[{"name":"node","assessments":[{"node":"node","provider":"p","status":"` + status + `","reason":"old sizing"}]}]}`)
				require.NoError(t, err)
				return rec
			}}
			service := NewNodesService(&mockClusterState{localNode: map[string]any{"name": "node"}}, router, nil)
			nodes, err := service.ListCompatibleNodes(t.Context(), &ListCompatibleNodesRequest{Model: "m"})
			require.NoError(t, err)
			require.Empty(t, nodes.Data)
			require.Len(t, nodes.Assessments, 1)
			assert.Equal(t, "unknown", nodes.Assessments[0].Status)
			assert.Equal(t, "unknown", nodes.Assessments[0].ContextMode)
			s := NewLoadService(nil, &loadResultRouter{status: 200, body: `{"model":"m","provider":"vllm","assessment":` + assessment + `}`})
			preview, err := s.PreviewRun(t.Context(), &PreviewRunRequest{ModelName: "m", Provider: "vllm", Node: "worker"})
			require.NoError(t, err)
			assert.Equal(t, "unknown", preview.Assessment.Status)
			assert.Equal(t, "unknown", preview.Assessment.ContextMode)
			assert.Zero(t, preview.Assessment.RequiredMiB)
		})
	}
}

func TestAutoContextRESTPreservesEvidence(t *testing.T) {
	for _, status := range []string{"fit", "no_fit", "unknown"} {
		t.Run(status, func(t *testing.T) {
			evidence := process.MemoryAssessment{Status: "fit", Reason: "sized", ContextMode: "auto", ContextTokens: 4096, AffordableContextTokens: 8192, Placement: "offload"}
			evidence.Status, evidence.WeightsMiB, evidence.CapacityMiB = status, 1024, 10000
			if status == "unknown" {
				evidence.UnknownCause = process.ModelUnmodelled
			}
			executor := NewNodesExecutor(func() map[string]any { return map[string]any{"name": "node"} }, func(string, string, string, string, bool) []string { return []string{"p"} }, func(context.Context, prov_apps.EnsureRequest) process.MemoryAssessment { return evidence })
			nodes, err := capacityService(executor).ListCompatibleNodes(t.Context(), &ListCompatibleNodesRequest{Model: "m"})
			require.NoError(t, err)
			if status == "fit" {
				require.Len(t, nodes.Data, 1)
			} else {
				assert.Empty(t, nodes.Data)
			}
			require.Len(t, nodes.Assessments, 1)
			assert.Equal(t, evidence, nodes.Assessments[0].MemoryAssessment)
			body, err := json.Marshal(map[string]any{"assessment": evidence})
			require.NoError(t, err)
			s := NewLoadService(nil, &loadResultRouter{status: 200, body: string(body)})
			preview, err := s.PreviewRun(t.Context(), &PreviewRunRequest{ModelName: "m", Provider: "p", Node: "worker"})
			require.NoError(t, err)
			require.NotNil(t, preview.Assessment)
			assert.Equal(t, evidence, *preview.Assessment)
		})
	}
}
