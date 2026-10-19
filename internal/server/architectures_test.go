package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

func architectureServer(t *testing.T) *Server {
	t.Helper()
	s := featureRoutingServer(t)
	cfg := &config.AppsConfig{}
	for name, architectures := range map[string][]string{"llamacpp": nil, "strata": {"qwen4exp"}, "other": {"other"}} {
		require.NoError(t, cfg.AddApp(name, config.ServiceConfig{Mode: "external", Enabled: new(false), Protocol: config.ProtocolOpenAI,
			Runtime: &config.AppRuntimeConfig{Endpoint: "http://example.test"}, Capabilities: &config.AppCapabilities{Formats: []string{"gguf"}, Architectures: architectures, WireEndpoints: []string{"chat_completions"}}}))
	}
	manager, err := prov_apps.NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	s.providers.appMgr = manager
	s.configStore = config.NewMemoryAppsConfigStore(cfg)
	return s
}

func TestArchitectureDeployEligibility(t *testing.T) {
	s := architectureServer(t)
	for _, tc := range []struct {
		architecture, provider string
		allowUnknown           bool
		want                   []string
	}{
		{"qwen4exp", "", false, []string{"strata"}},
		{"qwen3", "", false, []string{"llamacpp"}},
		{"qwen4exp", "llamacpp", false, []string{}},
		{"", "", false, []string{"llamacpp"}},
		{"", "strata", true, []string{"strata"}},
		{"", "", true, []string{"llamacpp", "other", "strata"}},
	} {
		require.Equal(t, tc.want, s.eligibleDeployProviders("uncached.gguf", "huggingface", "gguf", tc.architecture, tc.provider, false, tc.allowUnknown).Providers)
	}
	require.True(t, s.isLocalNodeCompatible("uncached.gguf", "huggingface", "strata"))
	require.True(t, s.isLocalNodeCompatible("uncached.gguf", "huggingface", ""))
}

func TestDeployArchitectureMismatch(t *testing.T) {
	s := architectureServer(t)
	listed := 0
	executor := NewNodesExecutor(func() map[string]any { return map[string]any{"name": "worker"} }, s.eligibleDeployProviders, func(context.Context, prov_apps.EnsureRequest) process.MemoryAssessment {
		return process.MemoryAssessment{Status: "unknown"}
	})
	router := capacityRouter{serve: func(req *routing.Request) *httptest.ResponseRecorder {
		require.Equal(t, 1, listed, "HF evidence must exist before visiting any node")
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(req.Method, req.Path, nil)
		executor.HandleInternalListCompatibleNodes(c)
		return rec
	}}
	service := NewDeploymentsService(&router, nil, s.appsConfig)
	service.repoFiles = func(context.Context, string) (metadata.RepoFiles, error) {
		listed++
		return metadata.RepoFiles{Architecture: "qwen4exp", Files: []metadata.TreeFileEntry{{Name: "model.gguf", Size: 7}}}, nil
	}
	req := &DeployRequest{Model: "org/model-GGUF", Provider: "llamacpp"}
	_, err := service.planDeploy(t.Context(), req, []string{"worker"})
	err = planError(err) // what Deploy answers
	var routed *RoutedError
	require.ErrorAs(t, err, &routed)
	require.Equal(t, http.StatusBadRequest, routed.StatusCode)
	require.Equal(t, string(httperr.CodeArchitectureUnsupported), routed.ProblemDetails.Code)
	require.Len(t, routed.ProblemDetails.Errors, 1)
	require.Equal(t, routed.ProblemDetails.Code, routed.ProblemDetails.Errors[0].Code)
	evidence := routed.ProblemDetails.Errors[0].Got.(map[string]any)
	require.Equal(t, "qwen4exp", evidence["architecture"])
	require.Equal(t, []string{"strata"}, evidence["eligible_providers"])
	wire, err := json.Marshal(routed.ProblemDetails)
	require.NoError(t, err)
	for _, evidence := range []string{"qwen4exp", "strata", "zz_model_download"} {
		require.Contains(t, string(wire), evidence)
	}
	listed = 0
	req.Provider = "strata"
	service.repoFiles = func(context.Context, string) (metadata.RepoFiles, error) {
		listed++
		return metadata.RepoFiles{Architecture: "qwen3", Files: []metadata.TreeFileEntry{{Name: "model.gguf", Size: 7}}}, nil
	}
	_, err = service.planDeploy(t.Context(), req, []string{"worker"})
	require.ErrorAs(t, planError(err), &routed)
	require.Equal(t, http.StatusBadRequest, routed.StatusCode)
	require.Equal(t, string(httperr.CodeArchitectureUnsupported), routed.ProblemDetails.Code)
	listed = 0
	req.Provider = ""
	service.repoFiles = func(context.Context, string) (metadata.RepoFiles, error) {
		listed++
		return metadata.RepoFiles{Architecture: "qwen4exp", Files: []metadata.TreeFileEntry{{Name: "model.gguf", Size: 7}}}, nil
	}
	plans, err := service.planDeploy(t.Context(), req, []string{"a", "b"})
	require.NoError(t, err)
	require.Equal(t, 1, listed)
	require.Equal(t, "strata", plans["a"].Provider)
	require.Equal(t, "strata", plans["b"].Provider)
	for _, provider := range []string{"", "strata"} {
		listed = 0
		req.Provider = provider
		service.repoFiles = func(context.Context, string) (metadata.RepoFiles, error) {
			listed++
			return metadata.RepoFiles{Files: []metadata.TreeFileEntry{{Name: "model.gguf", Size: 7}}}, nil
		}
		plans, err := service.planDeploy(t.Context(), req, []string{"worker"})
		require.NoError(t, err, "unknown architecture permits explicit selection without force")
		want := provider
		if want == "" {
			want = "llamacpp"
		}
		require.Equal(t, want, plans["worker"].Provider)
	}
	service.router = &deploymentPlanRouter{providers: map[string][]string{"worker": {}}}
	service.repoFiles = func(context.Context, string) (metadata.RepoFiles, error) {
		return metadata.RepoFiles{Architecture: "qwen4exp", Files: []metadata.TreeFileEntry{{Name: "model.gguf", Size: 7}}}, nil
	}
	req.Provider = ""
	_, err = service.planDeploy(t.Context(), req, []string{"worker"})
	var invalid *invalidInputError
	require.ErrorAs(t, err, &invalid)
	require.ErrorContains(t, err, "specify provider")

}

func TestDeployArchitectureRefusalRequiresExcludedCandidate(t *testing.T) {
	for _, tc := range []struct {
		name, provider         string
		formats, architectures []string
		wantArchitecture       bool
	}{
		{name: "wrong named provider", provider: "vllm", formats: []string{"gguf"}},
		{name: "MLX only", formats: []string{"safetensors"}},
		{name: "nothing installed"},
		{name: "every candidate excluded", formats: []string{"gguf"}, architectures: []string{"other"}, wantArchitecture: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := architectureServer(t)
			cfg := &config.AppsConfig{}
			if len(tc.formats) > 0 {
				require.NoError(t, cfg.AddApp("candidate", config.ServiceConfig{Mode: "external", Enabled: new(false), Protocol: config.ProtocolOpenAI,
					Runtime:      &config.AppRuntimeConfig{Endpoint: "http://example.test"},
					Capabilities: &config.AppCapabilities{Formats: tc.formats, Architectures: tc.architectures, WireEndpoints: []string{"chat_completions"}},
				}))
			}
			manager, err := prov_apps.NewProviderAppManager(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
			s.providers.appMgr = manager
			s.configStore = config.NewMemoryAppsConfigStore(cfg)
			executor := NewNodesExecutor(func() map[string]any { return map[string]any{"name": "worker"} }, s.eligibleDeployProviders, func(context.Context, prov_apps.EnsureRequest) process.MemoryAssessment {
				return process.MemoryAssessment{Status: "unknown"}
			})
			router := capacityRouter{serve: func(req *routing.Request) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(req.Method, req.Path, nil)
				executor.HandleInternalListCompatibleNodes(c)
				return rec
			}}
			service := NewDeploymentsService(&router, nil, s.appsConfig)
			service.repoFiles = func(context.Context, string) (metadata.RepoFiles, error) {
				return metadata.RepoFiles{Architecture: "qwen3", Files: []metadata.TreeFileEntry{{Name: "model.gguf", Size: 7}}}, nil
			}
			_, err = service.planDeploy(t.Context(), &DeployRequest{Model: "org/model-GGUF", Provider: tc.provider}, []string{"worker"})
			if tc.wantArchitecture {
				var routed *RoutedError
				require.ErrorAs(t, err, &routed)
				require.Equal(t, string(httperr.CodeArchitectureUnsupported), routed.ProblemDetails.Code)
			} else {
				var invalid *invalidInputError
				require.ErrorAs(t, err, &invalid)
				if tc.provider != "" {
					require.ErrorContains(t, err, "not eligible")
				} else {
					require.ErrorContains(t, err, "specify provider")
				}
			}
		})
	}
}
