package harness_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/test/e2e/harness"
)

// TestFetchNodes_HappyPath asserts the wire shape decode + name
// extraction. Uses httptest because FetchNodes is pure library; no
// SSH backend or real cluster needed.
func TestFetchNodes_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zzrouter/v1/nodes" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != "admin-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"name": "coord", "role": "coordinator", "is_local": true, "health_status": "healthy"},
				{"name": "worker-1", "role": "worker", "health_status": "healthy", "address": "192.0.2.10"},
			},
			"total": 2,
		})
	}))
	defer srv.Close()

	nodes, err := harness.FetchNodes(context.Background(), srv.Client(), srv.URL, "admin-key")
	if err != nil {
		t.Fatalf("FetchNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("len(nodes)=%d want 2", len(nodes))
	}
	if nodes[0].Name != "coord" || !nodes[0].IsLocal {
		t.Errorf("nodes[0]=%+v", nodes[0])
	}
	if nodes[1].Name != "worker-1" || nodes[1].Address != "192.0.2.10" {
		t.Errorf("nodes[1]=%+v", nodes[1])
	}
}

// TestFetchNodes_AuthFailure pins the error path so a 401 surfaces a
// distinguishable error to the test (not a silent empty list).
func TestFetchNodes_AuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := harness.FetchNodes(context.Background(), srv.Client(), srv.URL, "")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error doesn't carry status: %v", err)
	}
}

// TestRequireNodes_Missing checks the assertion helper used by Ring 2
// suite() to fail fast when manual pairing is incomplete.
func TestRequireNodes_Missing(t *testing.T) {
	have := []harness.NodeInfo{{Name: "coord"}}
	err := harness.RequireNodes(have, []string{"coord", "worker-1"})
	if err == nil {
		t.Fatal("expected missing-node error")
	}
	if !strings.Contains(err.Error(), "worker-1") {
		t.Errorf("error doesn't name missing worker: %v", err)
	}
}

// TestRequireNodes_CaseInsensitive mirrors the server's
// strings.EqualFold behavior in ListNodes — names from the operator's
// site YAML may not match the registry's canonical case exactly.
func TestRequireNodes_CaseInsensitive(t *testing.T) {
	have := []harness.NodeInfo{{Name: "Worker-1"}}
	if err := harness.RequireNodes(have, []string{"worker-1"}); err != nil {
		t.Fatalf("expected case-insensitive match: %v", err)
	}
}

// TestRequireNodes_EmptyWant is the no-op base case — callers may
// invoke RequireNodes on a topology with no workers and shouldn't see
// a spurious error.
func TestRequireNodes_EmptyWant(t *testing.T) {
	if err := harness.RequireNodes(nil, nil); err != nil {
		t.Fatalf("empty want should be no-op, got %v", err)
	}
}
