package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"testing"
	"time"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type jobTestRouter struct {
	fakeRouter
	registry *jobs.Registry
}

func (r *jobTestRouter) Route(_ context.Context, req *routing.Request) (*routing.Response, error) {
	u, _ := url.Parse(req.Path)
	ev, err := r.registry.Get(path.Base(u.Path))
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(ev)
	return &routing.Response{StatusCode: 200, Body: b}, err
}

func TestDeployRestartWaitsForDownloadAndScopesRuns(t *testing.T) {
	reg := newTestJobsRegistry(t)
	dl, err := reg.StartDetached(jobs.KindDownload, "", nil)
	require.NoError(t, err)
	cfg := featureTestConfig(t)
	require.NoError(t, cfg.UpdateApp("llamacpp", func(svc *pkgConfig.ServiceConfig) error {
		svc.Models = map[string]pkgConfig.ModelSpec{"model+vision": {From: "org/model-GGUF"}}
		return nil
	}))
	a, variant, other, remote, unchanged := run("a", "worker", stale), run("variant", "worker", stale), run("other", "worker", stale), run("remote", "elsewhere", stale), run("current", "worker", current)
	a.Model, variant.Model, other.Model, remote.Model, unchanged.Model = "org/model-GGUF", "model+vision", "org/other-GGUF", "org/model-GGUF", "org/model-GGUF"
	f := &fakeRuns{runs: []instance.InstanceInfo{a, variant, other, remote, unchanged}}
	router := &jobTestRouter{registry: reg}
	s := NewDeploymentsService(router, nil, func() *pkgConfig.AppsConfig { return cfg })
	s.refresher = &runsRefresher{runs: f, jobs: reg}
	d := &Deployment{ID: "deployment", Model: "org/model-GGUF", Nodes: []DeploymentNode{{Node: "worker", JobID: dl.ID()}}}
	id, err := s.restartAfterDeploy(d, map[string]deployPlan{"worker": {Provider: "llamacpp", Download: &metadata.DownloadRequest{Repo: d.Model}}})
	require.NoError(t, err)
	ev, err := reg.Get(id)
	require.NoError(t, err)
	assert.False(t, ev.Phase.IsTerminal())
	f.mu.Lock()
	assert.Empty(t, f.restarted)
	f.mu.Unlock()
	dl.Done()
	ev = waitJob(t, reg, id)
	assert.Equal(t, jobs.PhaseDone, ev.Phase)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, []RestartRunRequest{{RunID: "a", Node: "worker"}, {RunID: "variant", Node: "worker"}}, f.restarted)
}

type readinessRuns struct{ fakeRuns }

func (f *readinessRuns) RestartRun(ctx context.Context, req *RestartRunRequest) (*RestartRunResponse, error) {
	resp, err := f.fakeRuns.RestartRun(ctx, req)
	if resp != nil {
		resp.JobID = "restart-" + req.RunID
	}
	return resp, err
}

func TestRestartRunsCancellationStopsSubsequentRuns(t *testing.T) {
	reg := newTestJobsRegistry(t)
	h, err := reg.StartDetached(jobs.KindRun, "", nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &readinessRuns{}
	r := &runsRefresher{runs: f, waitJob: func(waitCtx context.Context, _, _ string) error { cancel(); <-waitCtx.Done(); return waitCtx.Err() }}
	err = r.restartRuns(ctx, []RunRef{{ID: "first", Node: "worker"}, {ID: "second", Node: "worker"}}, h)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []RestartRunRequest{{RunID: "first", Node: "worker"}}, f.restarted)
}

func TestRestartRunsReadinessFailureIsNotSuccess(t *testing.T) {
	reg := newTestJobsRegistry(t)
	h, err := reg.StartDetached(jobs.KindRun, "", nil)
	require.NoError(t, err)
	f := &readinessRuns{}
	r := &runsRefresher{runs: f, waitJob: func(context.Context, string, string) error { return errors.New("engine failed readiness") }}
	err = r.restartRuns(t.Context(), []RunRef{{ID: "run", Node: "worker"}}, h)
	assert.ErrorContains(t, err, "engine failed readiness")
}

func TestWaitJobCompletionStopsOnCancellationAndFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	err := waitJobCompletion(ctx, func(context.Context) (jobs.Event, error) { return jobs.Event{Phase: jobs.PhaseRunning}, nil })
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	err = waitJobCompletion(t.Context(), func(context.Context) (jobs.Event, error) {
		return jobs.Event{JobID: "failed", Phase: jobs.PhaseFailed, Err: "installer failed"}, nil
	})
	assert.ErrorContains(t, err, "installer failed")
}
