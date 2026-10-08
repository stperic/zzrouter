package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// fakeRuns is a cluster with fixed runs that records restarts.
type fakeRuns struct {
	runs    []instance.InstanceInfo
	listErr error
	fail    map[string]bool

	mu        sync.Mutex
	restarted []RestartRunRequest
	listed    *ListRunsRequest
}

func (f *fakeRuns) ListRuns(_ context.Context, req *ListRunsRequest) (*ListRunsResponse, error) {
	f.listed = req
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &ListRunsResponse{Data: f.runs}, nil
}

func (f *fakeRuns) RestartRun(_ context.Context, req *RestartRunRequest) (*RestartRunResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, *req)
	if f.fail[req.RunID] {
		return nil, errors.New("relaunch refused")
	}
	return &RestartRunResponse{OldID: req.RunID, ID: req.RunID + "-new"}, nil
}

func run(id, node string, status *instance.ParametersStatus) instance.InstanceInfo {
	return instance.InstanceInfo{ID: id, Node: node, Provider: "llamacpp", Model: "m", Status: instance.StatusRunning, ParametersStatus: status}
}

var (
	stale   = &instance.ParametersStatus{Provider: "llamacpp", State: instance.ParametersStale, Changed: []string{"parameters.chat-template-file"}}
	current = &instance.ParametersStatus{Provider: "llamacpp", State: instance.ParametersCurrent}
)

// landedOn is a sync that reached exactly these workers.
func landedOn(nodes ...string) SyncReport { return SyncReport{State: SyncComplete, Landed: nodes} }

// Each live run of the provider lands in the list its node's status puts
// it in; a node the write has not reached cannot vouch for its runs.
func TestRunsReport_ClassifiesByWhatTheNodeSays(t *testing.T) {
	stopped := run("stopped", "worker-1", stale)
	stopped.Status = instance.StatusStopped
	other := run("other", "worker-1", &instance.ParametersStatus{Provider: "vllm", State: instance.ParametersStale})
	f := &fakeRuns{runs: []instance.InstanceInfo{
		run("a", "worker-1", stale),
		run("b", "worker-1", current),
		run("c", "worker-1", &instance.ParametersStatus{Provider: "llamacpp", State: instance.ParametersCurrent, Overridden: []string{"parameters.ctx-size"}}),
		run("d", "worker-1", &instance.ParametersStatus{Provider: "llamacpp", State: instance.ParametersUnknown, Error: "asset gone"}),
		run("daemon", "worker-1", nil),
		run("f", "asleep", stale),
		stopped, other,
	}}
	r := &runsRefresher{runs: f}

	got := r.report(t.Context(), "llamacpp", SyncReport{State: SyncPartial, Landed: []string{"worker-1"}}, false)
	require.NotNil(t, f.listed)
	assert.True(t, f.listed.WithParametersStatus, "the report asks the nodes for the status")

	assert.Equal(t, []RunRef{{ID: "a", Node: "worker-1", Model: "m", Changed: []string{"parameters.chat-template-file"}}}, got.Stale)
	assert.Equal(t, []RunRef{{ID: "c", Node: "worker-1", Model: "m", Overridden: []string{"parameters.ctx-size"}}}, got.Overridden)
	assert.Equal(t, []RunRef{
		{ID: "d", Node: "worker-1", Model: "m", Reason: "asset gone"},
		{ID: "f", Node: "asleep", Model: "m", Reason: reasonSyncPending},
	}, got.Unknown, "a run no config launched (daemon) is not in the report")
	assert.Empty(t, got.RestartJobID)
	assert.Empty(t, f.restarted, "nothing restarts unless asked")
}

// A sync that outlived its wait reached no worker known, but this node
// holds every write it makes, so its own runs are judged as usual.
func TestRunsReport_PendingSyncVouchesForNoWorker(t *testing.T) {
	r := &runsRefresher{
		runs:  &fakeRuns{runs: []instance.InstanceInfo{run("a", "worker-1", stale), run("b", "coord", stale)}},
		local: func() string { return "coord" },
	}
	got := r.report(t.Context(), "llamacpp", SyncReport{State: SyncPending}, false)
	assert.Equal(t, []RunRef{{ID: "b", Node: "coord", Model: "m", Changed: stale.Changed}}, got.Stale)
	assert.Equal(t, []RunRef{{ID: "a", Node: "worker-1", Model: "m", Reason: reasonSyncPending}}, got.Unknown)
}

// A run is matched by the config key its node judged it against, not the
// spelling it was launched under.
func TestRunsReport_MatchesByConfigKey(t *testing.T) {
	launchedAs := run("a", "worker-1", stale)
	launchedAs.Provider = "llama.cpp"
	r := &runsRefresher{runs: &fakeRuns{runs: []instance.InstanceInfo{launchedAs}}}
	got := r.report(t.Context(), "llamacpp", landedOn("worker-1"), false)
	require.Len(t, got.Stale, 1)
	assert.Equal(t, "a", got.Stale[0].ID)
}

// A runs listing that fails does not fail the write it follows.
func TestRunsReport_ListFailureIsReported(t *testing.T) {
	r := &runsRefresher{runs: &fakeRuns{listErr: errors.New("cluster unreachable")}}
	got := r.report(t.Context(), "llamacpp", landedOn("worker-1", "macbook-pro"), true)
	assert.Equal(t, "cluster unreachable", got.Error)
	assert.Empty(t, got.Stale)
}

// waitJob returns a job's terminal event.
func waitJob(t *testing.T, reg *jobs.Registry, id string) jobs.Event {
	t.Helper()
	var ev jobs.Event
	require.Eventually(t, func() bool {
		var err error
		ev, err = reg.Get(id)
		return err == nil && (ev.Phase == jobs.PhaseDone || ev.Phase == jobs.PhaseFailed)
	}, 5*time.Second, 10*time.Millisecond)
	return ev
}

// Asked to, the report restarts exactly the stale runs, each on its own
// node, in a job that says which runs came back.
func TestRunsReport_RestartsStaleRunsInAJob(t *testing.T) {
	f := &fakeRuns{runs: []instance.InstanceInfo{
		run("a", "worker-1", stale), run("b", "macbook-pro", stale), run("c", "worker-1", current),
	}}
	reg := newTestJobsRegistry(t)
	r := &runsRefresher{runs: f, jobs: reg}

	got := r.report(t.Context(), "llamacpp", landedOn("worker-1", "macbook-pro"), true)
	require.NotEmpty(t, got.RestartJobID)

	ev := waitJob(t, reg, got.RestartJobID)
	assert.Equal(t, jobs.PhaseDone, ev.Phase)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.ElementsMatch(t, []RestartRunRequest{{RunID: "a", Node: "worker-1"}, {RunID: "b", Node: "macbook-pro"}}, f.restarted)
}

// A restart stops the run before relaunching it, so one that did not come
// back fails the job by name; the others still restart.
func TestRunsReport_FailedRestartFailsTheJob(t *testing.T) {
	f := &fakeRuns{
		runs: []instance.InstanceInfo{run("a", "worker-1", stale), run("b", "worker-1", stale)},
		fail: map[string]bool{"a": true},
	}
	reg := newTestJobsRegistry(t)
	r := &runsRefresher{runs: f, jobs: reg}

	got := r.report(t.Context(), "llamacpp", landedOn("worker-1", "macbook-pro"), true)
	ev := waitJob(t, reg, got.RestartJobID)
	assert.Equal(t, jobs.PhaseFailed, ev.Phase)
	assert.Contains(t, ev.Err, "a on worker-1")
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.restarted, 2)
}

// ?restart takes one value; anything else is refused before the write.
func TestReadRestart(t *testing.T) {
	for query, want := range map[string]bool{"": false, "?restart=affected": true} {
		c, _ := testGinContext(http.MethodPatch, "/x"+query)
		restart, ok := readRestart(c)
		assert.True(t, ok, query)
		assert.Equal(t, want, restart, query)
	}
	c, rec := testGinContext(http.MethodPatch, "/x?restart=all")
	_, ok := readRestart(c)
	assert.False(t, ok)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
}
