package shared

// Shared data types used by both TUI views and non-TUI CLI commands.
// Extracted from cluster_nodes.go, cluster_providers.go, tui_data.go, utils.go
// to break the circular dependency between tuishared/views and package cli.

import "github.com/stperic/zzrouter/pkg/constants"

// NodeInfo represents node information for display
type NodeInfo struct {
	Name          string             `json:"name"`
	IPAddress     string             `json:"ip_address"`
	ClusterRole   string             `json:"cluster_role"`
	HealthStatus  string             `json:"health_status"`
	OS            string             `json:"os"`
	Version       string             `json:"version"`
	Disk          string             `json:"disk"`
	Memory        string             `json:"memory"`
	GPU           string             `json:"gpu"`      // formatted VRAM summary: "95 / 96 GB [1]"
	GPUDesc       string             `json:"gpu_desc"` // legacy summary
	GPUs          []GPUInfo          `json:"gpus,omitempty"`
	UptimeSeconds int                `json:"uptime_seconds"`
	Address       string             `json:"address"`
	Providers     []NodeProviderInfo `json:"providers,omitempty"`
	LastError     string             `json:"last_error,omitempty"`
}

// GPUInfo represents a single GPU device. Index is the PCI-sorted
// stable position assigned by the worker — what zzrouter calls
// "GPU 0", "GPU 1" in TUI display. The provider-runtime identifiers
// live in the optional fields:
//
//   - PCIAddress: hardware slot, "0000:01:00.0". Cross-reboot stable.
//   - UUID:       vendor durable id (NVIDIA "GPU-..." or AMD 16-hex
//     unique-id). CUDA_VISIBLE_DEVICES / HIP_VISIBLE_DEVICES
//     accept this directly.
//   - DriverIndex: int from nvidia-smi -i N / rocm-smi. What every
//     vendor tool prints; less stable across driver
//     re-enumeration but matches operator-facing output.
//     Pointer so absence (Apple unified, Intel iGPU,
//     ghw backfill) doesn't collapse to "GPU 0".
type GPUInfo struct {
	Index       int     `json:"index"`
	Name        string  `json:"name"`
	Vendor      string  `json:"vendor,omitempty"`
	PCIAddress  string  `json:"pci_address,omitempty"`
	UUID        string  `json:"uuid,omitempty"`
	DriverIndex *int    `json:"driver_index,omitempty"`
	VRAMTotalGB float64 `json:"memory_total_gb"`
	// VRAMFreeGB is nil when the node could not measure this card. Nil
	// means unknown; 0 means the card is full. Rendering them the same
	// way is what made a loaded 96 GB card read as "0 / 96 GB".
	VRAMFreeGB *float64 `json:"memory_available_gb"`
}

// NodeProviderInfo represents a provider on a node
type NodeProviderInfo struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Formats string `json:"formats"`
	Cloud   bool   `json:"cloud"`
}

// ProviderInfo represents a provider from the API.
//
// Mode is the legacy discriminator (retained for back-compat during the
// kind-split arc); Kind is the canonical new discriminator. The server
// emits both with identical values until Commit 6 drops Mode. Callers
// should prefer Kind — Mode is read only as a fallback for older
// coordinator builds.
type ProviderInfo struct {
	Name       string   `json:"name"`
	Node       string   `json:"node"`
	Enabled    bool     `json:"enabled"`
	Version    string   `json:"version"`
	Mode       string   `json:"mode"`
	Kind       string   `json:"kind"`
	Formats    []string `json:"formats"`
	FormatNote string   `json:"format_note,omitempty"`

	// LatestVersion is the newest release the provider's upstream has
	// published, as the raw upstream tag so it pairs with Version and with
	// the config pin. Served from the coordinator's cache, so it is empty
	// until the first check completes.
	LatestVersion string `json:"latest_version,omitempty"`
	// VersionStatus compares Version against LatestVersion: newer, same,
	// older or unknown. Empty when no check has run.
	VersionStatus    string `json:"version_status,omitempty"`
	VersionCheckedAt string `json:"version_checked_at,omitempty"`
}

// DeploymentInfo holds parsed deploy/download status for TUI display.
// Status uses constants.Status so comparisons go through the shared
// vocabulary rather than string literals.
type DeploymentInfo struct {
	DownloadID string // Per-node download ID (host/repo/model)
	// DeploymentID addresses the owning deployment for
	// DELETE /deployments/:id/nodes/:node, which is the only call that
	// actually stops a transfer: the download goroutine watches the
	// tracker's cancel func, not its job context, so cancelling the job
	// leaves the transfer running.
	DeploymentID string
	// JobID identifies the job the transfer reports progress under.
	JobID    string
	Node     string
	Model    string
	Provider string
	Status   constants.Status
	Progress string
	Size     string
	Speed    string
	ETA      string
	Error    string
}

// ModelQueryParams holds common parameters for querying models (DRY)
type ModelQueryParams struct {
	Node     string // Node filter (supports wildcards) - named Node internally for API compatibility
	Registry string // Source registry filter (ollama, huggingface)
	Provider string // Provider filter (vllm, ollama, mlx, llamacpp)
	Model    string // Model name filter (supports wildcards)
	SortBy   string // Sort field (node, repo, model, size, date)
}

// DownloadOption represents a provider/node choice for downloading a model.
type DownloadOption struct {
	Index    int
	Node     string
	Provider string
	Display  string
}
