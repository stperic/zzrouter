package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

type rolloutFixture struct {
	manifest      *ReleaseManifest
	manifestError error
	windows       map[string]version.ProtocolWindow
	windowErrors  map[string]error
	windowCalls   []string
	statuses      map[string]UpdateStatus
	unavailable   map[string]bool
	calls         []string
	statusHook    func()
	applyError    error
}

func (f *rolloutFixture) Manifest(_ context.Context, target string) (ReleaseManifest, error) {
	if f.manifest != nil {
		return *f.manifest, f.manifestError
	}
	return rolloutManifest(target), f.manifestError
}

func (f *rolloutFixture) Window(_ context.Context, node string) (version.ProtocolWindow, error) {
	f.windowCalls = append(f.windowCalls, node)
	if err := f.windowErrors[node]; err != nil {
		return version.ProtocolWindow{}, err
	}
	if w, ok := f.windows[node]; ok {
		return w, nil
	}
	return version.LocalProtocolWindow(), nil
}

func rolloutManifest(target string) ReleaseManifest {
	return ReleaseManifest{Version: target, ClusterProtocol: version.ClusterProtocolVersion, MinClusterProtocol: version.MinClusterProtocolVersion}
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
	record, err := r.Submit(context.Background(), "2.0.0", []string{"coord", "z", "a"}, nil)
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
	_, err := r.Submit(context.Background(), "2.0.0", []string{"coord", "worker"}, nil)
	require.NoError(t, err)
	f.unavailable["worker"] = true
	for i := 0; i < 5; i++ {
		r.step(context.Background())
	}
	require.Empty(t, f.calls)
	require.Equal(t, "pending", r.History()[0].Nodes[0].State)
	f.unavailable["worker"] = false
	r.step(context.Background())
	require.Equal(t, []string{"worker"}, f.calls)
}
func TestRolloutPendingNeverStartedTimesOut(t *testing.T) {
	registry, err := jobs.NewRegistry(jobs.Config{NodeName: "coord", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(registry.Stop)
	f := &rolloutFixture{statuses: map[string]UpdateStatus{}, unavailable: map[string]bool{}}
	fc := clocktest.NewFakeClock(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	r, err := NewRollouts(filepath.Join(t.TempDir(), "rollouts.json"), "coord", f, registry, fc)
	require.NoError(t, err)
	t.Cleanup(r.Stop)

	_, err = r.Submit(context.Background(), "2.0.0", []string{"worker"}, nil)
	require.NoError(t, err)
	f.unavailable["worker"] = true
	r.step(context.Background())
	node := r.History()[0].Nodes[0]
	require.Equal(t, "pending", node.State)
	require.Nil(t, node.StartedAt)
	require.NotNil(t, node.WaitingSince)
	require.Equal(t, fc.Now(), *node.WaitingSince)
	var persisted []Rollout
	require.NoError(t, readJSON(r.path, &persisted))
	require.NotNil(t, persisted[0].Nodes[0].WaitingSince)
	require.Equal(t, *node.WaitingSince, *persisted[0].Nodes[0].WaitingSince)

	fc.Advance(rolloutApplyTimeout + time.Minute)
	r.step(context.Background())
	require.Empty(t, f.calls)
	result := r.History()[0]
	require.Equal(t, "failed", result.State)
	require.Equal(t, "failed", result.Nodes[0].State)
	require.Equal(t, "node was unreachable and never started the update", result.Nodes[0].Error)
}

func TestRolloutStopsOnFirstFailure(t *testing.T) {
	r, f, _ := newRolloutFixture(t)
	record, err := r.Submit(context.Background(), "2.0.0", []string{"coord", "worker"}, nil)
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
			record, err := r.Submit(context.Background(), "2.0.0", []string{"worker"}, nil)
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
	record, err := r.Submit(context.Background(), "2.0.0", []string{"worker"}, nil)
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
	record, err := r.Submit(context.Background(), "2.0.0", []string{"worker"}, nil)
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
	record, err := r.Submit(context.Background(), "2.0.0", []string{"worker"}, nil)
	require.NoError(t, err)
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	_, err = r.change(record.JobID, func(v *Rollout) { v.Nodes[0].StartedAt = &now })
	require.NoError(t, err)
	snap := r.History()
	*snap[0].Nodes[0].StartedAt = time.Time{}
	snap[0].Nodes[0].State = "changed"
	require.Equal(t, now, *r.History()[0].Nodes[0].StartedAt)
	require.NoError(t, r.Cancel(record.JobID))
	next, err := r.Submit(context.Background(), "3.0.0", []string{"worker"}, nil)
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

func assertRolloutRefused(t *testing.T, r *Rollouts, f *rolloutFixture, err error, status int, code httperr.RouteErrorCode) {
	t.Helper()
	var wire httperr.Error
	require.ErrorAs(t, err, &wire)
	require.Equal(t, status, wire.Status)
	require.Equal(t, string(code), wire.Code)
	r.step(context.Background())
	require.Empty(t, f.calls)
	require.Empty(t, r.History())
	_, statErr := os.Stat(r.path)
	require.True(t, os.IsNotExist(statErr), "rollout refusal persisted a record")
}

func TestRolloutPreflightRefusesProtocolCutover(t *testing.T) {
	r, f, _ := newRolloutFixture(t)
	manifest := rolloutManifest("2.0.0")
	manifest.ClusterProtocol = version.ClusterProtocolVersion + 1
	f.manifest = &manifest
	_, err := r.Submit(context.Background(), manifest.Version, []string{"worker", "coord"}, []string{"worker"})
	assertRolloutRefused(t, r, f, err, http.StatusConflict, httperr.CodeProtocolCutoverRequired)
	require.ErrorContains(t, err, "coord")
	require.ErrorContains(t, err, fmt.Sprintf("protocol %d", manifest.ClusterProtocol))
}

func TestRolloutPreflightRefusesStrandedPeer(t *testing.T) {
	r, f, _ := newRolloutFixture(t)
	f.windows = map[string]version.ProtocolWindow{"older": {Min: version.ClusterProtocolVersion - 1, Max: version.ClusterProtocolVersion - 1}}
	manifest := rolloutManifest("2.0.0")
	manifest.MinClusterProtocol = version.ClusterProtocolVersion
	f.manifest = &manifest
	_, err := r.Submit(context.Background(), manifest.Version, []string{"worker", "coord"}, []string{"worker", "older"})
	assertRolloutRefused(t, r, f, err, http.StatusConflict, httperr.CodeProtocolCutoverRequired)
	require.ErrorContains(t, err, "older")
	require.ErrorContains(t, err, fmt.Sprintf("minimum protocol %d", manifest.MinClusterProtocol))
}

func TestRolloutPreflightUnknownWindowIs503(t *testing.T) {
	for _, node := range []string{"worker", "unselected"} {
		t.Run(node, func(t *testing.T) {
			r, f, _ := newRolloutFixture(t)
			f.windowErrors = map[string]error{node: ErrNodeUnavailable}
			_, err := r.Submit(context.Background(), "2.0.0", []string{"worker", "coord"}, []string{"worker", "unselected"})
			assertRolloutRefused(t, r, f, err, http.StatusServiceUnavailable, httperr.CodeNodeUnreachable)
			require.ErrorContains(t, err, node)
		})
	}
}

func TestRolloutPreflightAllowsCompatible(t *testing.T) {
	r, f, _ := newRolloutFixture(t)
	record, err := r.Submit(context.Background(), "2.0.0", []string{"coord", "worker"}, []string{"worker", "unselected"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"coord", "worker", "unselected"}, f.windowCalls)
	require.Equal(t, "worker", record.Nodes[0].Node)
	r.step(context.Background())
	require.Equal(t, []string{"worker"}, f.calls)
	var persisted []Rollout
	require.NoError(t, readJSON(r.path, &persisted))
	require.Len(t, persisted, 1)
}

func TestRolloutPreflightRefusalPreservesHistory(t *testing.T) {
	r, f, _ := newRolloutFixture(t)
	record, err := r.Submit(context.Background(), "2.0.0", []string{"worker"}, nil)
	require.NoError(t, err)
	require.NoError(t, r.Cancel(record.JobID))
	before, err := os.ReadFile(r.path)
	require.NoError(t, err)
	manifest := rolloutManifest("3.0.0")
	manifest.ClusterProtocol++
	f.manifest = &manifest
	_, err = r.Submit(context.Background(), manifest.Version, []string{"worker"}, nil)
	require.Error(t, err)
	after, err := os.ReadFile(r.path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Empty(t, f.calls)
}

// A target whose manifest cannot be fetched or verified starts nothing.
func TestRolloutManifestFailureStartsNothing(t *testing.T) {
	r, f, _ := newRolloutFixture(t)
	f.manifestError = errors.New("release.json signature invalid")
	_, err := r.Submit(context.Background(), "2.0.0", []string{"worker"}, nil)
	require.ErrorIs(t, err, f.manifestError)
	_, statErr := os.Stat(r.path)
	require.True(t, os.IsNotExist(statErr), "nothing may be persisted")
	require.Empty(t, f.calls)
}
