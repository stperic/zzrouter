package resources

import (
	"context"
	"os"

	"github.com/shirou/gopsutil/v4/cpu"
	processinfo "github.com/shirou/gopsutil/v4/process"
)

// Run identifies a root process, including shared daemon roots.
type Run struct {
	ID  string
	PID int
}

// RunSample holds process-tree observations attributed to the run.
type RunSample struct {
	HostMemoryMiB *int64
	CPUPct        *float64
}

type processSample struct {
	memory  *uint64
	cpuTime *float64
	started int64
}

// ParentPID reads the same parent relationship on every supported OS.
func ParentPID(pid int32) (int32, error) {
	return (&processinfo.Process{Pid: pid}).Ppid()
}

// OwningPID finds the nearest run root and tells callers whether ancestry was readable.
func OwningPID(pid int, root func(int) bool, parent func(int32) (int32, error)) (int, bool) {
	seen := map[int]bool{}
	for pid > 0 && !seen[pid] {
		if root(pid) {
			return pid, true
		}
		seen[pid] = true
		ppid, err := parent(int32(pid))
		if err != nil {
			return 0, false
		}
		pid = int(ppid)
	}
	return 0, pid <= 0
}

func sampleRuns(ctx context.Context, runs []Run, pools []Pool, host bool, list func(context.Context) (map[int32]int32, error)) (map[string]RunSample, map[string]map[int32]processSample) {
	roots := make(map[int]string, len(runs))
	for _, run := range runs {
		if run.PID > 0 && roots[run.PID] == "" {
			roots[run.PID] = run.ID
		}
	}
	parents, err := list(ctx)
	parentOf := func(pid int32) (int32, error) {
		if err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return 0, canceled
			}
			return sampleParent(ctx, pid)
		}
		parent, ok := parents[pid]
		if !ok {
			return 0, os.ErrNotExist
		}
		return parent, nil
	}
	for _, pool := range pools {
		holders := pool.Holders
		for j := range holders {
			holder := &holders[j]
			root, _ := OwningPID(holder.PID, func(pid int) bool { return roots[pid] != "" }, parentOf)
			holder.RunID = roots[root]
		}
	}
	if err != nil {
		return nil, nil
	}
	children := make(map[int32][]int32)
	for pid, parent := range parents {
		children[parent] = append(children[parent], pid)
	}
	samples := make(map[string]RunSample, len(runs))
	trees := make(map[int]map[int32]processSample, len(roots))
	observations := make(map[string]map[int32]processSample, len(runs))
	for _, run := range runs {
		if run.PID <= 0 || ctx.Err() != nil {
			continue
		}
		tree, sampled := trees[run.PID]
		if !sampled {
			tree = processTree(ctx, int32(run.PID), roots, children, host, readProcess)
			trees[run.PID] = tree
		}
		if tree != nil {
			observations[run.ID] = tree
			if host {
				samples[run.ID] = RunSample{HostMemoryMiB: residentMiB(tree)}
			}
		}
	}
	return samples, observations
}

func verifyProcessParents(parents map[int32]int32, started map[int32]int64) map[int32]int32 {
	for child, parent := range parents {
		parents[child] = verifiedParent(parent, started[parent], started[child])
	}
	return parents
}

// Windows leaves an orphan's parent PID in place after that PID is reused.
func verifiedParent(parent int32, parentStarted, childStarted int64) int32 {
	if parentStarted == 0 || childStarted == 0 || parentStarted > childStarted {
		return 0
	}
	return parent
}

func processTree(ctx context.Context, root int32, roots map[int]string, children map[int32][]int32, host bool, read func(context.Context, int32, bool) (processSample, bool)) map[int32]processSample {
	pending := []int32{root}
	seen := make(map[int32]bool)
	tree := make(map[int32]processSample)
	for len(pending) > 0 && ctx.Err() == nil {
		pid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[pid] || (pid != root && roots[int(pid)] != "") {
			continue
		}
		seen[pid] = true
		pending = append(pending, children[pid]...)
		if sample, present := read(ctx, pid, host); present {
			tree[pid] = sample
		}
	}
	if _, present := tree[root]; !present {
		return nil
	}
	return tree
}

func readProcess(ctx context.Context, pid int32, host bool) (processSample, bool) {
	proc := &processinfo.Process{Pid: pid}
	var sample processSample
	if host {
		if memory, err := proc.MemoryInfoWithContext(ctx); err == nil {
			sample.memory = &memory.RSS
		}
	}
	if times, err := proc.TimesWithContext(ctx); err == nil {
		if started, err := proc.CreateTimeWithContext(ctx); err == nil {
			sample.cpuTime, sample.started = new(times.User+times.System), started
		}
	}
	if (host && sample.memory == nil) || sample.cpuTime == nil {
		if alive, err := processAlive(ctx, pid); err == nil && !alive {
			return processSample{}, false
		}
	}
	return sample, true
}

func residentMiB(tree map[int32]processSample) *int64 {
	var total uint64
	for _, proc := range tree {
		if proc.memory == nil {
			return nil
		}
		total += *proc.memory
	}
	return mib(total)
}

func (s *Snapshot) applyCPU(previous *Snapshot) {
	if seconds := s.hostAt.Sub(previous.hostAt).Seconds(); seconds > 0 && s.hostCPU != nil && previous.hostCPU != nil {
		delta := busyCPU(s.hostCPU) - busyCPU(previous.hostCPU)
		for i := range s.Pools {
			if s.Pools[i].Kind == Host && delta >= 0 {
				s.Pools[i].CPUPct = new(100 * delta / seconds)
			}
		}
	}
	seconds := s.processAt.Sub(previous.processAt).Seconds()
	if seconds <= 0 {
		return
	}
	for run, current := range s.processes {
		if delta := cpuDelta(current, previous.processes[run], previous.processAt.UnixMilli()); delta != nil {
			sample := s.Runs[run]
			sample.CPUPct = new(100 * *delta / seconds)
			s.Runs[run] = sample
		}
	}
}

func busyCPU(times *cpu.TimesStat) float64 {
	return times.User + times.Nice + times.System + times.Irq + times.Softirq + times.Steal
}

func cpuDelta(current, previous map[int32]processSample, fromMillis int64) *float64 {
	if len(current) == 0 || len(previous) == 0 {
		return nil
	}
	var total float64
	for pid, proc := range current {
		if proc.cpuTime == nil || proc.started == 0 {
			return nil
		}
		before, existed := previous[pid]
		if !existed || proc.started != before.started {
			if proc.started < fromMillis || (existed && before.started == 0) {
				return nil
			}
			total += *proc.cpuTime
			continue
		}
		if before.cpuTime == nil || *proc.cpuTime < *before.cpuTime {
			return nil
		}
		total += *proc.cpuTime - *before.cpuTime
	}
	return &total
}
