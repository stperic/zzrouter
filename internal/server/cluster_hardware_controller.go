package server

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
)

// ClusterGPURow — every GPUInfo field plus the node it lives on.
type ClusterGPURow struct {
	Node string `json:"node"`
	hardware.GPUInfo
}

type ClusterGPUError struct {
	Node  string `json:"node"`
	Error string `json:"error"`
}

// gpus + errors omit no fields — agents skip the nil-vs-[] branch.
type ClusterGPUResponse struct {
	GPUs    []ClusterGPURow   `json:"gpus"`
	Errors  []ClusterGPUError `json:"errors"`
	Summary struct {
		TotalHosts      int `json:"total_hosts"`
		SuccessfulHosts int `json:"successful_hosts"`
		FailedHosts     int `json:"failed_hosts"`
		TotalGPUs       int `json:"total_gpus"`
	} `json:"summary"`
}

// handleClusterHardwareGPUs returns flat per-GPU rows across the cluster
// in the shape an agent walks to answer "where is there a free A100?".
// Aggregates the coord's local inventory plus each peer's via mTLS
// dispatch to /zzrouter/v1/internal/discover/hardware/gpus on the
// worker's cluster engine. Failures degrade per-host into the errors
// array; the response always carries successful rows alongside.
func (s *Server) handleClusterHardwareGPUs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), constants.DiscoveryTimeout)
	defer cancel()

	svc := s.services.Discovery
	if svc == nil {
		respondSuccess(c, "Cluster GPU inventory retrieved", flattenClusterGPUs(map[string]any{"hosts": []ClusterDiscoveryResult{}}))
		return
	}

	data, err := svc.fanOutToCluster(ctx, "/zzrouter/v1/internal/discover/hardware/gpus", svc.DiscoverGPUs)
	if err != nil {
		InternalNodeError(c, err.Error())
		return
	}
	resp := flattenClusterGPUs(data)
	msg := "Cluster GPU inventory retrieved"
	if resp.Summary.FailedHosts > 0 {
		// Honest envelope: a "retrieved" message with non-empty errors
		// reads as success even though some peers failed. Switch the
		// message + status so an agent that cares about completeness
		// can branch on either.
		msg = fmt.Sprintf("Cluster GPU inventory partially retrieved (%d/%d hosts succeeded)",
			resp.Summary.SuccessfulHosts, resp.Summary.TotalHosts)
	}
	respondSuccess(c, msg, resp)
}

// flattenClusterGPUs reshapes a per-host result list into ClusterGPUResponse.
// Defensive on every type assertion — a malformed peer demotes its rows
// to skipped instead of bringing the cluster view down.
func flattenClusterGPUs(data map[string]any) ClusterGPUResponse {
	out := ClusterGPUResponse{
		GPUs:   []ClusterGPURow{},
		Errors: []ClusterGPUError{},
	}

	hosts, _ := data["hosts"].([]ClusterDiscoveryResult)
	out.Summary.TotalHosts = len(hosts)

	for _, h := range hosts {
		if !h.Success {
			out.Errors = append(out.Errors, ClusterGPUError{Node: h.Node, Error: h.Error})
			out.Summary.FailedHosts++
			continue
		}
		out.Summary.SuccessfulHosts++

		gpus, ok := h.Data["gpus"].([]hardware.GPUInfo)
		if !ok {
			gpus = decodeGPUList(h.Data["gpus"])
		}
		for _, g := range gpus {
			out.GPUs = append(out.GPUs, ClusterGPURow{Node: h.Node, GPUInfo: g})
		}
	}
	out.Summary.TotalGPUs = len(out.GPUs)
	return out
}

// decodeGPUList handles the post-JSON shape of `gpus` arriving from a
// remote peer (router response body decoded into map[string]any →
// gpus is []any with map elements, not the typed []GPUInfo).
func decodeGPUList(raw any) []hardware.GPUInfo {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]hardware.GPUInfo, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, hardware.GPUInfo{
			Name:              stringField(m, "name"),
			PCIAddress:        stringField(m, "pci_address"),
			MemoryGB:          floatField(m, "memory_gb"),
			DriverVersion:     stringField(m, "driver_version"),
			CUDAVersion:       stringField(m, "cuda_version"),
			ComputeCapability: stringField(m, "compute_capability"),
		})
	}
	return out
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func floatField(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}
