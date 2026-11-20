package server

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/host/inventory"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

// ============================================================================
// /health endpoint
// ============================================================================
//
// This file owns the /health handler and the system-info map
// it serves. Previously mixed with GPU detection helpers in
// system_utils.go; after the pkg/discovery/gpu consolidation
// arc, the only things left were handleHealth + GetSystemInfo,
// so the file was renamed to describe what it actually does.
//
// The /zzrouter/v1/system endpoint lives in system.go and
// system_controller.go — it's a different request flow that
// goes through SystemService/SystemExecutor for cluster-wide
// aggregation. This file serves the bare liveness probe at
// GET /health used by kubelet, docker healthcheck, and
// monitoring loops.

// handleHealth provides basic health information about the server.
func (s *Server) handleHealth(c *gin.Context) {
	// Get hostname using centralized helper (DRY)
	hostname := s.node.Nodename()

	// Calculate uptime
	uptime := time.Since(s.startTime)

	// Get system information
	systemInfo := s.GetSystemInfo()

	// Get running models count from InstanceRegistry
	runningCount := 0
	if s.providers.appMgr != nil {
		for _, inst := range s.providers.appMgr.Instances().ListRunning() {
			if inst.Model != "" {
				runningCount++
			}
		}
	}

	// Get provider count
	providerCount := s.providers.appMgr.Protocols().Count()

	_, port, addr := s.nodeEndpointFields()
	health := map[string]any{
		"status":         "healthy",
		"hostname":       hostname,
		"uptime_seconds": int(uptime.Seconds()),
		"version":        version.Current.String(),
		"running_models": runningCount,
		"providers":      providerCount,
		"apps_detail":    s.GetLocalAppsInfo(), // reads from cache
		// Drift-detection signal: coord compares against EndpointSnapshot.CollectedAt
		// to know whether a cache refresh is needed when a notify was lost.
		"last_config_mutation_at": time.Unix(0, s.lastMutation.Load()).UTC(),
		"port":                    port,
		"address":                 addr,
		"os":                      getOSName(),
		"mdns_discovery":          s.config.MDNSDiscovery,
		"coordinator":             s.config.Cluster.IsCoordinator(),
	}

	// Add system information
	maps.Copy(health, systemInfo)
	if s.providers.appMgr != nil {
		health["activity"], health["provider_activity"] = s.providers.appMgr.Activity(utils.Now())
	}
	if s.cluster.resources != nil {
		health["resources"] = s.cluster.resources.GetMetrics()
	}

	respondRetrievalSuccess(c, "Health check", health)
}

// GetSystemInfo serves the shared node resource sample in the existing health format.
func (s *Server) GetSystemInfo() map[string]any {
	info := make(map[string]any)
	ctx := context.Background()
	cache := inventory.Default()

	snapshot := cache.GetSnapshot(ctx)
	addHealthMemory(info, snapshot)

	// Disk information — pick the primary mount from the disk
	// slice ("/" on Unix, "C:\" on Windows) and fall back to the
	// first entry on unusual setups. Values come back in GB from
	// the hardware layer; convert to bytes for the /health JSON
	// shape consumers expect.
	if root := rootDisk(cache.GetDiskInfo(ctx)); root != nil {
		totalBytes := int64(root.TotalGB * float64(constants.BytesPerGB))
		freeBytes := int64(root.AvailableGB * float64(constants.BytesPerGB))
		usedBytes := int64(root.UsedGB * float64(constants.BytesPerGB))
		info["disk_total"] = map[string]any{
			"value":         uint64(totalBytes),
			"display_value": utils.FormatSize(totalBytes),
		}
		info["disk_free"] = map[string]any{
			"value":         uint64(freeBytes),
			"display_value": utils.FormatSize(freeBytes),
		}
		info["disk_used"] = map[string]any{
			"value":         uint64(usedBytes),
			"display_value": utils.FormatSize(usedBytes),
		}
		info["disk_used_percent"] = map[string]any{
			"value":         root.UsedPercent,
			"display_value": fmt.Sprintf("%.2f%%", root.UsedPercent),
		}
	} else {
		info["disk_total"] = map[string]any{"value": 0, "display_value": "N/A"}
		info["disk_free"] = map[string]any{"value": 0, "display_value": "N/A"}
		info["disk_used"] = map[string]any{"value": 0, "display_value": "N/A"}
		info["disk_used_percent"] = map[string]any{"value": 0.0, "display_value": "N/A"}
	}

	// CPU information — permanently cached after first call.
	// cpu_count now reports logical threads (CPUThreads) instead
	// of the pre-refactor len(cpu.Info()) which was typically 1
	// on x86 because gopsutil collapses per-CPU entries into a
	// single row. CPUThreads is the useful scheduling figure
	// (e.g. 12 on a 6-core Intel with hyper-threading).
	if cpuInfo := cache.GetCPUInfo(ctx); cpuInfo != nil && cpuInfo.CPUThreads > 0 {
		info["cpu_count"] = cpuInfo.CPUThreads
		info["cpu_model"] = cpuInfo.CPUModel
	} else {
		info["cpu_count"] = 0
		info["cpu_model"] = "N/A"
	}

	// Only compute cards belong in node views.
	cards := gpu.List(false).Cards

	gpuInfo := make([]map[string]any, 0, len(cards))
	idx := 0
	for _, card := range cards {
		if card.Vendor == gpu.VendorOther {
			continue
		}
		gpuCard := map[string]any{
			"index":  idx,
			"name":   card.Name,
			"vendor": string(card.Vendor),
		}
		idx++
		// pci_address: "0000:01:00.0" — hardware slot, cross-reboot stable.
		// Used as the durable fingerprint for "is this the same physical card?"
		if card.PCIAddress != "" {
			gpuCard["pci_address"] = card.PCIAddress
		}
		// uuid: vendor-issued durable identifier. NVIDIA "GPU-abc123…" or
		// AMD rocm unique-id. CUDA_VISIBLE_DEVICES / HIP_VISIBLE_DEVICES
		// accept this — the right pin for production workload affinity.
		if card.UUID != "" {
			gpuCard["uuid"] = card.UUID
		}
		// driver_index: nvidia-smi -i N / rocm-smi index. Less stable than
		// uuid (driver re-enumeration shuffles it) but matches what every
		// vendor tool prints — operators see this number in nvidia-smi /
		// nvtop output, so runtime troubleshooting joins on it. Pointer
		// type so a non-vendor-probed card (Apple, Intel iGPU, ghw
		// backfill) omits the field rather than misleading with 0.
		if card.DriverIndex != nil {
			gpuCard["driver_index"] = *card.DriverIndex
		}
		if card.MemoryMiB > 0 {
			totalBytes := card.MemoryMiB * 1024 * 1024
			gpuCard["vram_total"] = map[string]any{
				"value":         totalBytes,
				"display_value": utils.FormatSize(totalBytes),
			}
		} else {
			gpuCard["vram_total"] = map[string]any{"value": 0, "display_value": "N/A"}
		}
		// An unmeasured card omits these keys rather than sending zero.
		// A reader that finds no key knows it wasn't measured; one that
		// finds a zero has to guess, and "0 free" reads as a full card.
		if pool, ok := snapshot.Device(card.UUID, card.PCIAddress); ok {
			total, used, free := pool.DeviceMemoryBytes()
			if free != nil {
				gpuCard["vram_free"] = map[string]any{"value": *free, "display_value": utils.FormatSize(*free)}
			}
			if used != nil {
				gpuCard["vram_used"] = map[string]any{"value": *used, "display_value": utils.FormatSize(*used)}
			}
			if total == nil {
				total = new(card.MemoryMiB * constants.BytesPerMB)
			}
			gpuCard["memory_sample"] = inventory.NewMemorySample(*total, used, snapshot.At)
		}
		gpuInfo = append(gpuInfo, gpuCard)
	}
	info["gpus"] = gpuInfo
	info["gpu_count"] = len(gpuInfo)

	return info
}

func addHealthMemory(info map[string]any, snapshot resources.Snapshot) {
	pool, _ := snapshot.SystemPool()
	total, used, available := pool.MemoryBytes()
	if total == nil || *total <= 0 || available == nil {
		return
	}
	info["memory_total"] = map[string]any{"value": *total, "display_value": utils.FormatSize(*total)}
	info["memory_available"] = map[string]any{"value": *available, "display_value": utils.FormatSize(*available)}
	info["memory_sample"] = inventory.NewMemorySample(*total, used, snapshot.At)
	if used != nil {
		percent := float64(*used) / float64(*total) * 100
		info["memory_used"] = map[string]any{"value": *used, "display_value": utils.FormatSize(*used)}
		info["memory_used_percent"] = map[string]any{"value": percent, "display_value": fmt.Sprintf("%.2f%%", percent)}
	}
}

// rootDisk picks the primary filesystem mount from a disk slice
// returned by inventory.Cache.GetDiskInfo. Prefers the conventional
// root on each platform ("/" on Unix, "C:\" / "C:" on Windows)
// and falls back to the first entry on unusual setups where the
// root mount isn't named.
//
// Returns nil on an empty slice; callers branch to their N/A
// fallback in that case.
func rootDisk(disks []hardware.DiskInfo) *hardware.DiskInfo {
	if len(disks) == 0 {
		return nil
	}
	for i := range disks {
		switch disks[i].Path {
		case "/", "C:\\", "C:":
			return &disks[i]
		}
	}
	return &disks[0]
}
