package prov_apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/require"
)

func TestFailedEvictionPublishesServingTransition(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "queued"}[queued], func(t *testing.T) {
			f := newQueueFixture(t)
			makeNodeScoped(t, f.mgr)
			reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
			require.NoError(t, err)
			t.Cleanup(reg.Stop)
			f.mgr.jobsRegistry = reg
			f.mgr.idleEvictionDefault = true
			victim := instance.NewInstance("victim", "other", "weights", 0, time.Hour, 10)
			victim.MarkRunning(false, nil)
			victim.SetProcessInfo(9000001, 0, 0, nil)
			victim.TrackGoroutine()
			defer victim.GoroutineDone()
			require.NoError(t, f.mgr.instances.Register(victim))
			budget := f.mgr.stopBudget
			f.mgr.stopBudget = func(string) time.Duration { return -time.Nanosecond }
			defer func() { f.mgr.stopBudget = budget }()
			f.mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
				return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 1000, Processes: []gpu.MemoryProcess{{PID: 9000001, MemoryMiB: new(int64(5000))}}}}, nil
			}
			publicationErr := errors.New("catalog unavailable")
			published := make(chan struct{}, 1)
			f.mgr.publishCatalog = func() error { published <- struct{}{}; return publicationErr }
			if queued {
				f.mgr.memoryAdmission <- struct{}{}
			}
			result, err := f.mgr.EnsureRun(t.Context(), EnsureRequest{Provider: "generic"})
			if queued {
				require.NoError(t, err)
				<-f.mgr.memoryAdmission
				ev := subscribeTerminal(t, reg, result.Run.StreamJobID, 5*time.Second)
				require.Equal(t, string(httperr.CodeCatalogPublicationFailed), ev.Meta["code"])
				require.NotContains(t, ev.Meta, "evicted_runs", "an unconfirmed stop is not reclaimed memory")
				require.Empty(t, result.Run.EvictedRuns())
			} else {
				var fit *process.MemoryFitError
				require.ErrorAs(t, err, &fit)
				require.Equal(t, int64(1000), fit.FreeMiB, "failed stop must not credit memory")
				var publication *CatalogPublicationError
				require.ErrorAs(t, err, &publication)
				require.ErrorIs(t, publication, publicationErr)
				var failed *runStartFailure
				require.ErrorAs(t, err, &failed)
				require.Empty(t, failed.run.EvictedRuns())
			}
			require.Equal(t, instance.StatusStopping, victim.GetStatus())
			require.True(t, victim.StopUnconfirmed.Load())
			select {
			case <-published:
			default:
				t.Fatal("failed model eviction lost catalog publication")
			}
		})
	}
}

func TestInstallSmokeAdmissionFailurePublishesAfterUnlock(t *testing.T) {
	for _, mode := range []string{"incomplete_weights", "retry_cancel"} {
		t.Run(mode, func(t *testing.T) {

			isolateProviderRoot(t)
			f := newQueueFixture(t)
			modelRoot := t.TempDir()
			previousRoot, err := modelregistry.GetModelsRootDir()
			require.NoError(t, err)
			modelregistry.SetModelsRootDirOverride(modelRoot)
			t.Cleanup(func() { modelregistry.SetModelsRootDirOverride(previousRoot) })
			dir := filepath.Join(modelRoot, "org", "broken")
			require.NoError(t, os.MkdirAll(dir, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "weights.gguf"), []byte("GGUF"), 0600))
			require.NoError(t, integrity.WriteManifest(dir, &integrity.ModelManifest{Version: 1, Files: []integrity.FileChecksum{{RelativePath: "weights.gguf", Size: 1024}}}))
			fresh := &config.AppsConfig{}
			svc, ok := f.mgr.AppsConfig().LookupApp("generic")
			require.True(t, ok)
			runtime := *svc.Runtime
			runtime.Execution.Args = []string{"--model", config.PlaceholderModelPath}
			svc.Runtime = &runtime
			require.NoError(t, fresh.AddApp("generic", svc))
			other := svc
			other.Name = "other"
			require.NoError(t, fresh.AddApp("other", other))
			f.mgr.ReloadConfig(fresh)
			reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
			require.NoError(t, err)
			t.Cleanup(reg.Stop)
			f.mgr.jobsRegistry = reg
			f.mgr.installs.jobs = reg
			f.mgr.idleEvictionDefault = true
			victim := instance.NewInstance("victim", "other", "live-model", 0, time.Hour, 10)
			victim.MarkRunning(false, nil)
			victim.SetProcessInfo(9000001, 0, 0, nil)
			require.NoError(t, f.mgr.instances.Register(victim))
			var cancelSmoke context.CancelFunc
			f.mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
				free := int64(1000)
				if victim.GetStatus() == instance.StatusStopped {
					free = 6000
					if mode == "retry_cancel" {
						cancelSmoke()
					}
				}
				return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: free, Processes: []gpu.MemoryProcess{{PID: 9000001, MemoryMiB: new(int64(5000))}}}}, nil
			}
			coord := f.mgr.installs
			busyReturned := false
			if mode == "retry_cancel" {
				originalSchema := f.mgr.providerSchema
				f.mgr.providerSchema = func(name string) (*schema.ProviderSchema, error) {
					if name == "generic" && !busyReturned {
						busyReturned = true
						return nil, ErrAtCapacity
					}
					return originalSchema(name)
				}
				originalSmoke := coord.smoke
				coord.smoke = func(ctx context.Context, provider, runtime, model string) error {
					child, cancel := context.WithCancel(ctx)
					defer cancel()
					cancelSmoke = cancel
					return originalSmoke(child, provider, runtime, model)
				}
			}
			publicationErr := errors.New("publication unavailable")
			published := false
			f.mgr.publishCatalog = func() error {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				unlock, lockErr := coord.acquireProvider(ctx, "generic")
				if lockErr != nil {
					return lockErr
				}
				unlock()
				lock := fsroot.NewLockFile("generic")
				if lockErr = lock.Lock(); lockErr != nil {
					return lockErr
				}
				lock.Unlock()
				require.Empty(t, f.mgr.memoryAdmission)
				published = true
				return publicationErr
			}
			rolledBack := false
			plan := &install.Plan{Provider: "generic", Action: "install", Recipe: &install.RecipeSnapshot{}, Rollback: func() error { rolledBack = true; return nil }}
			h := coord.openJob("install", "generic")
			require.NotNil(t, h)
			err = coord.runResolvedPlan(h, "generic", plan, install.PlanOptions{Smoke: &install.SmokeOptions{Model: "org/broken"}}, -1, func() error { t.Error("failed smoke finalized installation"); return nil })
			if mode == "retry_cancel" {
				require.True(t, busyReturned)
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, process.ErrModelIncomplete)
			}
			require.ErrorIs(t, err, publicationErr)
			require.True(t, published, "smoke effects must publish after installer unlock")
			require.True(t, rolledBack)
			require.Equal(t, instance.StatusStopped, victim.GetStatus())
			h.Fail(err)
			event, getErr := reg.Get(h.ID())
			require.NoError(t, getErr)
			require.Equal(t, "failed", event.Meta["smoke_status"])
			require.Equal(t, string(httperr.CodeCatalogPublicationFailed), event.Meta["code"])
			require.Len(t, event.Meta["evicted_runs"], 1)
			require.Empty(t, f.mgr.instances.List(), "a pre-watcher failure invents no retained run")

		})
	}
}
