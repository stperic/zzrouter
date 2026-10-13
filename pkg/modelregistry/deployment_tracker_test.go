package modelregistry

import (
	"context"
	"encoding/json"

	"testing"

	"github.com/stperic/zzrouter/pkg/constants"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateNodeJobID — tracker persists the pkg/jobs handle reported
// by the worker's internal deploy path and surfaces it as job_id in
// JSON. Used by clients (TUI, --watch CLI) to subscribe via
// /zzrouter/v1/jobs/:id/stream?node=<node>.
func TestUpdateNodeJobID(t *testing.T) {
	tr := NewDeploymentTracker()
	d := tr.CreateDeployment("repo/model", "", "hf", "", []string{"w1"})

	require.NoError(t, tr.UpdateNode(d.ID, "w1", "", func(n *DeploymentNode) { n.JobID = "dl_abc123" }))

	got := tr.GetDeployment(d.ID)
	require.NotNil(t, got)
	require.Len(t, got.Nodes, 1)
	assert.Equal(t, "dl_abc123", got.Nodes[0].JobID)

	// JSON tag must be job_id — wire contract for --watch clients.
	b, err := json.Marshal(got.Nodes[0])
	require.NoError(t, err)
	assert.Contains(t, string(b), `"job_id":"dl_abc123"`)
}

// TestUpdateNodeJobID_UnknownNode — error when the node isn't in the
// deployment (idempotent-safe caller signal).
func TestUpdateNodeJobID_UnknownNode(t *testing.T) {
	tr := NewDeploymentTracker()
	d := tr.CreateDeployment("m", "", "hf", "", []string{"w1"})
	err := tr.UpdateNode(d.ID, "ghost", "", func(n *DeploymentNode) { n.JobID = "dl_x" })
	assert.Error(t, err)
}

// TestDeploymentNode_JobID_OmitEmpty — empty JobID must not appear in
// JSON. Guards against leaking placeholder strings in stream/list
// responses for dedupe-early-return deployments.
func TestDeploymentNode_JobID_OmitEmpty(t *testing.T) {
	n := DeploymentNode{Node: "w1"}
	b, err := json.Marshal(n)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "job_id")
}

func TestDeploymentAtomicOwnershipAndTerminalOutcome(t *testing.T) {
	tr := NewDeploymentTracker()
	d := tr.CreateDeployment("m", "", "hf", "", []string{"a", "b"})
	d.Nodes[0].Status = constants.StatusCompleted
	require.Equal(t, constants.StatusPending, tr.GetDeployment(d.ID).Nodes[0].Status, "creation must return an independent snapshot")
	require.NoError(t, tr.UpdateNode(d.ID, "a", "", func(n *DeploymentNode) { n.JobID, n.Status = "fast-job", constants.StatusCompleted }))
	require.Error(t, tr.UpdateNode(d.ID, "a", "other-job", func(n *DeploymentNode) { n.Status = constants.StatusFailed }))
	require.Error(t, tr.UpdateNode(d.ID, "a", "fast-job", func(n *DeploymentNode) { n.Source = "other-node" }))
	require.NoError(t, tr.UpdateNode(d.ID, "a", "fast-job", func(n *DeploymentNode) { n.Status = constants.StatusDownloading }))
	require.Equal(t, constants.StatusCompleted, tr.GetDeployment(d.ID).Nodes[0].Status)
	require.NoError(t, tr.WaitNode(t.Context(), d.ID, "a"), "retained completed node does not need its expired worker job")
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- tr.WaitNode(ctx, d.ID, "b") }()
	require.NoError(t, tr.UpdateNode(d.ID, "b", "", func(n *DeploymentNode) { n.Status, n.Error = constants.StatusFailed, "tracking failed" }))
	require.ErrorContains(t, <-result, "tracking failed")
	cancel()
	require.Empty(t, tr.ListDeployments(true))
	require.NoError(t, tr.CancelDeployment(d.ID))
	require.Error(t, tr.WaitNode(t.Context(), d.ID, "a"), "aggregate cancellation prevents launching even from an already completed node")
}
