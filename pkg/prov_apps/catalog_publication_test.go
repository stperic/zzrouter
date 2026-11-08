package prov_apps

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/require"
)

func makeNodeScoped(t *testing.T, m *ProviderAppManager) {
	t.Helper()
	fresh := &config.AppsConfig{}
	m.AppsConfig().RangeApps(func(name string, svc config.ServiceConfig) bool {
		svc.RunScope, svc.Protocol, svc.Capabilities = config.RunScopeNode, "", nil
		require.NoError(t, fresh.AddApp(name, svc))
		return true
	})
	m.ReloadConfig(fresh)
}

func TestNodeRunRealLifecycleWithoutPublication(t *testing.T) {
	reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(reg.Stop)
	r := newContractRig(t, false, WithJobsRegistry(reg), WithCatalogPublication(func() error {
		t.Error("isolated node lifecycle attempted model publication")
		return errors.New("reverse path blocked")
	}))
	makeNodeScoped(t, r.m)
	result, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic"})
	require.NoError(t, err)
	require.Equal(t, jobs.PhaseDone, subscribeTerminal(t, reg, result.Run.StreamJobID, 5*time.Second).Phase)
	second, err := r.m.RestartInstance(t.Context(), result.Run.ID)
	require.NoError(t, err)
	require.Equal(t, jobs.PhaseDone, subscribeTerminal(t, reg, second.StreamJobID, 5*time.Second).Phase)
	require.NoError(t, r.m.StopInstance(t.Context(), second.ID))
}

func TestNodeAdmissionFailurePublishesEvictions(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "queued"}[queued], func(t *testing.T) {
			f := newQueueFixture(t)
			makeNodeScoped(t, f.mgr)
			reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
			require.NoError(t, err)
			t.Cleanup(reg.Stop)
			f.mgr.jobsRegistry = reg
			f.mgr.idleEvictionDefault = true
			idle := instance.NewInstance("idle", "generic", "weights", 0, time.Hour, 10)
			idle.MarkRunning(false, nil)
			idle.SetProcessInfo(9000001, 0, 0, nil)
			require.NoError(t, f.mgr.instances.Register(idle))
			f.mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
				free := int64(1000)
				if idle.GetStatus() == instance.StatusStopped {
					free = 6000
				}
				return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: free, Processes: []gpu.MemoryProcess{{PID: 9000001, MemoryMiB: new(int64(5000))}}}}, nil
			}
			preparationFailure := errors.New("preparation refused after eviction")
			f.mgr.localizeParams = func(_, _, _ string, params map[string]string, _ map[string]schema.ParamShape) (Localized, error) {
				if idle.GetStatus() == instance.StatusStopped {
					return Localized{}, preparationFailure
				}
				return Localized{Params: params}, nil
			}
			published := make(chan struct{}, 1)
			f.mgr.publishCatalog = func() error {
				require.Empty(t, f.mgr.memoryAdmission, "publication must run after admission unlocks")
				published <- struct{}{}
				return errors.New("publication unavailable")
			}
			if queued {
				f.mgr.memoryAdmission <- struct{}{}
			}
			result, err := f.mgr.EnsureRun(t.Context(), EnsureRequest{Provider: "generic"})
			if queued {
				require.NoError(t, err)
				<-f.mgr.memoryAdmission
				ev := subscribeTerminal(t, reg, result.Run.StreamJobID, 5*time.Second)
				require.Equal(t, jobs.PhaseFailed, ev.Phase)
				require.Equal(t, string(httperr.CodeCatalogPublicationFailed), ev.Meta["code"])
				require.Contains(t, ev.Err, preparationFailure.Error())
			} else {
				require.ErrorIs(t, err, preparationFailure)
				var publication *CatalogPublicationError
				require.ErrorAs(t, err, &publication)
				require.Equal(t, instance.StatusFailed, publication.Status)
			}
			select {
			case <-published:
			default:
				t.Fatal("evicted model was not published")
			}
		})
	}
}

func TestCanceledQueuedWatcherWaitsForAdmissionEffects(t *testing.T) {
	m, reg := newWatcherTestMgr(t)
	run := instance.NewInstance("queued", "tenant", "", 0, 0, 0)
	done := make(chan struct{})
	entered := make(chan struct{}, 1)
	m.publishCatalog = func() error { entered <- struct{}{}; return errors.New("publication blocked") }
	h, err := reg.StartDetached(jobs.KindRun, "", nil)
	require.NoError(t, err)
	sub, err := reg.Subscribe(h.ID(), jobs.SubscribeOptions{})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	watcherDone := make(chan struct{})
	go func() { defer close(watcherDone); m.watchInstanceReadiness(run, h, done) }()
	for ev := range sub.Events() {
		if ev.Step != "" {
			break
		}
	}
	run.Cancel()
	select {
	case <-watcherDone:
		t.Fatal("watcher skipped in-flight admission evidence")
	case <-time.After(50 * time.Millisecond):
	}
	run.SetEvictedRuns([]instance.EvictedRun{{RunID: "victim", Model: "weights"}})
	close(done)
	ev := subscribeTerminal(t, reg, h.ID(), 3*time.Second)
	require.Equal(t, string(httperr.CodeCatalogPublicationFailed), ev.Meta["code"])
	select {
	case <-entered:
	default:
		t.Fatal("cancelled admission lost its model eviction")
	}
}

func TestQueuedStopDrainsActualAdmissionPublication(t *testing.T) {
	f := newQueueFixture(t)
	makeNodeScoped(t, f.mgr)
	reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(reg.Stop)
	f.mgr.jobsRegistry = reg
	f.mgr.idleEvictionDefault = true
	idle := instance.NewInstance("idle", "generic", "weights", 0, time.Hour, 10)
	idle.MarkRunning(false, nil)
	idle.SetProcessInfo(9000001, 0, 0, nil)
	require.NoError(t, f.mgr.instances.Register(idle))
	evicted, resume, publication, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		free := int64(1000)
		if idle.GetStatus() == instance.StatusStopped {
			once.Do(func() { close(evicted); <-resume })
			free = 6000
		}
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: free, Processes: []gpu.MemoryProcess{{PID: 9000001, MemoryMiB: new(int64(5000))}}}}, nil
	}
	f.mgr.publishCatalog = func() error { close(publication); <-release; return errors.New("reverse blocked") }
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.mgr.memoryAdmission <- struct{}{}
	result, err := f.mgr.EnsureRun(t.Context(), EnsureRequest{Provider: "generic"})
	require.NoError(t, err)
	<-f.mgr.memoryAdmission
	select {
	case <-evicted:
	case <-time.After(5 * time.Second):
		t.Fatal("model eviction did not reach recovery")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- f.mgr.StopInstance(t.Context(), result.Run.ID) }()
	require.Eventually(t, func() bool { return result.Run.Context().Err() != nil }, time.Second, time.Millisecond)
	event, err := reg.Get(result.Run.StreamJobID)
	require.NoError(t, err)
	require.False(t, event.Phase.IsTerminal(), "cancellation must await the actual admission owner")
	close(resume)
	select {
	case <-publication:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled admission lost model publication")
	}
	select {
	case <-stopped:
		t.Fatal("stop did not drain publication")
	default:
	}
	close(release)
	select {
	case err = <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("stop failed to drain admission and watcher")
	}
	event = subscribeTerminal(t, reg, result.Run.StreamJobID, time.Second)
	require.Equal(t, string(httperr.CodeCatalogPublicationFailed), event.Meta["code"])
}

func TestNodeRunJobCatalogEffects(t *testing.T) {
	for _, tc := range []struct {
		name, model, victim string
		published           bool
	}{
		{"isolated node", "", "", false},
		{"node victim", "", "", false},
		{"model victim", "", "weights", true},
		{"model run", "weights", "", true},
	} {
		for _, status := range []instance.Status{instance.StatusRunning, instance.StatusFailed} {
			t.Run(tc.name+"/"+string(status), func(t *testing.T) {
				m, reg := newWatcherTestMgr(t)
				calls := 0
				m.publishCatalog = func() error { calls++; return errors.New("reverse connection unavailable") }
				run := instance.NewInstance("run", "any-provider", tc.model, 0, 0, 0)
				if tc.name != "isolated node" {
					run.SetEvictedRuns([]instance.EvictedRun{{RunID: "victim", Model: tc.victim}})
				}
				run.SetStatus(status)
				h, err := reg.StartDetached(jobs.KindRun, "", jobs.Meta{"run_id": run.ID})
				require.NoError(t, err)
				go m.watchInstanceReadiness(run, h, nil)
				ev := subscribeTerminal(t, reg, h.ID(), 3*time.Second)
				if tc.published {
					require.Equal(t, 1, calls)
					require.Equal(t, jobs.PhaseFailed, ev.Phase)
					require.Equal(t, string(httperr.CodeCatalogPublicationFailed), ev.Meta["code"])
					require.Equal(t, status, ev.Meta["run_status"])
				} else {
					require.Zero(t, calls)
					want := jobs.PhaseDone
					if status == instance.StatusFailed {
						want = jobs.PhaseFailed
					}
					require.Equal(t, want, ev.Phase)
				}
			})
		}
	}
}

func TestNodeRunEventsRefreshResourcesWithoutCatalog(t *testing.T) {
	m, _ := newWatcherTestMgr(t)
	resources, catalog := 0, 0
	m.resourcesChanged = func() { resources++ }
	m.catalogChanged = func() { catalog++ }
	for _, kind := range []EventType{EventInstanceRunning, EventInstanceFailed, EventInstanceStopped} {
		m.notifyCatalog(Event{Type: kind})
	}
	require.Equal(t, 3, resources)
	require.Zero(t, catalog)
	m.notifyCatalog(Event{Type: EventInstanceStopped, Model: "weights"})
	require.Equal(t, 4, resources)
	require.Equal(t, 1, catalog)
}
