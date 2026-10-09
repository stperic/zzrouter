package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

func workerVariantCatalog(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, qwenModel+".gguf"), []byte("GGUF"), 0600))
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(modelregistry.ClearModelsRootDirOverride)
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker, NodeName: "localai", SeedProvidersDir: true})
	require.NoError(t, s.configStore.SetProviderEnabled("llamacpp", true))
	require.NoError(t, s.configStore.ApplyParameterPatch("llamacpp", func(sc *config.ServiceConfig) error {
		sc.Models = map[string]config.ModelSpec{variantChat: {From: qwenModel}}
		return nil
	}))
	s.model.Cache.Start(t.Context())
	t.Cleanup(func() { s.model.Cache.Stop(context.Background()) })
	require.Equal(t, true, s.model.Cache.GetCacheStats()["valid"])
	return s, root
}

func TestWorkerCatalogPublicationRestoresVariantAdmission(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			s, root := workerVariantCatalog(t)
			dir := filepath.Join(root, "owner", "weights")
			require.NoError(t, os.MkdirAll(dir, 0750))
			require.NoError(t, os.WriteFile(filepath.Join(dir, strings.ToUpper(variantChat)+".gguf"), []byte("GGUF"), 0600))
			s.model.Cache.Invalidate()
			path := "/zzrouter/v1/internal/models"
			if method == http.MethodPost {
				path += "/rescan"
			}
			w := httptest.NewRecorder()
			buildInternalEngine(s).ServeHTTP(w, httptest.NewRequest(method, path, nil))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), strings.ToUpper(variantChat))
			require.Equal(t, true, s.model.Cache.GetCacheStats()["valid"], "publishing a worker catalog must also publish its admission index")
			for _, name := range []string{variantChat, strings.ToUpper(variantChat), "owner/weights"} {
				require.ErrorIs(t, s.providers.appMgr.ValidateModel(t.Context(), name), config.ErrModelNameConflict)
				_, err := s.providers.appMgr.LaunchInstance(t.Context(), prov_apps.LaunchRequest{Provider: "llamacpp", Model: name})
				require.ErrorIs(t, err, config.ErrModelNameConflict)
			}
			require.Empty(t, s.providers.appMgr.Instances().List())
		})
	}
}

func TestWorkerLaunchPreservesVariantCatalog(t *testing.T) {
	s, root := workerVariantCatalog(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, strings.ToUpper(variantChat)+".gguf"), []byte("GGUF"), 0600))
	s.model.Cache.Invalidate()
	require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
	executor := s.newLoadExecutor()
	launches := 0
	executor.launchOnDemand = func(_ context.Context, model, provider, _ string, _, _ map[string]string) (*instance.Instance, error) {
		launches++
		return instance.NewInstance("catalog-launch", provider, model, 8300, 0, 0), nil
	}
	for range 2 {
		result, err := executor.LoadLocalModel(t.Context(), &LoadModelRequest{ModelName: qwenModel, Provider: "llamacpp"})
		require.NoError(t, err)
		require.Zero(t, result.StatusCode)
		require.Equal(t, true, s.model.Cache.GetCacheStats()["valid"], "a launch changes no physical model identity")
		require.ErrorIs(t, s.providers.appMgr.ValidateModel(t.Context(), variantChat), config.ErrModelNameConflict)
	}
	require.Equal(t, 2, launches)
}

func TestWorkerCatalogPreservesInternalModelsWireBody(t *testing.T) {
	s, _ := workerVariantCatalog(t)
	require.NoError(t, s.appsConfig.AddApp("unreadable", config.ServiceConfig{
		Enabled: new(true), Mode: "external", Protocol: config.ProtocolOllama,
		Runtime:      &config.AppRuntimeConfig{Endpoint: "invalid://inventory"},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	require.NoError(t, s.appsConfig.AddApp("cloud", config.ServiceConfig{
		Enabled: new(true), Mode: "cloud", Protocol: config.ProtocolOpenAI,
		Runtime:      &config.AppRuntimeConfig{Endpoint: "https://example.test", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-token"}},
		Capabilities: &config.AppCapabilities{Models: []string{"cloud-model"}, WireEndpoints: []string{"chat_completions"}},
	}))
	s.model.Cache.Invalidate()
	models, failures, err := s.model.Registry.ModelSnapshot(t.Context())
	require.NoError(t, err)
	require.Contains(t, failures, "unreadable")
	items := make([]map[string]any, 0, len(models))
	for _, m := range models {
		items = append(items, m.ToOllamaFormat(s.node.Nodename()))
	}
	items = cache.EnrichLocalItems(items, s.configStore.Config(), s.providers.appMgr.ModelFeatureDetails)
	require.NotEmpty(t, items)
	require.Contains(t, items[0]["details"], "features")
	expected := map[string]any{
		"models": items, "inventory_unavailable": s.providerInventoryStatus(t.Context(), failures),
	}
	if metrics := s.newInternalExecutor().getResourceMetrics(); metrics != nil {
		expected["resources"] = metrics
	}
	before, err := json.Marshal(expected)
	require.NoError(t, err)
	engine := buildInternalEngine(s)
	for _, state := range []string{"invalid", "published"} {
		t.Run(state, func(t *testing.T) {
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/zzrouter/v1/internal/models", nil))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.JSONEq(t, string(before), w.Body.String())
			require.Equal(t, true, s.model.Cache.GetCacheStats()["valid"])
		})
	}
}
