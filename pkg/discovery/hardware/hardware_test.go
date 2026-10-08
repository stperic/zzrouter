package hardware

import (
	"context"
	"testing"
)

func TestDiscoverAll(t *testing.T) {
	ctx := context.Background()
	discovery := NewDiscovery()

	hardware, errors := discovery.DiscoverAll(ctx)

	// We don't assert on the actual results since they depend on the environment
	t.Logf("GPU Type: %s, Count: %d", hardware.GPUType, hardware.GPUCount)
	t.Logf("Total RAM: %d bytes", hardware.TotalRAM)
	t.Logf("Available RAM: %d bytes", hardware.AvailableRAM)
	t.Logf("CPU Info: %s", hardware.CPUInfo)
	t.Logf("Disk Volumes: %d", len(hardware.DiskInfo))
	t.Logf("Errors: %d", len(errors))

	for _, err := range errors {
		t.Logf("Error: %v", err)
	}
}

func TestDiscoverGPUs(t *testing.T) {
	ctx := context.Background()
	hardware, errors := DiscoverGPUs(ctx)

	t.Logf("GPU Type: %s", hardware.GPUType)
	t.Logf("GPU Count: %d", hardware.GPUCount)
	t.Logf("GPUs: %d", len(hardware.GPUs))

	for i, gpu := range hardware.GPUs {
		t.Logf("GPU %d: %s (%.1f GB)", i+1, gpu.Name, gpu.MemoryGB)
	}

	if len(errors) > 0 {
		t.Logf("Errors: %v", errors)
	}
}

func TestDiscoverCPU(t *testing.T) {
	ctx := context.Background()
	hardware, errors := DiscoverCPU(ctx)

	t.Logf("CPU Info: %s", hardware.CPUInfo)

	if len(errors) > 0 {
		t.Errorf("DiscoverCPU returned errors: %v", errors)
	}

	if hardware.CPUInfo == "" {
		t.Error("CPU Info is empty")
	}
}

func TestDiscoverMemory(t *testing.T) {
	ctx := context.Background()
	hardware, errors := DiscoverMemory(ctx)

	t.Logf("Total RAM: %d bytes (%.2f GB)", hardware.TotalRAM, float64(hardware.TotalRAM)/BytesPerGB)
	t.Logf("Available RAM: %d bytes (%.2f GB)", hardware.AvailableRAM, float64(hardware.AvailableRAM)/BytesPerGB)

	if len(errors) > 0 {
		t.Errorf("DiscoverMemory returned errors: %v", errors)
	}

	if hardware.TotalRAM == 0 {
		t.Error("Total RAM is 0")
	}
}

func TestDiscoverDisk(t *testing.T) {
	ctx := context.Background()
	disks, errors := DiscoverDisk(ctx)

	t.Logf("Found %d disk volumes", len(disks))

	for i, disk := range disks {
		t.Logf("Disk %d: %s (%s) - %.1f GB total, %.1f GB available (%.1f%% used)",
			i+1, disk.Path, disk.FSType, disk.TotalGB, disk.AvailableGB, disk.UsedPercent)
	}

	if len(errors) > 0 {
		t.Logf("Errors: %v", errors)
	}

	if len(disks) == 0 {
		t.Error("No disks discovered")
	}
}

func TestHardwareTypes(t *testing.T) {
	// Test that types are properly defined
	var hw HardwareInfo
	hw.GPUType = "nvidia"
	hw.GPUCount = 2
	hw.TotalRAM = 16 * BytesPerGB
	hw.AvailableRAM = 8 * BytesPerGB
	hw.CPUInfo = "Test CPU"

	if hw.GPUType != "nvidia" {
		t.Errorf("GPUType mismatch")
	}
	if hw.GPUCount != 2 {
		t.Errorf("GPUCount mismatch")
	}
	if hw.TotalRAM != 16*BytesPerGB {
		t.Errorf("TotalRAM mismatch")
	}
	if hw.AvailableRAM != 8*BytesPerGB {
		t.Errorf("AvailableRAM mismatch")
	}
	if hw.CPUInfo != "Test CPU" {
		t.Errorf("CPUInfo mismatch")
	}
}

func TestGPUInfo(t *testing.T) {
	gpu := GPUInfo{
		Name:          "NVIDIA RTX 4090",
		MemoryGB:      24.0,
		DriverVersion: "535.54.03",
		CUDAVersion:   "12.2",
	}

	if gpu.Name == "" {
		t.Error("GPU Name is empty")
	}
	if gpu.MemoryGB <= 0 {
		t.Error("GPU Memory is invalid")
	}
	if gpu.DriverVersion == "" {
		t.Error("GPU DriverVersion is empty")
	}
	if gpu.CUDAVersion == "" {
		t.Error("GPU CUDAVersion is empty")
	}
}

func TestDiskInfo(t *testing.T) {
	disk := DiskInfo{
		Path:        "/",
		Device:      "/dev/sda1",
		FSType:      "ext4",
		TotalGB:     500.0,
		UsedGB:      300.0,
		AvailableGB: 200.0,
		UsedPercent: 60.0,
	}

	if disk.Path == "" {
		t.Error("Disk Path is empty")
	}
	if disk.Device == "" {
		t.Error("Disk Device is empty")
	}
	if disk.FSType == "" {
		t.Error("Disk FSType is empty")
	}
	if disk.TotalGB <= 0 {
		t.Error("Disk TotalGB is invalid")
	}

	expectedUsedPercent := (disk.UsedGB / disk.TotalGB) * 100
	if disk.UsedPercent < expectedUsedPercent-1 || disk.UsedPercent > expectedUsedPercent+1 {
		t.Logf("Warning: UsedPercent (%.1f) doesn't match calculated value (%.1f)",
			disk.UsedPercent, expectedUsedPercent)
	}

	t.Logf("Disk: %s - %.1f GB total, %.1f GB available", disk.Path, disk.TotalGB, disk.AvailableGB)
}

func TestBytesPerGB(t *testing.T) {
	expected := int64(1024 * 1024 * 1024)
	if BytesPerGB != expected {
		t.Errorf("BytesPerGB = %d, expected %d", BytesPerGB, expected)
	}

	// Test conversion
	gb := 16.0
	bytes := int64(gb * float64(BytesPerGB))
	t.Logf("%.1f GB = %d bytes", gb, bytes)

	if bytes != 17179869184 {
		t.Errorf("Conversion failed: %.1f GB should be 17179869184 bytes, got %d", gb, bytes)
	}
}
