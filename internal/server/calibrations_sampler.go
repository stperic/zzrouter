package server

import (
	"context"
	"math"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/calibration"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

func (r *nodeCalibration) Sample(ctx context.Context) calibration.Snapshot {
	mgr := r.server.providers.appMgr
	var roots []resources.Run
	for _, run := range mgr.Instances().List() {
		if pid := run.GetProcessID(); pid > 0 && !run.GetStatus().IsTerminal() {
			roots = append(roots, resources.Run{ID: run.ID, PID: pid})
		}
	}

	daemons := map[string][]*instance.Instance{}
	snapshot := calibration.Snapshot{Runs: make(map[string]calibration.RunSample, len(r.targets))}
	for id, target := range r.targets {
		run, revision, _ := r.currentRun(id)
		var sample calibration.RunSample
		if revision == target.TargetRevision && run != nil {
			cfg, ok := mgr.ProviderConfig(run.Provider)
			if ok && prov_apps.Lifecycle(cfg) == prov_apps.LifecycleServed {
				daemons[cfg.Name] = append(daemons[cfg.Name], run)
			}
			obs := health.ReadObservation(ctx, r.server.httpClient, run, r.probes[id])
			sample.Running, sample.Waiting = observationCount(obs.Running), observationCount(obs.Waiting)
			if fraction := obs.KVCacheUsage; fraction != nil && *fraction >= 0 && *fraction <= 1 {
				sample.KVCacheUsage = fraction
			}
		}
		snapshot.Runs[id] = sample
	}

	owners := make(map[string][]string, len(daemons))
	for provider, runs := range daemons {
		pid, loaded := mgr.CalibrationDaemon(ctx, runs[0])
		if pid == 0 {
			continue
		}
		root := "daemon:" + provider
		roots = append(roots, resources.Run{ID: root, PID: pid})
		owners[root] = daemonRunOwners(runs, loaded)
	}
	sampled := resources.Sample(ctx, roots, r.previous)
	if ctx.Err() == nil {
		r.previous = &sampled
	}
	for id, sample := range snapshot.Runs {
		_, revision, admitted := r.currentRun(id)
		sample.TargetRevision = revision
		if revision != "" {
			sample.RequestsAdmitted = new(admitted)
		}
		snapshot.Runs[id] = sample
	}
	snapshot.Pools = calibrationPools(sampled, snapshot.Runs, owners)
	return snapshot
}

func daemonRunOwners(runs []*instance.Instance, loaded []string) []string {
	if len(loaded) != len(runs) {
		return nil
	}
	names := make(map[string]string, len(runs))
	for _, run := range runs {
		if run.Weights().Backend != runs[0].Weights().Backend {
			return nil
		}
		names[strings.ToLower(run.Model)] = run.ID
	}
	owners := make([]string, 0, len(loaded))
	for _, model := range loaded {
		id, found := names[model]
		if !found {
			return nil
		}
		delete(names, model)
		owners = append(owners, id)
	}
	slices.Sort(owners)
	return owners
}

func observationCount(value *float64) *int {
	if value == nil || *value < 0 || *value >= float64(math.MaxInt) || math.Trunc(*value) != *value {
		return nil
	}
	return new(int(*value))
}

func calibrationPools(sampled resources.Snapshot, runs map[string]calibration.RunSample, daemons map[string][]string) []calibration.PoolSample {
	pools := make([]calibration.PoolSample, 0, len(sampled.Pools))
	hostMemory := make(map[string]*int64, len(runs))
	for root, observed := range sampled.Runs {
		ids, daemon := daemons[root]
		if !daemon {
			ids = []string{root}
		}
		for _, id := range ids {
			if sample, listed := runs[id]; listed {
				sample.CPUPct = observed.CPUPct
				runs[id], hostMemory[id] = sample, observed.HostMemoryMiB
			}
		}
	}
	for _, pool := range sampled.Pools {
		view := calibration.PoolSample{ID: pool.ID, Kind: string(pool.Kind), TotalMiB: pool.TotalMiB,
			UsedMiB: pool.UsedMiB, FreeMiB: pool.FreeMiB, AvailableMiB: pool.AvailableMiB, SwapUsedMiB: pool.SwapUsedMiB,
			UtilizationPct: pool.UtilizationPct, PowerW: pool.PowerW, CPUPct: pool.CPUPct}
		if pool.Vendor != "" {
			view.Vendor = new(pool.Vendor)
		}
		if pool.Kind == resources.Host {
			view.HostMemoryMiB = hostMemory
		}
		if pool.Holders != nil {
			view.Holders = make([]calibration.Holder, 0, len(pool.Holders))
			for _, process := range pool.Holders {
				h := calibration.Holder{PID: process.PID, MemoryMiB: process.MemoryMiB}
				if ids, daemon := daemons[process.RunID]; daemon {
					if len(ids) == 1 {
						h.RunID = new(ids[0])
					} else {
						h.ListedRuns = ids
					}
				} else if process.RunID != "" {
					h.RunID = new(process.RunID)
				}
				view.Holders = append(view.Holders, h)
			}
		}
		pools = append(pools, view)
	}
	return pools
}
