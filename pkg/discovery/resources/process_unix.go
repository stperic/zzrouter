//go:build !windows

package resources

import (
	"context"

	processinfo "github.com/shirou/gopsutil/v4/process"
)

func processParents(ctx context.Context) (map[int32]int32, error) {
	procs, err := processinfo.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	parents := make(map[int32]int32, len(procs))
	for _, proc := range procs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if parent, err := proc.PpidWithContext(ctx); err == nil {
			parents[proc.Pid] = parent
		}
	}
	return parents, nil
}

func sampleParent(ctx context.Context, pid int32) (int32, error) {
	return (&processinfo.Process{Pid: pid}).PpidWithContext(ctx)
}

func processAlive(ctx context.Context, pid int32) (bool, error) {
	return processinfo.PidExistsWithContext(ctx, pid)
}
