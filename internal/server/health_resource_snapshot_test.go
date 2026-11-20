package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/host/inventory"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestHealthMemoryUsageAndAgeComeFromSnapshot(t *testing.T) {
	at := utils.Now().Add(-7 * time.Second)
	pool := resources.Pool{Kind: resources.Host, TotalMiB: new(int64(64 << 10)), AvailableMiB: new(int64(32 << 10))}
	snapshot := resources.Snapshot{At: at, Pools: []resources.Pool{pool}}
	info := make(map[string]any)
	addHealthMemory(info, snapshot)
	sample := info["memory_sample"].(*inventory.MemorySample)
	require.Equal(t, at.UTC(), sample.ObservedAt)
	require.GreaterOrEqual(t, sample.AgeSeconds, 7.0)
	require.Nil(t, sample.UsedBytes)
	require.NotContains(t, info, "memory_used")
	require.NotContains(t, info, "memory_used_percent")
	wire, err := json.Marshal(sample)
	require.NoError(t, err)
	require.NotContains(t, string(wire), `"used_bytes"`)
	snapshot.Pools[0].UsedMiB = new(int64(17 << 10))
	addHealthMemory(info, snapshot)
	require.Equal(t, new(int64(17<<30)), info["memory_sample"].(*inventory.MemorySample).UsedBytes)
	require.Equal(t, 17.0/64*100, info["memory_used_percent"].(map[string]any)["value"])
}

func TestPeerNodeMemoryUsageRequiresAnObservation(t *testing.T) {
	ep := &mesh.Endpoint{Status: mesh.StatusUp, Snapshot: mesh.EndpointSnapshot{
		HealthReport: mesh.HealthReport{RAMTotalGB: 64, RAMAvailableGB: 32},
	}}
	service := &NodesService{}
	info := service.endpointToNodeInfo(ep)
	require.NotContains(t, info["memory"], "used_percent")
	ep.Snapshot.MemorySample = inventory.NewMemorySample(64<<30, new(int64(17<<30)), utils.Now())
	info = service.endpointToNodeInfo(ep)
	require.Equal(t, 26.6, info["memory"].(map[string]any)["used_percent"])
}

func TestClusterHealthReportsOnlyMeasuredRAMUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		used *int64
		want float64
	}{
		{name: "unknown"},
		{name: "measured", used: new(int64(17 * constants.BytesPerGB)), want: 17},
		{name: "measured zero", used: new(int64(0)), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample := inventory.NewMemorySample(64*constants.BytesPerGB, tc.used, utils.Now())
			state := &mockClusterState{
				localNode: map[string]any{
					"name": "coord", "health_status": "healthy",
					"memory": &inventory.NodeMemory{TotalGB: 64, AvailableGB: 32, Sample: sample},
				},
				endpoints: []*mesh.Endpoint{{NodeName: "worker", Status: mesh.StatusUp, Snapshot: mesh.EndpointSnapshot{
					HealthReport: mesh.HealthReport{RAMTotalGB: 64, RAMAvailableGB: 32, MemorySample: sample},
				}}},
			}
			handler := &ClusterSystemHandlers{nodes: NewNodesService(state, nil, nil)}
			engine := gin.New()
			engine.GET("/zzrouter/v1/health", handler.HandleHealthFromNodes)
			resp := httptest.NewRecorder()
			engine.ServeHTTP(resp, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/zzrouter/v1/health", nil))
			require.Equal(t, http.StatusOK, resp.Code)
			var body struct {
				Data struct {
					Nodes []map[string]any `json:"nodes"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
			require.Len(t, body.Data.Nodes, 2)
			require.Equal(t, 64.0, body.Data.Nodes[1]["ram_total_gb"])
			for _, node := range body.Data.Nodes {
				if tc.used == nil {
					require.NotContains(t, node, "ram_used_gb", "%s", node["hostname"])
				} else {
					require.Equal(t, tc.want, node["ram_used_gb"], "%s", node["hostname"])
				}
			}
		})
	}
}
