package install

import (
	"context"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newJobsReg(t *testing.T) *jobs.Registry {
	t.Helper()
	r, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(r.Stop)
	return r
}

func drainBounded(ch <-chan jobs.Event, deadline time.Duration) []jobs.Event {
	var out []jobs.Event
	timer := time.After(deadline)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timer:
			return out
		}
	}
}

func TestInstallProgress_SetStepMirrorsToJobs(t *testing.T) {
	reg := newJobsReg(t)
	h, err := reg.Start(context.Background(), jobs.KindInstall, "", jobs.Meta{"provider": "ollama"})
	require.NoError(t, err)

	p := &InstallProgress{Provider: "ollama", Action: "install", Status: StatusRunning}
	p.SetJobHandle(h)
	require.Equal(t, h.ID(), p.JobID, "JobID should populate on attach")

	sub, err := reg.Subscribe(h.ID(), jobs.SubscribeOptions{})
	require.NoError(t, err)

	p.SetStep(2, 5, "downloading venv")
	p.SetDownloadProgress(1024, 4096, 25)
	h.Done()

	events := drainBounded(sub.Events(), 500*time.Millisecond)
	require.NotEmpty(t, events)

	var sawStep, sawBytes, sawDone bool
	for _, ev := range events {
		if ev.Phase == jobs.PhaseRunning && ev.Step == "downloading venv" && ev.Bytes == nil {
			sawStep = true
		}
		if ev.Phase == jobs.PhaseRunning && ev.Bytes != nil && ev.Bytes.Total == 4096 {
			sawBytes = true
		}
		if ev.Phase == jobs.PhaseDone {
			sawDone = true
		}
	}
	assert.True(t, sawStep, "SetStep should fan out as a running event with description")
	assert.True(t, sawBytes, "SetDownloadProgress should carry bytes")
	assert.True(t, sawDone, "terminal must fire")
}

func TestInstallProgress_SetFailedCarriesError(t *testing.T) {
	reg := newJobsReg(t)
	h, err := reg.Start(context.Background(), jobs.KindInstall, "", nil)
	require.NoError(t, err)

	p := &InstallProgress{Provider: "vllm", Action: "install", Status: StatusRunning}
	p.SetJobHandle(h)

	sub, err := reg.Subscribe(h.ID(), jobs.SubscribeOptions{})
	require.NoError(t, err)

	p.SetFailed("pip install exited with 1")

	events := drainBounded(sub.Events(), 500*time.Millisecond)
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	assert.Equal(t, jobs.PhaseFailed, last.Phase)
	assert.Contains(t, last.Err, "pip install exited with 1")
}

func TestInstallProgress_NoOpWithoutHandle(t *testing.T) {
	p := &InstallProgress{Provider: "x", Action: "install", Status: StatusRunning}
	// No SetJobHandle. Every Set* must be safe.
	p.SetStep(1, 3, "step")
	p.SetDownloadProgress(1, 2, 50)
	p.SetCompleted()
	p.SetFailed("whatever")
	assert.Empty(t, p.JobID)
}
