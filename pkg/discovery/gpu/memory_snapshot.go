package gpu

import "github.com/stperic/zzrouter/pkg/discovery/resources"

// MemoryProcess is driver-attributed device memory.
type MemoryProcess = resources.MemoryProcess

// MemoryDevice is the admission view of a resource pool.
type MemoryDevice = resources.MemoryDevice

// UnifiedMemoryDevice builds the admission view of a shared RAM pool.
func UnifiedMemoryDevice(total, available int64) (MemoryDevice, error) {
	return resources.UnifiedMemoryDevice(total, available)
}
