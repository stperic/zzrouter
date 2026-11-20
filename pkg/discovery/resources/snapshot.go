// Package resources samples local resource pools without owning cache policy.
package resources

import (
	"context"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Kind identifies the hardware sharing a memory budget.
type Kind string

const (
	GPU     Kind = "gpu"     // GPU is a dedicated device pool.
	Unified Kind = "unified" // Unified represents Apple Silicon's shared RAM.
	Host    Kind = "host"    // Host represents separate system RAM.
)

// Pool holds one measurement per known field and driver attribution where available.
type Pool struct {
	Kind                                                  Kind
	ID, Vendor, PCIAddress                                string
	Index, PressureLevel                                  *int
	TotalMiB, UsedMiB, FreeMiB, AvailableMiB, SwapUsedMiB *int64
	UtilizationPct, PowerW, CPUPct                        *float64
	Holders                                               []Holder
	ProcessReason                                         string
	totalBytes, availableBytes                            *int64
}

// Holder connects a driver process to its run when the ancestry is known.
type Holder struct {
	PID       int
	MemoryMiB *int64
	RunID     string
}

// Snapshot carries observations completed together and any dedicated GPU admission error.
type Snapshot struct {
	Pools             []Pool
	Runs              map[string]RunSample
	At                time.Time
	GPUError          error
	amd               GPUStats
	hostCPU           *cpu.TimesStat
	hostAt, processAt time.Time
	processes         map[string]map[int32]processSample
}

// SystemPool returns the sole RAM pool, including when RAM is shared with the GPU.
func (s Snapshot) SystemPool() (Pool, bool) {
	for _, pool := range s.Pools {
		if pool.Kind == Host || pool.Kind == Unified {
			return pool, true
		}
	}
	return Pool{}, false
}

// MemoryBytes preserves the OS's byte precision for node views.
func (p Pool) MemoryBytes() (total, used, available *int64) {
	if p.totalBytes != nil {
		return p.totalBytes, toBytes(p.UsedMiB), p.availableBytes
	}
	return toBytes(p.TotalMiB), toBytes(p.UsedMiB), toBytes(p.AvailableMiB)
}

// Sample reads fresh data. The previous snapshot is optional and supplies the CPU interval.
func Sample(ctx context.Context, runs []Run, previous *Snapshot) Snapshot {
	return sample(ctx, runs, previous, runtime.GOOS == "darwin" && runtime.GOARCH == "arm64", runtime.GOOS != "darwin")
}

// SampleNode includes the existing aggregate AMD view for node metrics.
func SampleNode(ctx context.Context, previous *Snapshot) Snapshot {
	snapshot := Sample(ctx, nil, previous)
	if runtime.GOOS != "darwin" && ctx.Err() == nil {
		snapshot.amd = liveAMD(ctx, QueryRocmSMI)
	}
	return snapshot
}

func sample(ctx context.Context, runs []Run, previous *Snapshot, unified, nvidia bool) Snapshot {
	if err := ctx.Err(); err != nil {
		return Snapshot{GPUError: err}
	}
	pool, times := systemPool(ctx, unified)
	s := Snapshot{Pools: []Pool{pool}, hostCPU: times, hostAt: utils.Now()}
	if nvidia && ctx.Err() == nil {
		pools, err := NVIDIAPools(ctx)
		s.GPUError = err
		s.Pools = append(s.Pools, pools...)
	}
	if ctx.Err() == nil && len(runs) > 0 {
		s.processAt = utils.Now()
		s.Runs, s.processes = sampleRuns(ctx, runs, s.Pools, !unified, processParents)
	}
	if previous != nil && ctx.Err() == nil {
		s.applyCPU(previous)
	}
	return s
}

func systemPool(ctx context.Context, unified bool) (Pool, *cpu.TimesStat) {
	pool := Pool{Kind: Host, ID: string(Host)}
	if unified {
		pool.Kind, pool.ID, pool.Vendor = Unified, string(Unified), "apple"
		pool.PressureLevel = pressureLevel()
	}
	if total, available, err := ReadRAM(ctx); err == nil {
		pool.totalBytes, pool.availableBytes = &total, &available
		pool.TotalMiB, pool.AvailableMiB = mib(uint64(total)), mib(uint64(available))
	}
	if swap, err := mem.SwapMemoryWithContext(ctx); err == nil {
		pool.SwapUsedMiB = mib(swap.Used)
	}
	if !unified {
		if times, err := cpu.TimesWithContext(ctx, false); err == nil && len(times) == 1 {
			return pool, &times[0]
		}
	}
	return pool, nil
}

// ReadRAM omits gopsutil’s Used, which is derived from Available on the supported platforms.
func ReadRAM(ctx context.Context) (total, available int64, err error) {
	memory, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return 0, 0, err
	}
	return int64(memory.Total), int64(memory.Available), nil
}

func mib(bytes uint64) *int64 { return new(int64(bytes / constants.BytesPerMB)) }

func pressure(value uint32) *int {
	switch value {
	case 1, 2, 4:
		return new(int(value))
	default:
		return nil
	}
}
