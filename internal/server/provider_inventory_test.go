package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func inventoryServer(t *testing.T, failures []cache.InventoryUnavailable) *Server {
	t.Helper()
	s := createTestNodeWithDefaults(t)
	for i := range failures {
		if failures[i].Node == "" {
			failures[i].Node = s.node.Nodename()
		}
	}
	s.model.Cache = cache.New(cache.Config{Node: s.node,
		Registry:        func() *modelregistry.Registry { return nil },
		AppsConfig:      func() *config.AppsConfig { return nil },
		ClusterClient:   func() mesh.ClusterClient { return nil },
		InventoryStatus: func(context.Context, map[string]string) []cache.InventoryUnavailable { return failures },
	})
	require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
	return s
}

func TestLocalWorkerReportsStoppedProviderBeforeForwarding(t *testing.T) {
	s := inventoryServer(t, []cache.InventoryUnavailable{stoppedInventory("")})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set(constants.HeaderServingProvider, "generic-daemon")
	require.Error(t, s.HandleLocalModel(response, request, "known", "chat"))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Contains(t, response.Body.String(), "provider_not_running")
	require.Contains(t, response.Body.String(), "service/start")
}

func stoppedInventory(node string) cache.InventoryUnavailable {
	return cache.InventoryUnavailable{Node: node, Provider: "generic-daemon", Reason: "provider stopped",
		Service: &prov_apps.ProviderServiceStatus{Provider: "generic-daemon", Supervisor: "zzRouter", Managed: true, Enabled: true}}
}

func inventoryContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, w
}

func TestMissingModelOnStoppedNodeReportsRecoveryWithoutClaimingIdentity(t *testing.T) {
	s := inventoryServer(t, []cache.InventoryUnavailable{stoppedInventory("worker")})
	c, response := inventoryContext()
	require.False(t, s.admitResolvedModels(c, &resolver.Resolved{ModelName: "unknown", Node: "worker"}, "worker"))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Contains(t, response.Body.String(), "provider_not_running")
	require.Contains(t, response.Body.String(), "zzRouter")
	require.Contains(t, response.Body.String(), "/service/start?node=worker")
	require.Contains(t, response.Body.String(), "Model existence cannot be established")
	require.NotContains(t, response.Body.String(), "deployments")
}

func TestUnpinnedMissingModelRetainsWorkerInventoryFailure(t *testing.T) {
	s := inventoryServer(t, []cache.InventoryUnavailable{stoppedInventory("worker")})
	c, response := inventoryContext()
	require.False(t, s.admitResolvedModels(c, &resolver.Resolved{ModelName: "unknown"}, ""))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Contains(t, response.Body.String(), "worker")
	response = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.Error(t, s.HandleLocalModel(response, request, "unknown", "chat"))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.NotContains(t, response.Body.String(), "model_not_found")
	c, _ = inventoryContext()
	require.True(t, s.admitResolvedModels(c, &resolver.Resolved{ModelName: "unknown"}, s.node.Nodename()),
		"an explicitly local request does not consult a worker's inventory")
}

func TestProviderInventoryFailureDoesNotBlockHealthyReplicas(t *testing.T) {
	s := inventoryServer(t, []cache.InventoryUnavailable{stoppedInventory("worker")})
	for _, resolved := range []*resolver.Resolved{
		{Node: "other", Provider: "generic-daemon"},
		{Node: "worker", Provider: "healthy-provider"},
	} {
		c, _ := inventoryContext()
		require.True(t, s.admitResolvedModels(c, resolved, resolved.Node))
	}
	c, response := inventoryContext()
	require.False(t, s.admitResolvedModels(c, &resolver.Resolved{Node: "worker", Provider: "generic-daemon"}, "worker"))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
}

func TestProviderInventoryFiltersFallbackButKeepsViableCandidate(t *testing.T) {
	s := inventoryServer(t, []cache.InventoryUnavailable{stoppedInventory("worker")})
	bad := fallback.Candidate{Name: "bad", Model: "shared", App: "generic-daemon", Node: "worker"}
	good := fallback.Candidate{Name: "good", Model: "shared", App: "healthy-provider", Node: "worker"}
	resolved := &resolver.Resolved{Candidates: []fallback.Candidate{bad, good}}
	c, _ := inventoryContext()
	require.True(t, s.admitResolvedModels(c, resolved, ""))
	require.Equal(t, []fallback.Candidate{good}, resolved.Candidates)
	resolved.Candidates = []fallback.Candidate{bad}
	c, response := inventoryContext()
	require.False(t, s.admitResolvedModels(c, resolved, ""))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Contains(t, response.Body.String(), "provider_not_running")
}

func TestInventoryReadFailureDoesNotPretendTheProviderStopped(t *testing.T) {
	s := inventoryServer(t, []cache.InventoryUnavailable{{Node: "worker", Provider: "generic", Reason: "inventory read failed"}})
	problem := s.providerInventoryFailure("worker", "", true)
	require.NotNil(t, problem)
	require.Equal(t, "model_inventory_unavailable", problem.Code)
	require.NotContains(t, problem.Message, "/service/start")
	require.Nil(t, s.providerInventoryFailure("worker", "generic", false), "a failed inventory does not prove a known backend stopped")
	s.model.Cache.Invalidate()
	require.Nil(t, s.providerInventoryFailure("worker", "", true), "invalidated evidence is not a refusal")
}

func TestMissingModelAfterSuccessfulInventoryRemainsNotFound(t *testing.T) {
	s := inventoryServer(t, nil)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.Error(t, s.HandleLocalModel(response, request, "absent", "chat"))
	require.Equal(t, http.StatusNotFound, response.Code)
	require.Contains(t, response.Body.String(), "model_not_found")
}

func TestRunningInstanceDoesNotDependOnAnotherProvidersInventory(t *testing.T) {
	s := inventoryServer(t, nil)
	s.model.Cache = cache.New(cache.Config{Node: s.node, Registry: func() *modelregistry.Registry { return nil },
		AppsConfig: func() *config.AppsConfig { return nil }, ClusterClient: func() mesh.ClusterClient { return nil },
		InventoryStatus: func(context.Context, map[string]string) []cache.InventoryUnavailable {
			return []cache.InventoryUnavailable{stoppedInventory(s.node.Nodename())}
		},
	})
	require.NoError(t, s.model.Cache.RefreshCacheSync(t.Context()))
	running := instance.NewInstance("live", "healthy-provider", "uncatalogued", 12345, 0, 0)
	running.SetStatus(instance.StatusRunning)
	require.NoError(t, s.providers.appMgr.Instances().Register(running))
	t.Cleanup(func() { require.NoError(t, s.providers.appMgr.Instances().Remove(running.ID)) })
	c, _ := inventoryContext()
	require.True(t, s.admitResolvedModels(c, &resolver.Resolved{ModelName: "uncatalogued"}, ""))
}
