package hardware

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/mem"
)

// DiscoverMemory discovers system memory information
func DiscoverMemory(ctx context.Context) (HardwareInfo, []error) {
	var hardware HardwareInfo
	var errors []error

	memInfo, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		errors = append(errors, fmt.Errorf("failed to get memory information: %w", err))
		return hardware, errors
	}

	hardware.TotalRAM = int64(memInfo.Total)
	hardware.AvailableRAM = int64(memInfo.Available)
	used := int64(memInfo.Used)
	hardware.UsedRAM = &used

	return hardware, errors
}
