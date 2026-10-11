package process

import (
	"fmt"
	"math"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

const (
	// DefaultAutoMemorySafetyMarginMiB covers overhead observed above 1.1 GiB.
	DefaultAutoMemorySafetyMarginMiB int64 = 2048
	// DefaultAutoMemorySafetyMarginFraction scales the reserve on larger GPUs.
	DefaultAutoMemorySafetyMarginFraction = 0.05
	// autoFractionScale is the four decimals the engine is given.
	autoFractionScale = 10000
)

// autoFraction is the least free fraction of the selected GPUs after the safety margin, rounded down.
// A GPU with no room beyond the margin is refused like any budget that cannot fit.
func autoFraction(budget schema.MemoryBudget, selected []gpu.MemoryDevice, owners map[int]RunRef) (float64, error) {
	if budget.AutoSafetyMarginMiB < 0 {
		return 0, fmt.Errorf("%w: auto safety margin must not be negative", ErrMemoryBudgetInvalid)
	}
	fraction := 1.0
	for _, device := range selected {
		if device.TotalMiB <= 0 || device.FreeMiB > device.TotalMiB {
			return 0, fmt.Errorf("%w: auto requires valid GPU totals on %s", ErrMemoryObservation, device.UUID)
		}
		margin := safetyMargin(budget, device)
		room := math.Floor(float64(device.FreeMiB-margin)/float64(device.TotalMiB)*autoFractionScale) / autoFractionScale
		if room <= 0 {
			return 0, memoryFitError(budget.Parameter, device, margin+1, owners)
		}
		fraction = math.Min(fraction, room)
	}
	return fraction, nil
}

// safetyMargin is the free memory auto leaves untouched: engines use some outside their own budget
// (lab, vLLM: CUDA context and sampler warm-up, about 2.2 GiB beyond gpu-memory-utilization).
func safetyMargin(budget schema.MemoryBudget, device gpu.MemoryDevice) int64 {
	if budget.AutoSafetyMarginMiB > 0 {
		return budget.AutoSafetyMarginMiB
	}
	return max(DefaultAutoMemorySafetyMarginMiB, int64(math.Ceil(float64(device.TotalMiB)*DefaultAutoMemorySafetyMarginFraction)))
}

// DefaultAutoHeadroomFraction adds 10 percent for allocator variation beyond the estimated need.
const DefaultAutoHeadroomFraction = 0.10

func needFraction(need Need, selected []gpu.MemoryDevice) float64 {
	perDevice := float64(need.WeightsMiB+need.KVMiB)/float64(len(selected)) + float64(need.OverheadMiB)
	required := math.Ceil(perDevice + perDevice*DefaultAutoHeadroomFraction)
	fraction := 0.0
	for _, device := range selected {
		fraction = math.Max(fraction, math.Ceil(required/float64(device.TotalMiB)*autoFractionScale)/autoFractionScale)
	}
	return fraction
}
