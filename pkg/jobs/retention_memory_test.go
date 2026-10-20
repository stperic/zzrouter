package jobs_test

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
)

// BenchmarkRetainedJobBytes measures live heap after GC, including the registry's
// map, contexts, preallocated rings and per-event sidecars, without subscribers.
func BenchmarkRetainedJobBytes(b *testing.B) {
	cases := []struct {
		name  string
		kind  jobs.Kind
		full  bool
		nodes int
	}{{"sparse/run", jobs.KindRun, false, 0}}
	for _, kind := range []jobs.Kind{jobs.KindDownload, jobs.KindInstall, jobs.KindUpdate, jobs.KindSync, jobs.KindInferenceLog, jobs.KindRun, jobs.KindService} {
		cases = append(cases, struct {
			name  string
			kind  jobs.Kind
			full  bool
			nodes int
		}{"full/" + string(kind), kind, true, 0})
	}
	cases = append(cases, struct {
		name  string
		kind  jobs.Kind
		full  bool
		nodes int
	}{"full/update64", jobs.KindUpdate, true, 64})
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			const count = 128
			for range b.N {
				fc := clocktest.NewFakeClock(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
				registry, err := jobs.NewRegistry(jobs.Config{NodeName: "coordinator", Clock: fc, JanitorInterval: 2 * constants.IdempotencyReplayWindow})
				if err != nil {
					b.Fatal(err)
				}
				// Warm up janitor/context registration so its one-time cost is excluded.
				warm, err := registry.StartDetached(jobs.KindInferenceLog, "", nil)
				if err != nil {
					b.Fatal(err)
				}
				warm.Done()
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				for i := range count {
					h, startErr := registry.StartDetached(tc.kind, "agent", nil)
					if startErr != nil {
						b.Fatal(startErr)
					}
					if tc.full {
						for tick := range tc.kind.DefaultRingPolicy().Size {
							meta := jobs.Meta{"model": fmt.Sprintf("org/model-%d", i), "provider": "engine", "phase": "loading", "step": tick}
							if tc.nodes > 0 {
								nodes := make([]update.NodeUpdate, tc.nodes)
								for n := range nodes {
									nodes[n] = update.NodeUpdate{Node: fmt.Sprintf("worker-%02d", n), State: "succeeded", FromVersion: "1.0.0", OperationID: fmt.Sprintf("operation-%d-%d-%d", i, tick, n)}
								}
								meta["nodes"] = nodes
							}
							h.Meta(meta)
							h.Progress(tick%100, "transferring model weights", jobs.Bytes{Done: int64(tick), Total: 1000})
						}
					}
					h.Done()
				}
				runtime.GC()
				runtime.ReadMemStats(&after)
				b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/count, "B/job")
				runtime.KeepAlive(registry)
				registry.Stop()
			}
		})
	}
}
