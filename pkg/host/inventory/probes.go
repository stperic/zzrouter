package inventory

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/utils"
)

// GatherDisk picks the disk that hosts the supplied models directory and
// returns it as a NodeDisk DTO. Creates the directory if it does not exist
// and logs a warning (not an error) when the cache yields no disks.
//
// The caller supplies modelsDir so this package stays free of config
// dependencies; callers are expected to resolve the directory via their
// own configuration manager.
func GatherDisk(ctx context.Context, modelsDir string, cache *Cache) *NodeDisk {
	_ = os.MkdirAll(modelsDir, 0755)

	absPath, err := filepath.Abs(modelsDir)
	if err != nil {
		slog.Error("Failed to get absolute path for models directory", "directory", err)
		return nil
	}

	diskInfoList := cache.GetDiskInfo(ctx)
	if len(diskInfoList) == 0 {
		slog.Warn("Failed to discover disk info")
		return nil
	}

	// Find the disk whose mount point is the longest prefix of modelsDir.
	var bestMatch *hardware.DiskInfo
	var longestMatch int
	for i := range diskInfoList {
		disk := &diskInfoList[i]
		if strings.HasPrefix(absPath, disk.Path) && len(disk.Path) > longestMatch {
			bestMatch = disk
			longestMatch = len(disk.Path)
		}
	}

	// Fallback: primary mount point (/ on Unix, C:\ on Windows).
	if bestMatch == nil {
		for i := range diskInfoList {
			disk := &diskInfoList[i]
			if disk.Path == "/" || disk.Path == "C:\\" || disk.Path == "C:" {
				bestMatch = disk
				break
			}
		}
	}

	// Last resort: first disk in the list.
	if bestMatch == nil {
		bestMatch = &diskInfoList[0]
	}

	utils.LogDebugf("Models directory '%s' is on disk '%s' with %.1f GB available", modelsDir, bestMatch.Path, bestMatch.AvailableGB)

	return &NodeDisk{
		TotalGB:     bestMatch.TotalGB,
		AvailableGB: bestMatch.AvailableGB,
		UsedPercent: bestMatch.UsedPercent,
	}
}

// GatherMemory projects the OS memory observation into the node wire view.
func GatherMemory(snapshot resources.Snapshot) *NodeMemory {
	pool, _ := snapshot.SystemPool()
	total, used, available := pool.MemoryBytes()
	if total == nil || *total == 0 || available == nil {
		return nil
	}
	memory := &NodeMemory{
		Sample:  NewMemorySample(*total, used, snapshot.At),
		TotalGB: float64(*total) / constants.BytesPerGB, AvailableGB: float64(*available) / constants.BytesPerGB,
	}
	memory.UsedPercent = memory.Sample.UsedPercent()
	return memory
}

// GatherGPU joins live pools to the static inventory by UUID or slot.
func GatherGPU(inv gpu.Inventory, snapshot resources.Snapshot) *NodeGPU {
	hw := hardware.InventoryToHardwareInfo(inv)
	out := &NodeGPU{Count: hw.GPUCount, Type: hw.GPUType}
	if out.Count == 0 {
		return out
	}
	for i, g := range hw.GPUs {
		card := inv.Cards[i]
		detail := NodeGPUDetails{
			Index: i, Name: g.Name, MemoryTotalGB: g.MemoryGB, ComputeCapability: g.ComputeCapability,
		}
		if pool, ok := snapshot.Device(card.UUID, card.PCIAddress); ok {
			total, used, free := pool.DeviceMemoryBytes()
			if free != nil {
				detail.MemoryAvailableGB = new(float64(*free) / constants.BytesPerGB)
			}
			totalBytes := card.MemoryMiB * constants.BytesPerMB
			if total != nil {
				totalBytes = *total
			}
			detail.Sample = NewMemorySample(totalBytes, used, snapshot.At)
		}
		out.GPUs = append(out.GPUs, detail)
	}
	if gpu.IsUnifiedMemory(out.Type) {
		out.UnifiedMemory = true
		if pool, ok := snapshot.SystemPool(); ok && pool.Kind == resources.Unified {
			total, _, available := pool.MemoryBytes()
			if total != nil && available != nil {
				out.MemoryTotalGB = roundGB(float64(*total) / constants.BytesPerGB)
				out.MemoryAvailableGB = roundGB(float64(*available) / constants.BytesPerGB)
				for i := range out.GPUs {
					out.GPUs[i].MemoryTotalGB = 0
				}
				out.GPUs[0].MemoryTotalGB = out.MemoryTotalGB
				out.GPUs[0].MemoryAvailableGB = new(out.MemoryAvailableGB)
			}
		}
		return out
	}
	stats := snapshot.GPUStats(out.Type)
	if stats.TotalMiB != nil {
		out.MemoryTotalGB = float64(*stats.TotalMiB) / 1024
	}
	if stats.FreeMiB != nil && (out.Type != "nvidia" || stats.Count == out.Count) {
		out.MemoryAvailableGB = float64(*stats.FreeMiB) / 1024
	}
	out.UtilizationPercent = stats.UtilizationPct
	return out
}

// roundGB trims a gibibyte figure to one decimal, the precision peers are
// reported at.
func roundGB(v float64) float64 { return math.Round(v*10) / 10 }

// StartupGPUWarnings collects per-vendor Diagnostic strings from the
// cached GPU inventory. Vendors that probed cleanly (or that simply
// aren't present — StateAbsent with no diagnostic) contribute nothing.
// Callers surface this to HTTP clients so operators don't have to grep
// server logs for startup probe failures.
func StartupGPUWarnings(inv gpu.Inventory) []string {
	var warnings []string
	for _, vendor := range []gpu.Vendor{gpu.VendorNVIDIA, gpu.VendorAMD, gpu.VendorApple} {
		det, ok := inv.Vendors[vendor]
		if !ok || det.Diagnostic == "" {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("%s: %s", vendor, det.Diagnostic))
	}
	return warnings
}
