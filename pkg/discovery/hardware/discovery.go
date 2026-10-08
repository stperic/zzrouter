package hardware

import (
	"context"
)

// Discovery handles hardware discovery
type Discovery struct{}

// NewDiscovery creates a new hardware discovery instance
func NewDiscovery() *Discovery {
	return &Discovery{}
}

// DiscoverAll discovers all hardware information
func (d *Discovery) DiscoverAll(ctx context.Context) (HardwareInfo, []error) {
	var hardware HardwareInfo
	var errors []error

	// Discover GPU information
	gpuInfo, gpuErrors := DiscoverGPUs(ctx)
	hardware = gpuInfo
	errors = append(errors, gpuErrors...)

	// Discover memory information
	memInfo, memErrors := DiscoverMemory(ctx)
	hardware.TotalRAM = memInfo.TotalRAM
	hardware.AvailableRAM = memInfo.AvailableRAM
	errors = append(errors, memErrors...)

	// Discover CPU information
	cpuInfo, cpuErrors := DiscoverCPU(ctx)
	hardware.CPUInfo = cpuInfo.CPUInfo
	errors = append(errors, cpuErrors...)

	// Discover disk information
	diskInfo, diskErrors := DiscoverDisk(ctx)
	hardware.DiskInfo = diskInfo
	errors = append(errors, diskErrors...)

	return hardware, errors
}
