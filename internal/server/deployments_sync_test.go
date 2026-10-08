package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/routing"
)

// fakeRouter captures every Route call so tests can assert on the
// partition + dispatch behavior without spinning up a real cluster.
type fakeRouter struct {
	calls []routing.Request
	// perPath maps path → (status, body). Missing paths return 500.
	perPath map[string]struct {
		status int
		body   []byte
	}
}

func (r *fakeRouter) Route(_ context.Context, req *routing.Request) (*routing.Response, error) {
	r.calls = append(r.calls, *req)
	hit, ok := r.perPath[req.Path]
	if !ok {
		return &routing.Response{StatusCode: 500, Body: []byte("unmapped")}, nil
	}
	return &routing.Response{StatusCode: hit.status, Body: hit.body}, nil
}

func (r *fakeRouter) Unicast(_ context.Context, _, _, _ string, _ []byte) (*routing.Response, error) {
	return &routing.Response{StatusCode: 500}, nil
}

func (r *fakeRouter) Broadcast(_ context.Context, _, _ string, _ []byte) (*routing.Response, error) {
	return &routing.Response{StatusCode: 500}, nil
}

func (r *fakeRouter) MultiRoute(_ context.Context, _, _ string, _ map[string][]byte) (*routing.Response, error) {
	return &routing.Response{StatusCode: 500}, nil
}

func (r *fakeRouter) RequestsToPath(path string) int {
	n := 0
	for _, c := range r.calls {
		if c.Path == path || (len(c.Path) >= len(path) && c.Path[:len(path)] == path) {
			n++
		}
	}
	return n
}

func TestPartitionByModelPresence_BucketsCorrectly(t *testing.T) {
	exists := mustJSON(t, map[string]any{"exists": true, "model": "m", "format": "gguf"})
	absent := mustJSON(t, map[string]any{"exists": false, "model": "m", "format": "gguf"})

	// partitionByModelPresence probes the same path for each node; we
	// discriminate via a per-node override by re-routing in Route().
	// Simpler: use a per-node map field.
	byNode := map[string][]byte{
		"nodeA": exists,
		"nodeB": absent,
		"nodeC": exists,
	}
	routed := &perNodeRouter{responses: byNode}
	s := &DeploymentsService{router: routed}

	haves, needs := s.partitionByModelPresence(context.Background(), []string{"nodeA", "nodeB", "nodeC"}, "m", "gguf", nil)
	assert.Equal(t, []string{"nodeA", "nodeC"}, haves)
	assert.Equal(t, []string{"nodeB"}, needs)
}

func TestPartitionByModelPresence_ErrorTreatedAsNeed(t *testing.T) {
	r := &fakeRouter{perPath: map[string]struct {
		status int
		body   []byte
	}{}} // no mapping → 500
	s := &DeploymentsService{router: r}
	haves, needs := s.partitionByModelPresence(context.Background(), []string{"nodeX"}, "m", "gguf", nil)
	assert.Empty(t, haves)
	assert.Equal(t, []string{"nodeX"}, needs, "probe errors must fall through to a registry pull on the target")
}

func TestDispatchSyncDeploy_ReturnsFalseWithoutResolver(t *testing.T) {
	s := &DeploymentsService{router: &fakeRouter{}}
	ok := s.dispatchSyncDeploy(context.Background(), "dep-1", "nodeB", "nodeA", &DeployRequest{Model: "m"}, deployPlan{})
	assert.False(t, ok, "no resolveNodeURL wired → must decline peer sync")
}

func TestDispatchSyncDeploy_ReturnsFalseWhenResolverBlank(t *testing.T) {
	s := &DeploymentsService{
		router:         &fakeRouter{},
		resolveNodeURL: func(string) string { return "" },
	}
	ok := s.dispatchSyncDeploy(context.Background(), "dep-1", "nodeB", "nodeA", &DeployRequest{Model: "m"}, deployPlan{})
	assert.False(t, ok, "resolver returned empty URL → decline peer sync")
}

func TestDispatchSyncDeploy_PopulatesTrackerOnSuccess(t *testing.T) {
	r := &fakeRouter{perPath: map[string]struct {
		status int
		body   []byte
	}{
		"/zzrouter/v1/internal/sync/deploy": {
			status: 202,
			body: mustJSON(t, map[string]any{
				"status":       "accepted",
				"model":        "m",
				"format":       "gguf",
				"source":       "nodeA",
				"job_id":       "dep-1",
				"local_job_id": "local-abc",
				"message":      "sync started",
			}),
		},
	}}
	s := &DeploymentsService{
		router:         r,
		tracker:        NewDeploymentTracker(),
		resolveNodeURL: func(node string) string { return "https://" + node + ":9090" },
	}
	d := s.tracker.CreateDeployment("m", "gguf", "hf", "", []string{"nodeA", "nodeB"})
	ok := s.dispatchSyncDeploy(context.Background(), d.ID, "nodeB", "nodeA", &DeployRequest{Model: "m", Format: "gguf"}, deployPlan{})
	require.True(t, ok)

	got := s.tracker.GetDeployment(d.ID)
	require.NotNil(t, got)
	var nodeB *DeploymentNode
	for i := range got.Nodes {
		if got.Nodes[i].Node == "nodeB" {
			nodeB = &got.Nodes[i]
		}
	}
	require.NotNil(t, nodeB)
	assert.Equal(t, "local-abc", nodeB.JobID, "tracker must carry the worker's local_job_id for subscribe")
	assert.Equal(t, "nodeA", nodeB.Source, "tracker must record the peer source")
}

// --- helpers ---

type perNodeRouter struct {
	responses map[string][]byte
	calls     []routing.Request
}

func (r *perNodeRouter) Route(_ context.Context, req *routing.Request) (*routing.Response, error) {
	r.calls = append(r.calls, *req)
	body, ok := r.responses[req.Node]
	if !ok {
		return &routing.Response{StatusCode: 500, Body: []byte("unmapped node")}, nil
	}
	return &routing.Response{StatusCode: 200, Body: body}, nil
}

func (r *perNodeRouter) Unicast(_ context.Context, _, _, _ string, _ []byte) (*routing.Response, error) {
	return &routing.Response{StatusCode: 500}, nil
}

func (r *perNodeRouter) Broadcast(_ context.Context, _, _ string, _ []byte) (*routing.Response, error) {
	return &routing.Response{StatusCode: 500}, nil
}

func (r *perNodeRouter) MultiRoute(_ context.Context, _, _ string, _ map[string][]byte) (*routing.Response, error) {
	return &routing.Response{StatusCode: 500}, nil
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
