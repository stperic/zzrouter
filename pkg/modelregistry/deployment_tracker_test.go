package modelregistry

import (
	"encoding/json"
	"testing"

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

	require.NoError(t, tr.UpdateNodeJobID(d.ID, "w1", "dl_abc123"))

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
	err := tr.UpdateNodeJobID(d.ID, "ghost", "dl_x")
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
