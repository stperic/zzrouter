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

// GatherMemory returns aggregate RAM metrics for the node. Returns nil if
// the cache yields no data or reports TotalRAM == 0.
func GatherMemory(ctx context.Context, cache *Cache) *NodeMemory {
	memInfo := cache.GetMemoryInfo(ctx)
	if memInfo == nil || memInfo.TotalRAM == 0 {
		return nil
	}

	totalGB := float64(memInfo.TotalRAM) / constants.BytesPerGB
	availableGB := float64(memInfo.AvailableRAM) / constants.BytesPerGB
	usedGB := totalGB - availableGB
	usedPercent := 0.0
	if totalGB > 0 {
		usedPercent = (usedGB / totalGB) * 100
	}

	return &NodeMemory{
		TotalGB:     totalGB,
		AvailableGB: availableGB,
		UsedPercent: usedPercent,
	}
}

// GatherGPU builds the NodeGPU DTO from a snapshot of pkg/discovery/gpu's
// Inventory. Per-card detail is reshaped from that inventory (names, PCI
// addresses, total memory, compute capability never change at runtime).
//
// Group-level live figures (total/available/utilization) come from
// gpu.LiveMetricsContext which runs one additional nvidia-smi / rocm-smi
// pass to pull the runtime-varying memory-used and utilization fields.
// The group-level aggregate is what schedulers act on; per-card free
// memory is joined on from cache alongside it, so the local node reports
// the same figures a remote worker does. A card the vendor tool couldn't
// measure keeps a nil MemoryAvailableGB rather than a zero, because zero
// free and unmeasured are opposite claims.
//
// Apple Silicon has no SMI-shaped live metrics, so its memory figures are
// taken from system RAM instead — see the unified-memory branch below.
func GatherGPU(ctx context.Context, inv gpu.Inventory, cache *Cache) *NodeGPU {
	hwInfo := hardware.InventoryToHardwareInfo(inv)
	if hwInfo.GPUCount == 0 {
		return &NodeGPU{Count: 0, Type: "none"}
	}

	out := &NodeGPU{
		Count: hwInfo.GPUCount,
		Type:  hwInfo.GPUType,
		GPUs:  make([]NodeGPUDetails, 0, len(hwInfo.GPUs)),
	}
	// Cards keep their durable identifiers in the gpu.Inventory; the
	// reshaped DTO carries only the PCI slot, so index the inventory to
	// recover a UUID for the join.
	uuidByPCI := make(map[string]string, len(inv.Cards))
	for _, card := range inv.Cards {
		if card.PCIAddress != "" && card.UUID != "" {
			uuidByPCI[card.PCIAddress] = card.UUID
		}
	}
	liveCards := cache.GetGPUCardMetrics(ctx)
	byIdentity := gpu.NewCardMetricsIndex(liveCards)

	for i, g := range hwInfo.GPUs {
		card := NodeGPUDetails{
			Index:             i,
			Name:              g.Name,
			MemoryTotalGB:     g.MemoryGB,
			ComputeCapability: g.ComputeCapability,
		}
		if m, ok := byIdentity.ForIdentity(uuidByPCI[g.PCIAddress], g.PCIAddress); ok {
			availGB := float64(m.FreeMemoryMiB) / 1024.0
			card.MemoryAvailableGB = &availGB
		}
		out.GPUs = append(out.GPUs, card)
	}

	// Group-level live aggregate. The per-card poll above already carries
	// every figure it needs, so summing that avoids a second nvidia-smi
	// pass per refresh. AMD has no per-card path yet and still reads the
	// vendor aggregate, which is why the fallback stays.
	if out.Type == "nvidia" && len(liveCards) > 0 {
		var totalMiB, freeMiB int64
		var utilSum float64
		for _, m := range liveCards {
			totalMiB += m.TotalMemoryMiB
			freeMiB += m.FreeMemoryMiB
			utilSum += m.UtilizationPct
		}
		out.MemoryTotalGB = float64(totalMiB) / 1024.0
		out.MemoryAvailableGB = float64(freeMiB) / 1024.0
		out.UtilizationPercent = utilSum / float64(len(liveCards))
		return out
	}

	// LiveMetricsContext returns per-vendor totals; pick whichever vendor
	// matches the node's chosen GPUType so a mixed NVIDIA+AMD box reports
	// the aggregate for whichever vendor hardware.pickBestGPUType favored.
	// Apple Silicon has no discrete VRAM to poll: the GPU addresses system
	// memory directly, so system RAM IS the budget a model is sized
	// against. Reporting zero here read as "this node has no usable GPU
	// memory" and took the whole machine out of scheduling consideration.
	if out.Type == "apple" {
		out.UnifiedMemory = true
		if mem := GatherMemory(ctx, cache); mem != nil {
			// Rounded to the tenth of a GB that peers are reported at, so
			// one node's card doesn't arrive as 67.07192993164062 while
			// every other reads 67.1.
			total, avail := roundGB(mem.TotalGB), roundGB(mem.AvailableGB)
			out.MemoryTotalGB = total
			out.MemoryAvailableGB = avail
			// Only ever stamp ONE card. Consumers sum per-card totals to
			// get a node's VRAM (mesh connector does), and every Apple
			// entry carrying full system RAM would report a two-GPU Mac as
			// having twice the memory it has. The pool is shared, so it
			// belongs to exactly one row.
			if len(out.GPUs) > 0 {
				if out.GPUs[0].MemoryTotalGB == 0 {
					out.GPUs[0].MemoryTotalGB = total
				}
				if out.GPUs[0].MemoryAvailableGB == nil {
					cardAvail := avail
					out.GPUs[0].MemoryAvailableGB = &cardAvail
				}
			}
		}
		return out
	}

	live := gpu.LiveMetricsContext(ctx)
	var m gpu.LiveMetrics
	switch out.Type {
	case "nvidia":
		m = live.NVIDIA
	case "amd":
		m = live.AMD
	}
	if m.Count > 0 {
		out.MemoryTotalGB = float64(m.TotalMemoryMiB) / 1024.0
		out.MemoryAvailableGB = float64(m.FreeMemoryMiB) / 1024.0
		out.UtilizationPercent = m.UtilizationPct
	}
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
