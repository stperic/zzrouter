package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

// deployServiceWithResult builds a service whose internal deploy endpoint
// answers with the given body, so Deploy's handling of a node's reported
// status can be exercised without a cluster.
func deployServiceWithResult(t *testing.T, body []byte) *DeploymentsService {
	t.Helper()
	r := &fakeRouter{perPath: map[string]struct {
		status int
		body   []byte
	}{
		"/zzrouter/v1/internal/deployments": {status: 200, body: body},
	}}
	return &DeploymentsService{
		router:     r,
		tracker:    NewDeploymentTracker(),
		appsConfig: func() *pkgConfig.AppsConfig { return nil },
	}
}

func TestDeploy_TerminalNodeResultCompletesDeployment(t *testing.T) {
	// A cloud model has nothing to download: the node registers it with
	// the provider and reports completed on the spot.
	s := deployServiceWithResult(t, mustJSON(t, map[string]any{
		"key":     "macbook-pro/openrouter/google/gemini-3.7-flash",
		"model":   "google/gemini-3.7-flash",
		"status":  string(constants.StatusCompleted),
		"message": "Model registered with cloud provider 'openrouter'",
	}))

	d, err := s.Deploy(context.Background(), &DeployRequest{
		Model:    "google/gemini-3.7-flash",
		Registry: "openrouter",
		Nodes:    []string{"macbook-pro"},
	})
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.Equal(t, constants.StatusCompleted, d.Status,
		"a node that answered terminally must not leave the deployment in flight")
	require.Len(t, d.Nodes, 1)
	assert.Equal(t, constants.StatusCompleted, d.Nodes[0].Status)
	assert.Empty(t, d.Nodes[0].Error, "a completed node carries no error text")
	assert.Equal(t, "macbook-pro/openrouter/google/gemini-3.7-flash", d.Nodes[0].DownloadID)
}

func TestDeploy_FailedNodeResultCarriesMessage(t *testing.T) {
	s := deployServiceWithResult(t, mustJSON(t, map[string]any{
		"key":     "macbook-pro/openrouter/some-model",
		"model":   "some-model",
		"status":  string(constants.StatusFailed),
		"message": "no credentials configured",
	}))

	d, err := s.Deploy(context.Background(), &DeployRequest{
		Model:    "some-model",
		Registry: "openrouter",
		Nodes:    []string{"macbook-pro"},
	})
	require.NoError(t, err)
	require.Len(t, d.Nodes, 1)

	assert.Equal(t, constants.StatusFailed, d.Status)
	assert.Equal(t, "no credentials configured", d.Nodes[0].Error)
}

func TestDeploy_RunningDownloadStaysInFlight(t *testing.T) {
	s := deployServiceWithResult(t, mustJSON(t, map[string]any{
		"key":    "macbook-pro/huggingface/org/model",
		"model":  "org/model",
		"status": string(constants.StatusDownloading),
		"job_id": "job-1",
	}))

	d, err := s.Deploy(context.Background(), &DeployRequest{
		Model:    "org/model",
		Registry: "huggingface",
		Nodes:    []string{"macbook-pro"},
	})
	require.NoError(t, err)
	require.Len(t, d.Nodes, 1)

	assert.Equal(t, constants.StatusDownloading, d.Status,
		"a live producer keeps the deployment in flight")
	assert.Equal(t, constants.StatusDownloading, d.Nodes[0].Status)
	assert.Equal(t, "job-1", d.Nodes[0].JobID)
}

func TestAggregateStatus_NodeRollUp(t *testing.T) {
	// A peer-sync node holds "syncing", which UpdateSummary has no bucket
	// for. Reading the counters alone would call this deployment failed the
	// moment it was dispatched.
	t.Run("a non-terminal node keeps the deployment in flight", func(t *testing.T) {
		tracker := NewDeploymentTracker()
		d := tracker.CreateDeployment("m", "gguf", "hf", "", []string{"a", "b"})
		require.NoError(t, tracker.UpdateNodeStatus(d.ID, "a", constants.StatusCompleted, ""))
		require.NoError(t, tracker.UpdateNodeStatus(d.ID, "b", constants.Status(syncStatusLabel), "syncing from a"))

		status, message := aggregateStatus(tracker.GetDeployment(d.ID))
		assert.Equal(t, constants.StatusDownloading, status)
		assert.Contains(t, message, "1/2")
	})

	t.Run("partial failure is a failure", func(t *testing.T) {
		tracker := NewDeploymentTracker()
		d := tracker.CreateDeployment("m", "gguf", "hf", "", []string{"a", "b"})
		require.NoError(t, tracker.UpdateNodeStatus(d.ID, "a", constants.StatusCompleted, ""))
		require.NoError(t, tracker.UpdateNodeStatus(d.ID, "b", constants.StatusFailed, "boom"))

		status, message := aggregateStatus(tracker.GetDeployment(d.ID))
		assert.Equal(t, constants.StatusFailed, status,
			"POST and the next GET must not disagree on a partial failure")
		assert.Contains(t, message, "1/2 nodes failed")
	})

	t.Run("all skipped says so rather than claiming a deployment", func(t *testing.T) {
		tracker := NewDeploymentTracker()
		d := tracker.CreateDeployment("m", "gguf", "hf", "", []string{"a", "b"})
		require.NoError(t, tracker.UpdateNodeStatus(d.ID, "a", constants.StatusSkipped, ""))
		require.NoError(t, tracker.UpdateNodeStatus(d.ID, "b", constants.StatusSkipped, ""))

		_, message := aggregateStatus(tracker.GetDeployment(d.ID))
		assert.Contains(t, message, "skipped")
	})

	t.Run("a user cancel survives a late merge", func(t *testing.T) {
		tracker := NewDeploymentTracker()
		d := tracker.CreateDeployment("m", "gguf", "hf", "", []string{"a"})
		require.NoError(t, tracker.UpdateDeploymentStatus(d.ID, constants.StatusCancelled, "cancelled by user"))
		require.NoError(t, tracker.UpdateNodeStatus(d.ID, "a", constants.StatusCompleted, ""))

		status, _ := aggregateStatus(tracker.GetDeployment(d.ID))
		assert.Equal(t, constants.StatusCancelled, status)
	})
}

func TestDeploy_NodeMayNotClaimCancelled(t *testing.T) {
	// cancelled is deployment-level only; a node reporting it must not be
	// recorded as terminal, or a cancel could be forged from below.
	s := deployServiceWithResult(t, mustJSON(t, map[string]any{
		"key":    "macbook-pro/huggingface/org/model",
		"model":  "org/model",
		"status": string(constants.StatusCancelled),
	}))

	d, err := s.Deploy(context.Background(), &DeployRequest{
		Model:    "org/model",
		Registry: "huggingface",
		Nodes:    []string{"macbook-pro"},
	})
	require.NoError(t, err)
	require.Len(t, d.Nodes, 1)
	assert.Equal(t, constants.StatusDownloading, d.Nodes[0].Status)
}
