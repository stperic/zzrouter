package hardware

const (
	BytesPerGB = 1024 * 1024 * 1024
)

// HardwareInfo contains information about detected hardware
type HardwareInfo struct {
	GPUType      string     `json:"gpu_type"` // "nvidia", "amd", "apple", "intel", "none", "other", "unknown"
	GPUCount     int        `json:"gpu_count"`
	GPUs         []GPUInfo  `json:"gpus,omitempty"`
	CPUInfo      string     `json:"cpu_info,omitempty"`      // Human-readable CPU description, e.g. "Intel Core i5-10400 CPU @ 2.90GHz (6 cores, 12 threads)"
	CPUModel     string     `json:"cpu_model,omitempty"`     // Raw CPU model name without the (N cores, M threads) suffix
	CPUCores     int        `json:"cpu_cores,omitempty"`     // Physical CPU cores
	CPUThreads   int        `json:"cpu_threads,omitempty"`   // Logical CPU threads
	TotalRAM     int64      `json:"total_ram,omitempty"`     // in bytes
	AvailableRAM int64      `json:"available_ram,omitempty"` // in bytes
	DiskInfo     []DiskInfo `json:"disk_info,omitempty"`     // Disk space information
}

// GPUInfo contains detailed information about a specific GPU
type GPUInfo struct {
	Name              string  `json:"name"`
	PCIAddress        string  `json:"pci_address,omitempty"` // canonical "0000:01:00.0" form — used to join ghw's GraphicsCards with nvidia-smi / rocm-smi rows so we don't index-mismatch on multi-GPU boxes where NVML reorders by compute capability
	MemoryGB          float64 `json:"memory_gb"`
	DriverVersion     string  `json:"driver_version,omitempty"`
	CUDAVersion       string  `json:"cuda_version,omitempty"`
	ComputeCapability string  `json:"compute_capability,omitempty"` // e.g., "80", "89", "90"
}

// DiskInfo contains information about disk space for a specific mount point
type DiskInfo struct {
	Path        string  `json:"path"`         // Mount point/path
	Device      string  `json:"device"`       // Device name
	FSType      string  `json:"fstype"`       // File system type
	TotalGB     float64 `json:"total_gb"`     // Total space in GB
	UsedGB      float64 `json:"used_gb"`      // Used space in GB
	AvailableGB float64 `json:"available_gb"` // Available space in GB
	UsedPercent float64 `json:"used_percent"` // Used percentage
}
