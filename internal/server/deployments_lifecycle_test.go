package server

import (
	"context"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/require"
)

// These use the real service wiring and job registry, with only dispatch
// replaced. No GET repairs the tracker before assertions.
func TestDeploymentRetainsUnreadCompletion(t *testing.T) {
	for _, kind := range []jobs.Kind{jobs.KindDownload, jobs.KindSync} {
		t.Run(string(kind), func(t *testing.T) {
			n := createTestNodeWithDefaults(t)
			s := n.services.Deployments
			s.appsConfig = nilAppsConfig
			h, err := n.jobs.StartDetached(kind, "", nil)
			require.NoError(t, err)
			r := &fakeRouter{perPath: map[string]struct {
				status int
				body   []byte
			}{
				"/zzrouter/v1/internal/deployments": {202, mustJSON(t, internalDeploymentResult{Key: "reused", Status: "downloading", JobID: h.ID()})},
				"/zzrouter/v1/internal/sync/deploy": {202, mustJSON(t, syncDeployAsyncResponse{Status: "accepted", LocalJobID: h.ID(), Source: "source"})},
			}}
			s.router = r
			var d *Deployment
			if kind == jobs.KindSync {
				s.resolveNodeURL = func(string) string { return "https://source:8080" }
				d = s.tracker.CreateDeployment("unit/model", "gguf", "huggingface", "", []string{n.node.Name()})
				require.True(t, s.dispatchSyncDeploy(t.Context(), d.ID, n.node.Name(), "source", &DeployRequest{Model: "unit/model"}, deployPlan{}))
			} else {
				d, err = s.Deploy(t.Context(), &DeployRequest{Model: "unit/model", Nodes: []string{n.node.Name()}, Force: true})
				require.NoError(t, err)
			}
			h.Progress(63, "weights", jobs.Bytes{Done: 63, Total: 100})
			h.Done()
			n.model.Downloads.CleanCompleted()
			require.Eventually(t, func() bool { return s.tracker.GetDeployment(d.ID).Status == constants.StatusCompleted }, time.Second, time.Millisecond)
			require.Empty(t, s.ListDeployments(t.Context(), true))
			got := s.GetDeployment(t.Context(), d.ID)
			require.Equal(t, constants.StatusCompleted, got.Nodes[0].Status)
			require.EqualValues(t, 63, got.Nodes[0].BytesDownloaded)
			require.EqualValues(t, 100, got.Nodes[0].BytesTotal)
			require.Zero(t, r.RequestsToPath("/zzrouter/v1/internal/jobs/"), "local subscriptions must use the registry")
		})
	}
}

func TestDeploymentCancelNeverUsesReusedDownloadKey(t *testing.T) {
	s := NewDeploymentsService(&fakeRouter{}, nil, nilAppsConfig)
	r := s.router.(*fakeRouter)
	d := s.tracker.CreateDeployment("m", "gguf", "hf", "", []string{"node"})
	require.NoError(t, s.tracker.UpdateNode(d.ID, "node", "", func(n *DeploymentNode) { n.DownloadID = "reused" }))
	require.NoError(t, s.tracker.UpdateNode(d.ID, "node", "", func(n *DeploymentNode) { n.JobID = "old-job" }))
	require.NoError(t, s.CancelDeployment(context.Background(), d.ID))
	require.Equal(t, 1, r.RequestsToPath("/zzrouter/v1/internal/jobs/old-job/cancel"))
	require.Zero(t, r.RequestsToPath("/zzrouter/v1/internal/deployments/stop"), "the key may now belong to another attempt")
}

func TestDeploymentCompletedBeforeObservationAndTeardown(t *testing.T) {
	reg := newTestJobsRegistry(t)
	s := NewDeploymentsService(&fakeRouter{}, nil, nilAppsConfig)
	s.jobs = &jobAccess{registry: reg}
	parent, cancel := context.WithCancel(t.Context())
	cancel()
	s.shutdown = func() context.Context { return parent }
	// Start replaces the constructor's cancelled context before acceptance.
	parent = t.Context()
	t.Cleanup(func() { s.Stop(context.Background()) })
	h, err := reg.StartDetached(jobs.KindDownload, "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { h.Fail(context.Canceled) })
	d := s.tracker.CreateDeployment("m", "", "hf", "", []string{"test"})
	s.acceptNode(t.Context(), d.ID, DeploymentNode{Node: "test", Source: "internet", JobID: h.ID(), Status: constants.StatusDownloading})
	h.Meta(jobs.Meta{"transfer_progress": 42.25, "transfer_speed": int64(123)})
	h.Progress(42, "weights", jobs.Bytes{Done: 42, Total: 100})
	require.Eventually(t, func() bool { return s.tracker.GetDeployment(d.ID).Nodes[0].Progress == 42.25 }, time.Second, time.Millisecond)
	require.EqualValues(t, 123, s.tracker.GetDeployment(d.ID).Nodes[0].Speed)
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stopCancel()
	s.Stop(stopCtx)
	require.Equal(t, constants.StatusFailed, s.tracker.GetDeployment(d.ID).Status)
	require.ErrorContains(t, s.tracker.WaitNode(t.Context(), d.ID, "test"), "outcome unconfirmed")
	blocked := s.tracker.CreateDeployment("blocked", "", "hf", "", []string{"test"})
	s.acceptNode(t.Context(), blocked.ID, DeploymentNode{Node: "test", Source: "internet", JobID: h.ID(), Status: constants.StatusDownloading})
	require.Equal(t, constants.StatusFailed, s.tracker.GetDeployment(blocked.ID).Status)
	s.Start()
	completed, err := reg.StartDetached(jobs.KindDownload, "", nil)
	require.NoError(t, err)
	completed.Progress(80, "weights", jobs.Bytes{Done: 80, Total: 100})
	completed.Progress(99, "install features", jobs.Bytes{})
	completed.Done()
	replay := s.tracker.CreateDeployment("replay", "", "hf", "", []string{"test"})
	s.acceptNode(t.Context(), replay.ID, DeploymentNode{Node: "test", Source: "internet", JobID: completed.ID(), Status: constants.StatusDownloading})
	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	require.NoError(t, s.tracker.WaitNode(ctx, replay.ID, "test"))
	require.EqualValues(t, 80, s.tracker.GetDeployment(replay.ID).Nodes[0].BytesDownloaded)
}

func TestDeploymentCancellationBeforeAcceptanceBindsOnlyItsJob(t *testing.T) {
	s := NewDeploymentsService(&fakeRouter{}, nil, nilAppsConfig)
	d := s.tracker.CreateDeployment("m", "", "hf", "", []string{"node"})
	require.NoError(t, s.CancelDeployment(t.Context(), d.ID))
	s.acceptNode(t.Context(), d.ID, DeploymentNode{Node: "node", Source: "internet", DownloadID: "reused", JobID: "late-job", Status: constants.StatusDownloading})
	got := s.tracker.GetDeployment(d.ID)
	require.Equal(t, constants.StatusCancelled, got.Status)
	require.Equal(t, constants.StatusCancelled, got.Nodes[0].Status)
	require.Equal(t, "late-job", got.Nodes[0].JobID)
	r := s.router.(*fakeRouter)
	require.Equal(t, 1, r.RequestsToPath("/zzrouter/v1/internal/jobs/late-job/cancel"))
	require.Zero(t, r.RequestsToPath("/zzrouter/v1/internal/deployments/stop"))
	require.Error(t, s.tracker.WaitNode(t.Context(), d.ID, "node"))
}

func TestDeploymentLocalSelectorPreservesCanonicalWorker(t *testing.T) {
	n := createTestNodeWithDefaults(t)
	s := n.services.Deployments
	s.appsConfig = nilAppsConfig
	h, err := n.jobs.StartDetached(jobs.KindDownload, "", nil)
	require.NoError(t, err)
	r := &fakeRouter{perPath: map[string]struct {
		status int
		body   []byte
	}{"/zzrouter/v1/internal/deployments": {202, mustJSON(t, internalDeploymentResult{Key: "key", Node: n.node.Name(), Status: "downloading", JobID: h.ID()})}}}
	s.router = r
	d, err := s.Deploy(t.Context(), &DeployRequest{Model: "unit/model", Nodes: []string{"localhost"}, Force: true})
	require.NoError(t, err)
	h.Done()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, s.tracker.WaitNode(ctx, d.ID, "localhost"))
}

func TestDeploymentWaitUsesRetainedFastNodeAfterJobExpiry(t *testing.T) {
	reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System(), CompletedTTL: 10 * time.Millisecond, JanitorInterval: time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(reg.Stop)
	s := NewDeploymentsService(&fakeRouter{}, nil, nilAppsConfig)
	s.jobs = &jobAccess{registry: reg}
	t.Cleanup(func() { s.Stop(context.Background()) })
	d := s.tracker.CreateDeployment("m", "", "hf", "", []string{"fast", "slow"})
	fast, err := reg.StartDetached(jobs.KindDownload, "", nil)
	require.NoError(t, err)
	slow, err := reg.StartDetached(jobs.KindDownload, "", nil)
	require.NoError(t, err)
	s.acceptNode(t.Context(), d.ID, DeploymentNode{Node: "fast", Source: "internet", JobID: fast.ID(), Status: constants.StatusDownloading})
	s.acceptNode(t.Context(), d.ID, DeploymentNode{Node: "slow", Source: "internet", JobID: slow.ID(), Status: constants.StatusDownloading})
	fast.Done()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, s.tracker.WaitNode(ctx, d.ID, "fast"))
	require.Eventually(t, func() bool { _, err := reg.Get(fast.ID()); return err != nil }, time.Second, time.Millisecond)
	slow.Done()
	require.NoError(t, s.tracker.WaitNode(ctx, d.ID, "slow"))
	require.NoError(t, s.tracker.WaitNode(ctx, d.ID, "fast"), "fast terminal retained after registry eviction")
	require.Equal(t, constants.StatusCompleted, s.tracker.GetDeployment(d.ID).Status)
}
