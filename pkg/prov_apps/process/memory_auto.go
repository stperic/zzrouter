package process

import (
	"fmt"
	"math"
	"strconv"

	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

const (
	// DefaultAutoMemorySafetyMarginMiB covers overhead observed above 1.1 GiB.
	DefaultAutoMemorySafetyMarginMiB int64 = 2048
	// DefaultAutoMemorySafetyMarginFraction scales the reserve on larger GPUs.
	DefaultAutoMemorySafetyMarginFraction = 0.05
)

// ResolveAutoMemory mutates parameters in place, replacing auto with the least free-memory fraction of selected GPUs.
func ResolveAutoMemory(budget schema.MemoryBudget, parameters, environment map[string]string, devices []gpu.MemoryDevice) error {
	if budget.Kind != "fraction_total" || parameters[budget.Parameter] != "auto" {
		return nil
	}
	if budget.AutoSafetyMarginMiB < 0 {
		return fmt.Errorf("%w: auto safety margin must not be negative", ErrMemoryBudgetInvalid)
	}
	count := 1
	if value := parameters[budget.DeviceCountParameter]; value != "" {
		var err error
		count, err = strconv.Atoi(value)
		if err != nil || count < 1 || count > 64 {
			return fmt.Errorf("%w: invalid device count", ErrMemoryBudgetInvalid)
		}
	}
	selected, err := memoryDevicesForLaunch(devices, environment, count)
	if err != nil {
		return err
	}
	fraction := 1.0
	for _, device := range selected {
		margin := budget.AutoSafetyMarginMiB
		if margin == 0 {
			margin = max(DefaultAutoMemorySafetyMarginMiB, int64(math.Ceil(float64(device.TotalMiB)*DefaultAutoMemorySafetyMarginFraction)))
		}
		if device.TotalMiB <= 0 || device.FreeMiB > device.TotalMiB || device.FreeMiB <= margin {
			return fmt.Errorf("%w: auto requires valid GPU totals and more than %d MiB free on %s", ErrMemoryObservation, margin, device.UUID)
		}
		fraction = math.Min(fraction, float64(device.FreeMiB-margin)/float64(device.TotalMiB))
	}
	fraction = math.Floor(fraction*10000) / 10000
	if fraction <= 0 {
		return fmt.Errorf("%w: insufficient headroom for auto", ErrMemoryBudgetInvalid)
	}
	parameters[budget.Parameter] = strconv.FormatFloat(fraction, 'f', 4, 64)
	return nil
}
