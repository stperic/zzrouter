package resources

import (
	"slices"

	"github.com/stperic/zzrouter/pkg/constants"
)

// GPUStats summarizes observed values without substituting a zero for missing data.
type GPUStats struct {
	Count                      int
	TotalMiB, UsedMiB, FreeMiB *int64
	UtilizationPct             *float64
}

// GPUStats aggregates dedicated pools of one vendor.
func (s Snapshot) GPUStats(vendor string) GPUStats {
	var total, used, free int64
	var utilization float64
	var totals, uses, frees, utilizations int
	var result GPUStats
	for _, pool := range s.Pools {
		if pool.Kind != GPU || pool.Vendor != vendor {
			continue
		}
		result.Count++
		if pool.TotalMiB != nil {
			total += *pool.TotalMiB
			totals++
		}
		if pool.UsedMiB != nil {
			used += *pool.UsedMiB
			uses++
		}
		if pool.FreeMiB != nil {
			free += *pool.FreeMiB
			frees++
		}
		if pool.UtilizationPct != nil {
			utilization += *pool.UtilizationPct
			utilizations++
		}
	}
	if result.Count == 0 {
		if vendor == "amd" {
			return s.amd
		}
		return result
	}
	if totals == result.Count {
		result.TotalMiB = &total
	}
	if uses == result.Count {
		result.UsedMiB = &used
	}
	if frees == result.Count {
		result.FreeMiB = &free
	}
	if utilizations > 0 {
		result.UtilizationPct = new(utilization / float64(utilizations))
	}
	return result
}

// Device joins a live pool by driver UUID or PCI slot.
func (s Snapshot) Device(uuid, pciAddress string) (Pool, bool) {
	pciAddress = NormalizePCIAddress(pciAddress)
	var bySlot Pool
	for _, pool := range s.Pools {
		if pool.Kind != GPU {
			continue
		}
		if uuid != "" && pool.ID == uuid {
			return pool, true
		}
		if pciAddress != "" && pool.PCIAddress == pciAddress {
			bySlot = pool
		}
	}
	return bySlot, bySlot.Kind == GPU
}

// DeviceMemoryBytes converts a dedicated driver's MiB readings for the health wire format.
func (p Pool) DeviceMemoryBytes() (total, used, free *int64) {
	return toBytes(p.TotalMiB), toBytes(p.UsedMiB), toBytes(p.FreeMiB)
}

func toBytes(mib *int64) *int64 {
	if mib == nil {
		return nil
	}
	return new(*mib * constants.BytesPerMB)
}

// Clone copies public observations while retaining read-only CPU baselines.
func (s Snapshot) Clone() Snapshot {
	s.Pools = slices.Clone(s.Pools)
	s.amd.UtilizationPct = clone(s.amd.UtilizationPct)
	s.amd.TotalMiB, s.amd.UsedMiB, s.amd.FreeMiB = clone(s.amd.TotalMiB), clone(s.amd.UsedMiB), clone(s.amd.FreeMiB)
	for i := range s.Pools {
		p := &s.Pools[i]
		p.Index, p.PressureLevel = clone(p.Index), clone(p.PressureLevel)
		p.TotalMiB, p.UsedMiB, p.FreeMiB = clone(p.TotalMiB), clone(p.UsedMiB), clone(p.FreeMiB)
		p.AvailableMiB, p.SwapUsedMiB = clone(p.AvailableMiB), clone(p.SwapUsedMiB)
		p.UtilizationPct, p.PowerW, p.CPUPct = clone(p.UtilizationPct), clone(p.PowerW), clone(p.CPUPct)
		p.totalBytes, p.availableBytes = clone(p.totalBytes), clone(p.availableBytes)
		p.Holders = slices.Clone(p.Holders)
		for j := range p.Holders {
			p.Holders[j].MemoryMiB = clone(p.Holders[j].MemoryMiB)
		}
	}
	if s.Runs != nil {
		runs := make(map[string]RunSample, len(s.Runs))
		for id, run := range s.Runs {
			runs[id] = RunSample{HostMemoryMiB: clone(run.HostMemoryMiB), CPUPct: clone(run.CPUPct)}
		}
		s.Runs = runs
	}
	return s
}

func clone[T any](value *T) *T {
	if value == nil {
		return nil
	}
	return new(*value)
}
