package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClusterHardwareGPUs_EndpointResponds(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeAuthRequest(t, server, "GET", "/zzrouter/v1/cluster/hardware/gpus", TestAdminKey, nil)
	require.Equalf(t, http.StatusOK, resp.Code, "body=%s", string(resp.Body))

	var env struct {
		Success bool               `json:"success"`
		Data    ClusterGPUResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &env))
	assert.True(t, env.Success)
	assert.GreaterOrEqual(t, env.Data.Summary.TotalHosts, 1, "test node must report at least itself")
	// Whether the test box has GPUs or not, the gpus list is a non-nil
	// JSON array (we initialize it explicitly so an agent can iterate
	// without a nil-array branch).
	assert.NotNil(t, env.Data.GPUs)
	assert.NotNil(t, env.Data.Errors)
}

// TestFlattenClusterGPUs_TypedHostList covers the local-fanout case where
// hosts[].Data["gpus"] is the typed []hardware.GPUInfo slice (no JSON
// round-trip).
func TestFlattenClusterGPUs_TypedHostList(t *testing.T) {
	hosts := []ClusterDiscoveryResult{
		{
			Node:    "coord",
			Success: true,
			Data: map[string]any{
				"gpus": []hardware.GPUInfo{
					{Name: "RTX 4090", PCIAddress: "0000:01:00.0", MemoryGB: 24, CUDAVersion: "12.4"},
				},
			},
		},
		{
			Node:    "worker-1",
			Success: true,
			Data: map[string]any{
				"gpus": []hardware.GPUInfo{
					{Name: "RTX PRO 6000", MemoryGB: 96},
					{Name: "RTX PRO 6000", MemoryGB: 96},
				},
			},
		},
		{
			Node:    "windows-worker",
			Success: false,
			Error:   "connection timeout",
		},
	}
	out := flattenClusterGPUs(map[string]any{"hosts": hosts})

	require.Len(t, out.GPUs, 3, "two local + one coord")
	assert.Equal(t, "coord", out.GPUs[0].Node)
	assert.Equal(t, "RTX 4090", out.GPUs[0].Name)
	assert.Equal(t, 24.0, out.GPUs[0].MemoryGB)
	assert.Equal(t, "worker-1", out.GPUs[1].Node)

	require.Len(t, out.Errors, 1)
	assert.Equal(t, "windows-worker", out.Errors[0].Node)
	assert.Equal(t, "connection timeout", out.Errors[0].Error)

	assert.Equal(t, 3, out.Summary.TotalHosts)
	assert.Equal(t, 2, out.Summary.SuccessfulHosts)
	assert.Equal(t, 1, out.Summary.FailedHosts)
	assert.Equal(t, 3, out.Summary.TotalGPUs)
}

// TestFlattenClusterGPUs_RemoteHostJSONShape covers the remote-host code
// path where Data went through json.Unmarshal into map[string]any —
// gpus is []any with map[string]any elements, NOT typed []GPUInfo.
func TestFlattenClusterGPUs_RemoteHostJSONShape(t *testing.T) {
	hosts := []ClusterDiscoveryResult{
		{
			Node:    "remote",
			Success: true,
			Data: map[string]any{
				"gpus": []any{
					map[string]any{
						"name":               "RTX A5000",
						"memory_gb":          float64(24),
						"pci_address":        "0000:65:00.0",
						"cuda_version":       "12.4",
						"compute_capability": "86",
					},
				},
			},
		},
	}
	out := flattenClusterGPUs(map[string]any{"hosts": hosts})

	require.Len(t, out.GPUs, 1)
	assert.Equal(t, "remote", out.GPUs[0].Node)
	assert.Equal(t, "RTX A5000", out.GPUs[0].Name)
	assert.Equal(t, 24.0, out.GPUs[0].MemoryGB)
	assert.Equal(t, "0000:65:00.0", out.GPUs[0].PCIAddress)
	assert.Equal(t, "86", out.GPUs[0].ComputeCapability)
}

// TestFlattenClusterGPUs_MalformedRemoteDoesntCrashCluster — a remote
// node that sends garbage in `gpus` shouldn't take down the whole
// cluster view. Flattener silently skips the bad rows, the rest of
// the cluster still aggregates. Covers every wrong-shape variant
// (string, map, nil) so a regression in decodeGPUList is caught.
func TestFlattenClusterGPUs_MalformedRemoteDoesntCrashCluster(t *testing.T) {
	hosts := []ClusterDiscoveryResult{
		{
			Node:    "good",
			Success: true,
			Data:    map[string]any{"gpus": []hardware.GPUInfo{{Name: "RTX 4090", MemoryGB: 24}}},
		},
		{Node: "garbage-string", Success: true, Data: map[string]any{"gpus": "not-an-array"}},
		{Node: "garbage-map", Success: true, Data: map[string]any{"gpus": map[string]any{"oops": 1}}},
		{Node: "garbage-nil", Success: true, Data: map[string]any{"gpus": nil}},
	}
	out := flattenClusterGPUs(map[string]any{"hosts": hosts})

	require.Len(t, out.GPUs, 1, "good node still contributes; bad nodes skipped silently")
	assert.Equal(t, "good", out.GPUs[0].Node)
	assert.Equal(t, 4, out.Summary.SuccessfulHosts, "garbage was non-error from upstream's POV")
}

// TestFlattenClusterGPUs_NoHosts — empty cluster returns the empty
// envelope; agent iterating .gpus / .errors hits zero rows, not nil.
func TestFlattenClusterGPUs_NoHosts(t *testing.T) {
	out := flattenClusterGPUs(map[string]any{})

	assert.NotNil(t, out.GPUs)
	assert.NotNil(t, out.Errors)
	assert.Equal(t, 0, out.Summary.TotalHosts)
	assert.Equal(t, 0, out.Summary.TotalGPUs)
}
