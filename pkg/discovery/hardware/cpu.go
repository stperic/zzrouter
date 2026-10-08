package hardware

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/cpu"
)

// DiscoverCPU discovers CPU information using gopsutil
func DiscoverCPU(ctx context.Context) (HardwareInfo, []error) {
	var hardware HardwareInfo
	var errors []error

	// Get CPU info
	info, err := cpu.Info()
	if err != nil {
		errors = append(errors, fmt.Errorf("failed to get CPU information: %w", err))
		return hardware, errors
	}

	if len(info) > 0 {
		cpuInfo := info[0]

		// Get logical CPU count (threads)
		logicalCount, err := cpu.Counts(true)
		if err != nil {
			logicalCount = int(cpuInfo.Cores) // Fallback to cores
		}

		// Get physical CPU count (cores)
		physicalCount, err := cpu.Counts(false)
		if err != nil {
			physicalCount = int(cpuInfo.Cores) // Fallback
		}

		// Populate both string description and numeric values
		hardware.CPUInfo = fmt.Sprintf("%s (%d cores, %d threads)",
			cpuInfo.ModelName,
			physicalCount,
			logicalCount)
		hardware.CPUModel = cpuInfo.ModelName
		hardware.CPUCores = physicalCount
		hardware.CPUThreads = logicalCount
	} else {
		hardware.CPUInfo = "Unknown CPU"
		hardware.CPUModel = ""
		hardware.CPUCores = 0
		hardware.CPUThreads = 0
	}

	return hardware, errors
}
