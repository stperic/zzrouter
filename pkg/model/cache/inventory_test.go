package cache

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stretchr/testify/require"
)

func TestInventoryEvidencePublishesWithCatalogGeneration(t *testing.T) {
	c := newWorkerCache(t)
	failure := InventoryUnavailable{Node: "worker-1", Provider: "generic", Reason: "stopped", Service: &prov_apps.ProviderServiceStatus{Supervisor: "zzRouter", Managed: true}}
	require.True(t, c.publishModelItems(nil, c.generation, []InventoryUnavailable{failure}))
	oldGeneration := c.generation
	c.Invalidate()
	require.False(t, c.publishModelItems(nil, oldGeneration, []InventoryUnavailable{failure}))
	require.Empty(t, c.UnavailableInventories("worker-1", "generic"), "stale source evidence cannot reappear")
	require.True(t, c.publishModelItems(nil, c.generation, []InventoryUnavailable{failure}))
	got := c.UnavailableInventories("WORKER-1", "generic")
	require.Len(t, got, 1)
	got[0].Service.Supervisor = "changed"
	require.Equal(t, "zzRouter", c.UnavailableInventories("worker-1", "generic")[0].Service.Supervisor)
	require.Empty(t, c.UnavailableInventories("other", "generic"))
	require.Empty(t, c.UnavailableInventories("worker-1", "other"))
	c.registry = nil // These admissions cannot populate, probe or scan.
	c.appsConfig = nil
	require.Len(t, c.UnavailableInventories("worker-1", "generic"), 1)
	require.True(t, c.publishModelItems(nil, c.generation, nil))
	require.Empty(t, c.UnavailableInventories("worker-1", "generic"), "successful refresh clears failure evidence")
}

type inventoryClusterClient struct {
	mesh.ClusterClient
	response *mesh.BroadcastResponse
}

func (c *inventoryClusterClient) Broadcast(context.Context, string, *mesh.QueryParams) (*mesh.BroadcastResponse, error) {
	return c.response, nil
}

func TestWorkerInventoryFailureSurvivesAnEmptyModelList(t *testing.T) {
	body, err := json.Marshal(map[string]any{"models": []map[string]any{}, "inventory_unavailable": []InventoryUnavailable{
		{Node: "wrong-body-node", Provider: "generic", Reason: "stopped", Service: &prov_apps.ProviderServiceStatus{Supervisor: "zzRouter", Managed: true}},
	}})
	require.NoError(t, err)
	client := &inventoryClusterClient{response: &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{
		{NodeName: "remote-worker", Response: &mesh.Response{StatusCode: 200, Body: body}},
	}}}
	c := newWorkerCache(t)
	c.clusterClient = func() mesh.ClusterClient { return client }
	require.NoError(t, c.RefreshCacheSync(t.Context()))
	require.Empty(t, c.GetAllModels())
	got := c.UnavailableInventories("remote-worker", "generic")
	require.Len(t, got, 1)
	require.Equal(t, "zzRouter", got[0].Service.Supervisor)
	require.Empty(t, c.UnavailableInventories("wrong-body-node", "generic"), "the response cannot substitute another node's evidence")
}

func TestPinnedLookupDoesNotBorrowAnotherNodesProvider(t *testing.T) {
	c := newWorkerCache(t)
	require.True(t, c.publishModelItems([]map[string]any{
		{"name": "shared", "assigned_app": "a", "node": "one"},
		{"name": "shared", "assigned_app": "b", "node": "two"},
	}, c.generation, nil))
	got, found := c.LookupByNode("SHARED", "ONE")
	require.True(t, found)
	require.Equal(t, "a", got.Provider)
	got, found = c.LookupByNode("shared", "two")
	require.True(t, found)
	require.Equal(t, "b", got.Provider)
	_, found = c.LookupByNode("shared", "three")
	require.False(t, found)
}

func TestInventoryEnvelopePreservesProtocolFiveModels(t *testing.T) {
	body := struct {
		Models      []map[string]any       `json:"models"`
		Unavailable []InventoryUnavailable `json:"inventory_unavailable"`
	}{Models: []map[string]any{{"name": "healthy"}}, Unavailable: []InventoryUnavailable{{Node: "worker", Provider: "generic", Reason: "failed"}}}
	data, err := json.Marshal(body)
	require.NoError(t, err)
	var old struct {
		Models []map[string]any `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &old))
	require.Equal(t, "healthy", old.Models[0]["name"])
}
