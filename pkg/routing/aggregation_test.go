package routing

import (
	"encoding/json"
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
)

func nodeResp(name, body string) *mesh.NodeResponse {
	return &mesh.NodeResponse{
		Node:     name,
		NodeName: name,
		Response: &mesh.Response{StatusCode: 200, Body: []byte(body)},
	}
}

// aggregatedNames pulls the "name" of every item out of an aggregated body.
func aggregatedNames(t *testing.T, body []byte, field string) []string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("aggregated body is not an object: %v", err)
	}
	items, ok := out[field].([]any)
	if !ok {
		t.Fatalf("aggregated body has no %q array: %s", field, body)
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item is not an object: %v", it)
		}
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	return names
}

// A worker answering with the standard {"data": [...]} envelope used to be
// discarded: the mesh layer stripped the envelope and this layer then looked
// for "data" inside the stripped item. GET /nodes/compatible could therefore
// only ever report the coordinator, however many workers matched.
func TestAggregateKeepsWorkersUsingDataEnvelope(t *testing.T) {
	t.Parallel()
	r := &ClusterAwareRouter{}

	local := &Response{StatusCode: 200, Body: []byte(`{"data":[{"name":"coordinator"}]}`)}
	cluster := &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{
		nodeResp("worker-a", `{"data":[{"name":"worker-a"}]}`),
		nodeResp("worker-b", `{"data":[{"name":"worker-b"}]}`),
	}}

	out, err := r.aggregateResponses(local, cluster)
	if err != nil {
		t.Fatalf("aggregateResponses: %v", err)
	}

	got := aggregatedNames(t, out.Body, "data")
	if len(got) != 3 {
		t.Fatalf("expected all 3 nodes, got %d: %s", len(got), out.Body)
	}
	for _, want := range []string{"coordinator", "worker-a", "worker-b"} {
		if !contains(got, want) {
			t.Errorf("%s missing from aggregate: %v", want, got)
		}
	}
}

// The coordinator is not always a candidate. When only workers match, their
// field name has to be discovered from the worker responses themselves.
func TestAggregateDetectsFieldFromWorkersWhenLocalIsEmpty(t *testing.T) {
	t.Parallel()
	r := &ClusterAwareRouter{}

	local := &Response{StatusCode: 200, Body: []byte(`{"data":[]}`)}
	cluster := &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{
		nodeResp("worker-a", `{"data":[{"name":"worker-a"}]}`),
	}}

	out, err := r.aggregateResponses(local, cluster)
	if err != nil {
		t.Fatalf("aggregateResponses: %v", err)
	}
	if got := aggregatedNames(t, out.Body, "data"); len(got) != 1 || got[0] != "worker-a" {
		t.Fatalf("expected only worker-a, got %v: %s", got, out.Body)
	}
}

// Named-field endpoints (the internal model list) must keep working.
func TestAggregateKeepsWorkersUsingNamedField(t *testing.T) {
	t.Parallel()
	r := &ClusterAwareRouter{}

	local := &Response{StatusCode: 200, Body: []byte(`{"models":[{"name":"local-model"}]}`)}
	cluster := &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{
		nodeResp("worker-a", `{"models":[{"name":"worker-model"}],"resources":{"node_name":"worker-a"}}`),
	}}

	out, err := r.aggregateResponses(local, cluster)
	if err != nil {
		t.Fatalf("aggregateResponses: %v", err)
	}
	if got := aggregatedNames(t, out.Body, "models"); len(got) != 2 {
		t.Fatalf("expected 2 models, got %v: %s", got, out.Body)
	}
}

// A single object in "data" is a payload, not a list, and merging it would
// corrupt the response.
func TestAggregateLeavesSingleObjectResponsesAlone(t *testing.T) {
	t.Parallel()
	r := &ClusterAwareRouter{}

	local := &Response{StatusCode: 200, Body: []byte(`{"data":{"name":"one-thing"}}`)}
	out, err := r.aggregateResponses(local, nil)
	if err != nil {
		t.Fatalf("aggregateResponses: %v", err)
	}
	if string(out.Body) != `{"data":{"name":"one-thing"}}` {
		t.Fatalf("single-object response was rewritten: %s", out.Body)
	}
}

// A node that could not be reached must not be mistaken for a node that
// answered with nothing.
func TestAggregateSkipsFailedNodes(t *testing.T) {
	t.Parallel()
	r := &ClusterAwareRouter{}

	local := &Response{StatusCode: 200, Body: []byte(`{"data":[{"name":"coordinator"}]}`)}
	cluster := &mesh.BroadcastResponse{Responses: []*mesh.NodeResponse{
		{Node: "worker-a", Error: errTest},
		nodeResp("worker-b", `{"data":[{"name":"worker-b"}]}`),
	}}

	out, err := r.aggregateResponses(local, cluster)
	if err != nil {
		t.Fatalf("aggregateResponses: %v", err)
	}
	if got := aggregatedNames(t, out.Body, "data"); len(got) != 2 {
		t.Fatalf("expected coordinator + worker-b, got %v", got)
	}
}

var errTest = &testError{"unreachable"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
