package routing

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stretchr/testify/require"
)

func TestDeletionBroadcastNodeResultsContract(t *testing.T) {
	client := &mockClusterClient{broadcastResp: &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{{Node: "worker", Response: &mesh.Response{StatusCode: 503, Body: []byte(`{"title":"Service Unavailable","deleted":1,"mutation_applied":true}`)}}}}}
	router := NewClusterAwareRouterWithClient("coordinator", client, "9090", mockLocalHandler(200, []byte(`{"deleted":2}`), nil), nil)
	response, err := router.MultiRoute(t.Context(), "/zzrouter/v1/internal/models", "DELETE", map[string][]byte{"*": []byte(`{"pattern":"*"}`)})
	require.NoError(t, err)
	var nodes []struct {
		Node   string         `json:"node"`
		Status int            `json:"status_code"`
		Body   map[string]any `json:"body"`
	}
	require.NoError(t, json.Unmarshal(response.Body, &nodes))
	require.Len(t, nodes, 2, "broadcast deletion must retain every node's effects and failures")
	require.Equal(t, "coordinator", nodes[0].Node)
	require.EqualValues(t, 2, nodes[0].Body["deleted"])
	require.Equal(t, "worker", nodes[1].Node)
	require.Equal(t, 503, nodes[1].Status)
	require.Equal(t, true, nodes[1].Body["mutation_applied"])
	local := NewLocalOnlyRouterWithClient("coordinator", "9090", mockLocalHandler(200, []byte(`{"deleted":2}`), nil), nil)
	response, err = local.MultiRoute(context.Background(), "/zzrouter/v1/internal/models", "DELETE", map[string][]byte{"*": []byte(`{"pattern":"*"}`)})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(response.Body, &nodes))
	require.Len(t, nodes, 1)
	require.Equal(t, 200, nodes[0].Status)
}
