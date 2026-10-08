package servercli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

func TestPrivilegedApplyResolvesRequestedTag(t *testing.T) {
	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3-lab.1","prerelease":true}`))
	}))
	defer server.Close()
	scheduler := update.NewSchedulerIn(t.TempDir(), &config.UpdateConfig{Source: &config.UpdateSourceConfig{Owner: "stperic", Repo: "zzrouter", APIBaseURL: server.URL}}, clock.System())
	defer scheduler.Stop()
	run := &privilegedRun{req: &update.Request{Action: update.ActionApply, TargetVersion: "1.2.3-lab.1"}, sched: scheduler}
	err := run.apply(context.Background())
	require.ErrorContains(t, err, "checksums.txt.sigstore.json")
	require.Equal(t, "/repos/stperic/zzrouter/releases/tags/v1.2.3-lab.1", <-paths)
}

type failedPrivilegedBackend struct {
	handoff *update.Handoff
	clock   *clocktest.FakeClock
	calls   atomic.Int32
}

func (b *failedPrivilegedBackend) Status(_ context.Context, _ string) (update.UpdateStatus, error) {
	run, err := b.handoff.LastRun()
	return update.UpdateStatus{CurrentVersion: version.MustParseVersion("1.0.0"), Operation: run}, err
}
func (b *failedPrivilegedBackend) Apply(ctx context.Context, _ string, target string) (string, error) {
	b.calls.Add(1)
	req := &update.Request{Action: update.ActionApply, TargetVersion: target, JobID: "accepted-operation", RequestedAt: b.clock.Now()}
	_ = executeUpdateRequest(ctx, b.handoff, req, false, func() (*config.NodeConfig, error) { return nil, errors.New("invalid config") })
	return req.JobID, nil
}
func TestPrivilegedFailureStopsClusterRollout(t *testing.T) {
	clk := clocktest.NewFakeClock(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	b := &failedPrivilegedBackend{handoff: update.NewHandoff(t.TempDir(), t.TempDir()), clock: clk}
	registry, err := jobs.NewRegistry(jobs.Config{NodeName: "coord", Clock: clk})
	require.NoError(t, err)
	defer registry.Stop()
	r, err := update.NewRollouts(filepath.Join(t.TempDir(), "rollout.json"), "coord", b, registry, clk)
	require.NoError(t, err)
	defer r.Stop()
	_, err = r.Submit("2.0.0", []string{"worker", "coord"})
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.Eventually(t, func() bool { return b.calls.Load() == 1 }, time.Second, time.Millisecond)
	clk.Advance(5 * time.Second)
	require.Eventually(t, func() bool { return r.History()[0].State == "failed" }, time.Second, time.Millisecond)
	require.EqualValues(t, 1, b.calls.Load())
	require.Equal(t, "skipped", r.History()[0].Nodes[1].State)
	require.Contains(t, r.History()[0].Nodes[0].Error, "invalid config")
}

func TestClaimedUpdateConfigFailureAllowsAPIRecovery(t *testing.T) {
	h := update.NewHandoff(t.TempDir(), t.TempDir())
	require.NoError(t, h.Submit(&update.Request{Action: update.ActionApply, TargetVersion: "1.2.3"}))
	req, err := h.Claim()
	require.NoError(t, err)
	err = executeUpdateRequest(context.Background(), h, req, false, func() (*config.NodeConfig, error) { return nil, errors.New("invalid config") })
	require.ErrorContains(t, err, "invalid config")
	status, err := h.LastRun()
	require.NoError(t, err)
	require.True(t, status.Finished())
	require.Equal(t, update.StateFailed, status.State)
	require.NoError(t, h.Submit(&update.Request{Action: update.ActionRollback}))
}
