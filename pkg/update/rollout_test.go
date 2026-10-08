package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

type rolloutFixture struct {
	statuses    map[string]UpdateStatus
	unavailable map[string]bool
	calls       []string
	statusHook  func()
	applyError  error
}

func (f *rolloutFixture) Status(_ context.Context, n string) (UpdateStatus, error) {
	if f.statusHook != nil {
		f.statusHook()
	}
	if f.unavailable[n] {
		return UpdateStatus{}, ErrNodeUnavailable
	}
	return f.statuses[n], nil
}
func (f *rolloutFixture) Apply(_ context.Context, n, v string) (string, error) {
	f.calls = append(f.calls, n)
	f.statuses[n] = UpdateStatus{CurrentVersion: version.MustParseVersion("1.0.0"), Operation: &RunStatus{JobID: "operation-" + n, ToVersion: v, State: StateApplying}}
	return "operation-" + n, f.applyError
}
func newRolloutFixture(t *testing.T) (*Rollouts, *rolloutFixture, *jobs.Registry) {
	t.Helper()
	registry, err := jobs.NewRegistry(jobs.Config{NodeName: "coord", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	f := &rolloutFixture{statuses: map[string]UpdateStatus{}, unavailable: map[string]bool{}}
	r, err := NewRollouts(filepath.Join(t.TempDir(), "rollouts.json"), "coord", f, registry, clock.System())
	require.NoError(t, err)
	t.Cleanup(r.Stop)
	return r, f, registry
}
func completeNode(f *rolloutFixture, n string) {
	status := f.statuses[n]
	status.CurrentVersion = version.MustParseVersion("2.0.0")
	finished := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	status.Operation.StartedAt = finished
	status.Operation.FinishedAt = &finished
	status.Operation.Success = true
	f.statuses[n] = status
}
func TestRolloutWorkersFirstAndClockIndependent(t *testing.T) {
	r, f, registry := newRolloutFixture(t)
	record, err := r.Submit("2.0.0", []string{"coord", "z", "a"})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "z", "coord"}, []string{record.Nodes[0].Node, record.Nodes[1].Node, record.Nodes[2].Node})
	for _, n := range []string{"a", "z", "coord"} {
		r.step(context.Background())
		require.Equal(t, n, f.calls[len(f.calls)-1])
		completeNode(f, n)
		r.step(context.Background())
	}
	r.step(context.Background())
	require.Equal(t, "succeeded", r.History()[0].State)
	event, err := registry.Get(record.JobID)
	require.NoError(t, err)
	require.Equal(t, jobs.PhaseDone, event.Phase)
	nodes := event.Meta["nodes"].([]NodeUpdate)
	for _, n := range nodes {
		require.Equal(t, "succeeded", n.State)
	}
}
func TestRolloutSleepingNodeRemainsPending(t *testing.T) {
	r, f, _ := newRolloutFixture(t)
	f.unavailable["worker"] = true
	_, err := r.Submit("2.0.0", []string{"coord", "worker"})
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		r.step(context.Background())
	}
	require.Empty(t, f.calls)
	require.Equal(t, "pending", r.History()[0].Nodes[0].State)
	f.unavailable["worker"] = false
	r.step(context.Background())
	require.Equal(t, []string{"worker"}, f.calls)
}
func TestRolloutStopsOnFirstFailure(t *testing.T) {
	r, f, _ := newRolloutFixture(t)
	record, err := r.Submit("2.0.0", []string{"coord", "worker"})
	require.NoError(t, err)
	r.step(context.Background())
	finished := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	status := f.statuses["worker"]
	status.Operation.FinishedAt = &finished
	status.Operation.Error = "signature identity mismatch"
	f.statuses["worker"] = status
	r.step(context.Background())
	r.step(context.Background())
	require.Equal(t, []string{"worker"}, f.calls)
	result := r.History()[0]
	require.Equal(t, record.JobID, result.JobID)
	require.Equal(t, "failed", result.State)
	require.Equal(t, "skipped", result.Nodes[1].State)
	require.Equal(t, "signature identity mismatch", result.Nodes[0].Error)
}
func TestRolloutReconcilesLostResponseAndPreDispatchRestart(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "before dispatch", true: "lost response"}[lost], func(t *testing.T) {
			r, f, registry := newRolloutFixture(t)
			record, err := r.Submit("2.0.0", []string{"worker"})
			require.NoError(t, err)
			_, err = r.change(record.JobID, func(v *Rollout) {
				now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
				v.Nodes[0].State = "applying"
				v.Nodes[0].StartedAt = &now
				v.Nodes[0].PreviousOperationID = "old"
			})
			require.NoError(t, err)
			if lost {
				f.statuses["worker"] = UpdateStatus{Operation: &RunStatus{JobID: "accepted", ToVersion: "2.0.0", State: StateApplying}}
			}
			require.NoError(t, registry.Cancel(record.JobID))
			fresh, err := jobs.NewRegistry(jobs.Config{NodeName: "coord", Clock: clock.System()})
			require.NoError(t, err)
			defer fresh.Stop()
			resumed, err := NewRollouts(r.path, "coord", f, fresh, clock.System())
			require.NoError(t, err)
			defer resumed.Stop()
			h, err := fresh.Resume(context.Background(), jobs.KindUpdate, "", record.JobID, nil)
			require.NoError(t, err)
			resumed.handle = h
			resumed.step(context.Background())
			if lost {
				require.Empty(t, f.calls)
			} else {
				require.Equal(t, []string{"worker"}, f.calls)
			}
		})
	}
}
func TestRolloutDurableCancellationDuringObservation(t *testing.T) {
	r, f, registry := newRolloutFixture(t)
	record, err := r.Submit("2.0.0", []string{"worker"})
	require.NoError(t, err)
	f.statusHook = func() { require.NoError(t, r.Cancel(record.JobID)); require.NoError(t, registry.Cancel(record.JobID)) }
	r.step(context.Background())
	require.Empty(t, f.calls)
	require.Equal(t, "cancelled", r.History()[0].State)
	var records []Rollout
	require.NoError(t, readJSON(r.path, &records))
	require.Equal(t, "cancelled", records[0].State)
}
func TestRolloutPersistenceFailureDoesNotDispatchOrTerminate(t *testing.T) {
	r, f, registry := newRolloutFixture(t)
	record, err := r.Submit("2.0.0", []string{"worker"})
	require.NoError(t, err)
	original := r.path
	r.path = filepath.Join(original, "impossible.json")
	r.step(context.Background())
	require.Empty(t, f.calls)
	require.Equal(t, "running", r.History()[0].State)
	event, err := registry.Get(record.JobID)
	require.NoError(t, err)
	require.NotEqual(t, jobs.PhaseFailed, event.Phase)
	require.Error(t, r.Cancel(record.JobID))
	require.Equal(t, "running", r.History()[0].State)
	r.path = original
	r.step(context.Background())
	require.Equal(t, []string{"worker"}, f.calls)
}
func TestRolloutSnapshotsAndOldIDAreIsolated(t *testing.T) {
	r, _, _ := newRolloutFixture(t)
	record, err := r.Submit("2.0.0", []string{"worker"})
	require.NoError(t, err)
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	_, err = r.change(record.JobID, func(v *Rollout) { v.Nodes[0].StartedAt = &now })
	require.NoError(t, err)
	snap := r.History()
	*snap[0].Nodes[0].StartedAt = time.Time{}
	snap[0].Nodes[0].State = "changed"
	require.Equal(t, now, *r.History()[0].Nodes[0].StartedAt)
	require.NoError(t, r.Cancel(record.JobID))
	next, err := r.Submit("3.0.0", []string{"worker"})
	require.NoError(t, err)
	_, err = r.change(record.JobID, func(v *Rollout) { v.State = "failed" })
	require.Error(t, err)
	require.Equal(t, next.JobID, r.History()[0].JobID)
	require.Equal(t, "running", r.History()[0].State)
}
func TestExactApplyOperationSurvivesRestartAndFailure(t *testing.T) {
	s := NewSchedulerIn(t.TempDir(), &config.UpdateConfig{}, clock.System())
	defer s.Stop()
	release := &ReleaseInfo{Version: version.MustParseVersion("2.0.0")}
	s.applying.Store(true)
	id, err := s.recordOperation("", release)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	require.Equal(t, id, s.GetStatus().Operation.JobID)
	s.finishOperation(errors.New("unsigned release"))
	status := s.GetStatus()
	require.True(t, status.Operation.Finished())
	require.Equal(t, "unsigned release", status.Operation.Error)
	s.applying.Store(false)
	_, err = s.recordOperation("next", release)
	require.NoError(t, err)
	require.Equal(t, "node stopped before completing update", s.GetStatus().Operation.Error)
	require.NoError(t, s.markOperationRestarting())
	require.NoError(t, os.WriteFile(s.operationPath, []byte("invalid"), 0600))
	require.NotEmpty(t, s.GetStatus().ConfirmationError)
}
func TestHandoffVersionRedeliveryIsIdempotent(t *testing.T) {
	h := NewHandoff(t.TempDir(), t.TempDir())
	first, err := h.ApplyVersion("2.0.0")
	require.NoError(t, err)
	require.NotEmpty(t, first)
	second, err := h.ApplyVersion("2.0.0")
	require.NoError(t, err)
	require.Equal(t, first, second)
	_, err = h.ApplyVersion("3.0.0")
	require.ErrorIs(t, err, ErrApplyInFlight)
	request, err := h.Claim()
	require.NoError(t, err)
	require.Equal(t, first, request.JobID)
	second, err = h.ApplyVersion("2.0.0")
	require.NoError(t, err)
	require.Equal(t, first, second)
}
