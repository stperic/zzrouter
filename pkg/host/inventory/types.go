// Package inventory reports per-node hardware state (disk, memory, GPU)
// for the /system and /hosts JSON APIs. JSON field names are the external
// wire contract — Go type names may change freely, but json tags must not.
package inventory

// NodeDisk reports disk-space metrics for the filesystem that hosts the
// models directory. Wire key: "disk".
type NodeDisk struct {
	TotalGB     float64 `json:"total_gb"`
	AvailableGB float64 `json:"available_gb"`
	UsedPercent float64 `json:"used_percent"`
}

// NodeMemory reports aggregate RAM metrics for the node. Wire key: "memory".
type NodeMemory struct {
	TotalGB     float64 `json:"total_gb"`
	AvailableGB float64 `json:"available_gb"`
	UsedPercent float64 `json:"used_percent"`
}

// NodeGPU reports GPU information for a node.
//
// Group-level MemoryTotalGB / MemoryAvailableGB / UtilizationPercent are
// aggregates across every GPU on the node (sum for memory, mean for
// utilization) sourced from gpu.LiveMetricsContext. Per-card breakdown
// lives in GPUs; those rows come from hardware.DiscoverGPUs via
// gpu.InventoryContext and carry static per-card metadata only (Name,
// MemoryTotalGB, ComputeCapability).
//
// Wire key: "gpu".
type NodeGPU struct {
	Count              int              `json:"count"`
	Type               string           `json:"type"`
	GPUs               []NodeGPUDetails `json:"gpus,omitempty"`
	MemoryTotalGB      float64          `json:"memory_total_gb,omitempty"`
	MemoryAvailableGB  float64          `json:"memory_available_gb,omitempty"`
	UtilizationPercent float64          `json:"utilization_percent,omitempty"`

	// UnifiedMemory reports that GPU memory IS system memory (Apple
	// Silicon). Without it a caller sizing a model would add this node's
	// RAM and VRAM and conclude it has twice the memory it has.
	UnifiedMemory bool `json:"unified_memory,omitempty"`
}

// NodeGPUDetails describes a single GPU. MemoryAvailableGB is a pointer
// so an unmeasured card omits the key entirely: a zero would be read
// downstream as "no VRAM left", which is the opposite of what a missing
// measurement means.
type NodeGPUDetails struct {
	Index             int      `json:"index"`
	Name              string   `json:"name"`
	MemoryTotalGB     float64  `json:"memory_total_gb"`
	MemoryAvailableGB *float64 `json:"memory_available_gb,omitempty"`
	ComputeCapability string   `json:"compute_capability,omitempty"`
}

// NodeGPUUsage is a stable JSON schema slot for future per-card live
// usage data. No code path populates it today. Wire key: "gpu_usage".
type NodeGPUUsage struct {
	Index              int     `json:"index"`
	MemoryUsedGB       float64 `json:"memory_used_gb"`
	MemoryFreeGB       float64 `json:"memory_free_gb"`
	UtilizationPercent float64 `json:"utilization_percent,omitempty"`
	TemperatureC       int     `json:"temperature_c,omitempty"`
}
