package prov_apps

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runningPublicationEvents(m *ProviderAppManager) []EventType {
	var events []EventType
	for {
		select {
		case event := <-m.auditEvents:
			if event.Type == EventInstanceRunning || event.Type == EventInstanceStopped {
				events = append(events, event.Type)
			}
		default:
			return events
		}
	}
}

func TestRunningPublicationContractRetirementDuringLoad(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(map[bool]string{false: "daemon retirement", true: "config retirement"}[disable], func(t *testing.T) {
			r := newServedRig(t)
			r.daemon.holdLoads()
			o := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "ollama", Model: servedModel})})[0]
			require.NoError(t, o.err)
			run := o.result.Run
			require.NotNil(t, run)
			r.daemon.waitEntered(t)
			r.daemon.set(func(d *fakeDaemon) {
				d.psHook = func() {
					if disable {
						r.edit(t, func(svc *config.ServiceConfig) { svc.Enabled = new(false) })
					} else {
						r.m.retireRun(run, "retired during final load listing", obsruns.StopReasonUser)
					}
				}
			})
			r.daemon.release()
			ctx, cancel := context.WithTimeout(t.Context(), contractWait)
			defer cancel()
			require.NoError(t, run.WaitForGoroutines(ctx))
			assert.Equal(t, instance.StatusFailed, run.GetStatus())
			assert.ErrorIs(t, run.FailureCause(), ErrModelDropped)
			assert.True(t, run.GetStartedAt().IsZero(), "a retired start never publishes readiness")
			assert.NotContains(t, runLog(t, run), " loaded\n")
			assert.Equal(t, []EventType{EventInstanceStopped}, runningPublicationEvents(r.m))
			_, exists := r.m.instances.Get(run.ID)
			assert.False(t, exists)
			assert.False(t, r.daemon.isLoaded(run.WireModel), "the rejected landing uses the existing unload cleanup")
			assert.Equal(t, 1, r.daemon.count(true))
		})
	}
}

func TestRunningPublicationContractReentrantRetirement(t *testing.T) {
	var r *servedRig
	var run *instance.Instance
	var armed atomic.Bool
	r = newServedRig(t, WithCatalogChanged(func() {
		if armed.CompareAndSwap(true, false) {
			assert.Equal(t, instance.StatusRunning, run.GetStatus(), "catalog callback may read the instance")
			_, exists := r.m.instances.Get(run.ID)
			assert.True(t, exists, "catalog callback may read the registry")
			r.m.retireRun(run, "retired by reentrant catalog observer", obsruns.StopReasonUser)
		}
	}))
	r.daemon.holdLoads()
	o := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "ollama", Model: servedModel})})[0]
	require.NoError(t, o.err)
	run = o.result.Run
	require.NotNil(t, run)
	r.daemon.waitEntered(t)
	armed.Store(true)
	r.daemon.release()
	ctx, cancel := context.WithTimeout(t.Context(), contractWait)
	defer cancel()
	require.NoError(t, run.WaitForGoroutines(ctx))
	assert.Equal(t, []EventType{EventInstanceRunning, EventInstanceStopped}, runningPublicationEvents(r.m))
	log := runLog(t, run)
	loaded, retired := strings.Index(log, " loaded\n"), strings.Index(log, "retired by reentrant catalog observer")
	require.NotEqual(t, -1, loaded)
	require.NotEqual(t, -1, retired)
	assert.Less(t, loaded, retired)
	assert.ErrorIs(t, run.FailureCause(), ErrModelDropped)
}

func TestRunningPublicationContractAdoptedStartingStop(t *testing.T) {
	r := newServedRig(t)
	key, svc, ok := r.m.resolveConfigKey("ollama")
	require.True(t, ok)
	run, _, err := (servedEngine{r.m}).servedRun(t.Context(), EnsureRequest{Provider: "ollama", Model: servedLoaded, Endpoint: EndpointChat}, svc, key)
	require.NoError(t, err)
	run.StartedBy = instance.StartedByExternal
	require.NoError(t, r.m.instances.Register(run))
	r.daemon.set(func(d *fakeDaemon) { d.loaded[run.WireModel] = time.Time{} })
	require.True(t, r.daemon.isLoaded(run.WireModel))
	require.True(t, run.GetStartedAt().IsZero())
	require.NoError(t, r.m.endRun(t.Context(), run, obsruns.StopReasonUser, (servedEngine{r.m}).down))
	assert.Equal(t, 1, r.daemon.count(true))
	assert.False(t, r.daemon.isLoaded(run.WireModel))
	assert.True(t, run.GetStartedAt().IsZero())
	assert.Equal(t, []EventType{EventInstanceStopped}, runningPublicationEvents(r.m))
}

func TestRunningPublicationContractEndExclusion(t *testing.T) {
	run := instance.NewInstance("commit", "p", "m", 0, -1, 1)
	leftStarting := run.LeftStarting()
	entered, release := make(chan struct{}), make(chan struct{})
	ready, ended := make(chan bool, 1), make(chan bool, 1)
	go func() { ready <- run.MarkRunning(false, func(bool) { close(entered); <-release }) }()
	select {
	case <-entered:
	case <-time.After(contractWait):
		t.Fatal("commit never entered")
	}
	go func() { ended <- run.ClaimEnd() }()
	waitParkedOn(t, ").ClaimEnd(", 1, contractWait)
	select {
	case <-leftStarting:
		t.Error("readiness escaped before commit")
	default:
	}
	close(release)
	select {
	case accepted := <-ready:
		require.True(t, accepted)
	case <-time.After(contractWait):
		t.Fatal("commit never returned")
	}
	select {
	case claimed := <-ended:
		require.True(t, claimed)
	case <-time.After(contractWait):
		t.Fatal("end never claimed")
	}
	assert.Equal(t, instance.StatusRunning, run.GetStatus())
	assert.False(t, run.GetStartedAt().IsZero())
	called := false
	assert.False(t, run.MarkRunning(true, func(bool) { called = true }))
	assert.False(t, called)
}

func TestRunningPublicationContractAdoptionReentry(t *testing.T) {
	var r *servedRig
	var calls atomic.Int32
	r = newServedRig(t, WithCatalogChanged(func() {
		if calls.Add(1) == 1 {
			// Adoption callbacks may enter the next observation without an outer lock held.
			r.m.adoptServed(t.Context(), "ollama", nil, "http://daemon")
		}
	}))
	r.daemon.loadExternally(servedLoaded, time.Time{})
	r.tick(t)
	r.tick(t)
	runs := r.m.instances.List()
	require.Len(t, runs, 1)
	assert.Equal(t, instance.StartedByExternal, runs[0].StartedBy)
	assert.Equal(t, instance.StatusRunning, runs[0].GetStatus())
	assert.False(t, runs[0].GetStartedAt().IsZero())
	assert.Equal(t, []EventType{EventInstanceRunning}, runningPublicationEvents(r.m))
	assert.Contains(t, runLog(t, runs[0]), "adopted (loaded by another client)")
}

func TestRunningPublicationContractRepeatedReadiness(t *testing.T) {
	var notifications atomic.Int32
	r := newContractRig(t, true, WithCatalogChanged(func() { notifications.Add(1) }))
	run := contextRun(t, r)
	var accepted atomic.Int32
	var publishers sync.WaitGroup
	for range 8 {
		publishers.Go(func() {
			if r.m.publishRunning(run, true, "ready") {
				accepted.Add(1)
			}
		})
	}
	publishers.Wait()
	assert.Equal(t, int32(1), accepted.Load())
	assert.Equal(t, []EventType{EventInstanceRunning}, runningPublicationEvents(r.m))
	assert.Equal(t, int32(1), notifications.Load())
	run.MarkUnhealthy("temporary health failure")
	require.True(t, r.m.publishRunning(run, true, "recovered"))
	assert.Equal(t, []EventType{EventInstanceRunning}, runningPublicationEvents(r.m))
	assert.Equal(t, int32(2), notifications.Load())
}
