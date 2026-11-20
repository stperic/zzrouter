package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
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
