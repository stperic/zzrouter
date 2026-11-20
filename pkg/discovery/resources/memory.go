package resources

import (
	"fmt"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
)

// MemoryProcess is device memory attributed by the driver to one process.
type MemoryProcess struct {
	PID       int    `json:"pid"`
	MemoryMiB *int64 `json:"memory_mib,omitempty"`
}

// MemoryDevice contains fresh per-device memory, never zero substituted for unknown.
type MemoryDevice struct {
	Unified       bool            `json:"unified,omitempty"`
	UUID          string          `json:"uuid"`
	Index         int             `json:"index"`
	TotalMiB      int64           `json:"total_mib"`
	FreeMiB       int64           `json:"free_mib"`
	Processes     []MemoryProcess `json:"processes"`
	ProcessReason string          `json:"process_reason,omitempty"`
	// UsedMiB is the driver's memory.used; nil means unknown.
	UsedMiB *int64 `json:"used_mib,omitempty"`
}

// AsMemoryDevice requires every field the admission budget depends on.
func (p Pool) AsMemoryDevice() (MemoryDevice, bool) {
	free := p.FreeMiB
	unified := p.Kind == Unified
	if unified {
		free = p.AvailableMiB
		if p.totalBytes != nil && p.availableBytes != nil && *p.availableBytes > *p.totalBytes {
			return MemoryDevice{}, false
		}
	}
	if (p.Kind != GPU && !unified) || p.ID == "" || p.TotalMiB == nil || *p.TotalMiB <= 0 ||
		free == nil || *free < 0 || *free > *p.TotalMiB || (!unified && (p.Index == nil || *p.Index < 0)) {
		return MemoryDevice{}, false
	}
	device := MemoryDevice{
		UUID: p.ID, Unified: unified, TotalMiB: *p.TotalMiB, FreeMiB: *free,
		Processes: make([]MemoryProcess, 0, len(p.Holders)), ProcessReason: p.ProcessReason,
	}
	if unified {
		device.ProcessReason = "unified memory has no per-process GPU attribution"
		return device, true
	}
	device.Index, device.UsedMiB = *p.Index, p.UsedMiB
	for _, holder := range p.Holders {
		device.Processes = append(device.Processes, MemoryProcess{PID: holder.PID, MemoryMiB: holder.MemoryMiB})
	}
	return device, true
}

// AdmissionTimeout bounds each fresh probe, including process attribution.
const AdmissionTimeout = 10 * time.Second

// MemoryDevices validates a snapshot as a whole before admitting against any device.
func (s Snapshot) MemoryDevices() ([]MemoryDevice, error) {
	if s.GPUError != nil {
		return nil, s.GPUError
	}
	var devices []MemoryDevice
	for _, pool := range s.Pools {
		if pool.Kind != Host {
			device, ok := pool.AsMemoryDevice()
			if !ok {
				return nil, fmt.Errorf("GPU memory observation incomplete; no safe admission budget")
			}
			devices = append(devices, device)
		}
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("GPU memory observation unavailable")
	}
	return devices, nil
}

// UnifiedMemoryDevice validates a shared byte budget before converting it to MiB.
func UnifiedMemoryDevice(total, available int64) (MemoryDevice, error) {
	pool := Pool{
		ID: string(Unified), Kind: Unified, TotalMiB: new(total / constants.BytesPerMB),
		AvailableMiB: new(available / constants.BytesPerMB), totalBytes: &total, availableBytes: &available,
	}
	device, ok := pool.AsMemoryDevice()
	if available < 0 || !ok {
		return MemoryDevice{}, fmt.Errorf("unified memory observation incomplete; no safe admission budget")
	}
	return device, nil
}
