package hardware

import (
	"context"
	"fmt"

	"github.com/stperic/zzrouter/pkg/discovery/resources"
)

// DiscoverMemory discovers system memory information
func DiscoverMemory(ctx context.Context) (HardwareInfo, []error) {
	var hardware HardwareInfo
	var errors []error

	total, available, err := resources.ReadRAM(ctx)
	if err != nil {
		errors = append(errors, fmt.Errorf("failed to get memory information: %w", err))
		return hardware, errors
	}

	hardware.TotalRAM = total
	hardware.AvailableRAM = available

	return hardware, errors
}
