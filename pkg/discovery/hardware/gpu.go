package hardware

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	gpuProbe "github.com/stperic/zzrouter/pkg/discovery/gpu"
)

// DiscoverGPUs produces HardwareInfo by reshaping the full GPU
// Inventory collected by pkg/discovery/gpu. The inventory runs
// every GPU subprocess (nvidia-smi, rocm-smi, ghw, platform-
// specific probes) at most once per call — DiscoverGPUs is the
// runtime-discovery view of the same data install preflight
// consumes as gpu.Detection.
//
// Before 2026-04, this package maintained its own parallel
// nvidia-smi / rocm-smi shell-out paths alongside an index-
// based ghw scan. The parallel paths could disagree (one side
// would report driver OK while the other reported hardware
// with no driver), and every cluster startup invoked each
// subprocess twice. Consolidating into gpu.InventoryContext
// removes the duplication and makes both views of the world
// come from a single source of truth.
//
// The errors return is retained for API compatibility with
// every other Discover* function in this package, but
// InventoryContext never surfaces errors — a catastrophic
// subprocess failure lands in Detection.Diagnostic on the
// per-vendor map, and a healthy no-GPU box produces a
// HardwareInfo with GPUType "none". DiscoverGPUs always
// returns a nil errors slice.
func DiscoverGPUs(ctx context.Context) (HardwareInfo, []error) {
	return InventoryToHardwareInfo(gpuProbe.InventoryContext(ctx)), nil
}

// InventoryToHardwareInfo is the pure reshape from gpu.Inventory
// to HardwareInfo. No subprocess, no ctx. Callers that cache the
// inventory at server startup (to avoid repeat nvidia-smi /
// rocm-smi subprocess calls on hot paths) use this directly to
// serve requests from the cache; DiscoverGPUs is the live-probe
// wrapper that runs InventoryContext first.
func InventoryToHardwareInfo(inv gpuProbe.Inventory) HardwareInfo {
	var hw HardwareInfo
	hw.GPUCount = len(inv.Cards)
	if hw.GPUCount == 0 {
		hw.GPUType = "none"
		return hw
	}
	hw.GPUType = pickBestGPUType(inv.Cards)
	hw.GPUs = make([]GPUInfo, 0, len(inv.Cards))
	for _, c := range inv.Cards {
		hw.GPUs = append(hw.GPUs, cardToGPUInfo(c))
	}
	return hw
}

// gpuTypePriority is the legacy priority rule zzRouter uses to
// pick a single GPUType string for a mixed-vendor node. NVIDIA
// wins over AMD wins over Apple wins over Intel wins over
// unknown. Matches the ordering provider config variant filters
// assume.
var gpuTypePriority = map[gpuProbe.Vendor]int{
	gpuProbe.VendorNVIDIA: 5,
	gpuProbe.VendorAMD:    4,
	gpuProbe.VendorApple:  3,
	gpuProbe.VendorIntel:  2,
	gpuProbe.VendorOther:  1,
}

// pickBestGPUType chooses the highest-priority vendor seen in
// the card slice and returns its lowercase tag. Falls back to
// "other" for an empty vendor or a tag the priority table
// doesn't recognize — same behavior as the pre-refactor loop.
func pickBestGPUType(cards []gpuProbe.Card) string {
	best := 0
	winner := "other"
	for _, c := range cards {
		p, ok := gpuTypePriority[c.Vendor]
		if !ok {
			continue
		}
		if p > best {
			best = p
			winner = string(c.Vendor)
		}
	}
	return winner
}

// cardToGPUInfo reshapes one gpu.Card into the legacy GPUInfo
// contract. Unit conversion (MiB → GB) lives here so Card's
// units stay clean and the adapter owns all compat glue.
//
// For Apple cards the compute capability is derived from the
// chip generation — M1=10, M2=20, M3=30, M4=40 — the same rule
// the old discoverAppleSiliconGPUs followed. The gpu package
// leaves Card.ComputeCapability empty on Apple because Apple
// Silicon has no nvidia-style compute-cap string to report.
//
// GPUInfo.CUDAVersion carries the node-wide max-supported CUDA
// runtime that pkg/discovery/gpu parses from nvidia-smi's banner.
// Non-NVIDIA cards leave it empty. llama.cpp's install-variant
// selector filters on this value via variant.cuda_min, so a
// populated string here is what unlocks the CUDA build on
// CUDA-capable Windows/Linux hosts.
func cardToGPUInfo(c gpuProbe.Card) GPUInfo {
	gi := GPUInfo{
		Name:              c.Name,
		PCIAddress:        c.PCIAddress,
		DriverVersion:     c.DriverVersion,
		ComputeCapability: c.ComputeCapability,
		CUDAVersion:       c.CUDAVersion,
	}
	if c.MemoryMiB > 0 {
		gi.MemoryGB = float64(c.MemoryMiB) / 1024.0
	}
	if c.Vendor == gpuProbe.VendorApple && gi.ComputeCapability == "" {
		gi.ComputeCapability = appleComputeCapFromName(c.Name)
	}
	return gi
}

var appleMSeriesNumber = regexp.MustCompile(`(?i)\bm([1-9]\d*)\b`)

// appleComputeCapFromName extracts the M-series number from an
// Apple chip name and returns "N0" where N is the chip
// generation. "Apple M1 Pro" → "10", "Apple M3 Max" → "30".
// Returns "" when the chip number can't be parsed; callers
// treat empty as "unknown", matching the old contract.
func appleComputeCapFromName(name string) string {
	m := appleMSeriesNumber.FindStringSubmatch(name)
	if len(m) < 2 {
		return ""
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d0", n)
}
