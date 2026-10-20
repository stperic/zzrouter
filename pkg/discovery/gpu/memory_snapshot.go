package gpu

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
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

// MemorySnapshotContext queries memory for admission without using cached metrics.
func MemorySnapshotContext(ctx context.Context) ([]MemoryDevice, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fields := []string{nvidiaFieldUUID, nvidiaFieldIndex, nvidiaFieldMemoryTotalMiB, nvidiaFieldMemoryFreeMiB, nvidiaFieldMemoryUsedMiB}
	output, err := boundedNVIDIAOutput(ctx, "--query-gpu="+strings.Join(fields, ","), "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	devices, err := memoryDevices(parseNVIDIASMIRows(output, fields))
	if err != nil {
		return nil, err
	}
	fields = []string{"gpu_uuid", "pid", "used_gpu_memory"}
	output, err = boundedNVIDIAOutput(ctx, "--query-compute-apps="+strings.Join(fields, ","), "--format=csv,noheader,nounits")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		for i := range devices {
			devices[i].ProcessReason = "driver process attribution unavailable"
		}
		return devices, nil
	}
	for _, row := range parseNVIDIASMIRows(output, fields) {
		pid, err := strconv.Atoi(row["pid"])
		if err != nil || pid <= 0 {
			continue
		}
		process := MemoryProcess{PID: pid}
		if amount, err := strconv.ParseInt(row["used_gpu_memory"], 10, 64); err == nil && amount >= 0 {
			process.MemoryMiB = &amount
		}
		for i := range devices {
			if devices[i].UUID == row["gpu_uuid"] {
				devices[i].Processes = append(devices[i].Processes, process)
			}
		}
	}
	return devices, nil
}

func memoryDevices(rows []nvidiaRow) ([]MemoryDevice, error) {
	if len(rows) == 0 || len(rows) > 64 {
		return nil, fmt.Errorf("GPU memory observation unavailable")
	}
	devices := make([]MemoryDevice, 0, len(rows))
	for _, row := range rows {
		index, indexErr := strconv.Atoi(row[nvidiaFieldIndex])
		total, totalErr := strconv.ParseInt(row[nvidiaFieldMemoryTotalMiB], 10, 64)
		free, freeErr := strconv.ParseInt(row[nvidiaFieldMemoryFreeMiB], 10, 64)
		if indexErr != nil || index < 0 || totalErr != nil || freeErr != nil || total <= 0 || free < 0 || free > total || !isMeaningful(row[nvidiaFieldUUID]) {
			return nil, fmt.Errorf("GPU memory observation incomplete; no safe admission budget")
		}
		device := MemoryDevice{UUID: row[nvidiaFieldUUID], Index: index, TotalMiB: total, FreeMiB: free, Processes: []MemoryProcess{}}
		if used, err := strconv.ParseInt(row[nvidiaFieldMemoryUsedMiB], 10, 64); err == nil && used >= 0 && used <= total {
			device.UsedMiB = &used
		}
		devices = append(devices, device)
	}
	return devices, nil
}

type boundedOutput struct {
	data      []byte
	truncated bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	n := len(data)
	remaining := (64 << 10) - len(b.data)
	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, data...)
	return n, nil
}

func boundedNVIDIAOutput(ctx context.Context, args ...string) ([]byte, error) {
	command := host.CommandContext(ctx, "nvidia-smi", args...)
	var stdout, stderr boundedOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("GPU driver query: %w", err)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("GPU driver response exceeds observation limit")
	}
	return stdout.data, nil
}
