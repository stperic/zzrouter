package harness

import (
	"context"
	"errors"
	"testing"
)

func TestExternalBackend_Provisions(t *testing.T) {
	t.Setenv("ZZROUTER_ADMIN_API_KEY", "admin-key-123")
	t.Setenv("ZZROUTER_CLUSTER_NETWORK_KEY", "cluster-key-456")

	cfg := Local()
	cfg.applyDefaults()
	cfg.Backend = "" // use external backend (no inproc subpackage in harness/_test)
	cluster, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := cluster.Provision(context.Background()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer func() {
		// A teardown that fails leaks a provisioned cluster in the lab; saying so
		// beats discovering it as the next run's mystery port conflict.
		if err := cluster.Teardown(context.Background()); err != nil {
			t.Errorf("Teardown: %v", err)
		}
	}()

	coord := cluster.Coordinator()
	if coord == nil {
		t.Fatal("nil coordinator")
	}
	if coord.AdminKey() != "admin-key-123" {
		t.Errorf("AdminKey: got %q", coord.AdminKey())
	}
	if coord.ClusterKey() != "cluster-key-456" {
		t.Errorf("ClusterKey: got %q", coord.ClusterKey())
	}
	if coord.BaseURL() == "" {
		t.Error("empty BaseURL")
	}
	workers := cluster.Workers()
	if len(workers) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(workers))
	}
	if !workers[0].HasTag("inproc") {
		t.Error("worker tag not propagated")
	}
}

func TestNodeByName_Unknown(t *testing.T) {
	t.Setenv("ZZROUTER_ADMIN_API_KEY", "x")
	t.Setenv("ZZROUTER_CLUSTER_NETWORK_KEY", "y")
	cfg := Local()
	cfg.Backend = ""
	cluster, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Provision(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.NodeByName("ghost"); err == nil {
		t.Fatal("expected error for unknown node")
	}
}

func TestProvision_DoubleCallErrors(t *testing.T) {
	t.Setenv("ZZROUTER_ADMIN_API_KEY", "x")
	t.Setenv("ZZROUTER_CLUSTER_NETWORK_KEY", "y")
	cfg := Local()
	cfg.Backend = ""
	cluster, _ := New(cfg)
	if err := cluster.Provision(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := cluster.Provision(context.Background())
	if !errors.Is(err, ErrAlreadyProvisioned) {
		t.Fatalf("want ErrAlreadyProvisioned, got %v", err)
	}
}

func TestNewNode_RejectsEmpty(t *testing.T) {
	if _, err := NewNode("", "http://x", RoleWorker, nil, NodeKeys{}); err == nil {
		t.Error("empty name should error")
	}
	if _, err := NewNode("n", "", RoleWorker, nil, NodeKeys{}); err == nil {
		t.Error("empty baseURL should error")
	}
}

func TestNode_TagsDefensiveCopy(t *testing.T) {
	n, err := NewNode("n", "http://x:9090", RoleWorker, []string{"a", "b"},
		NodeKeys{Admin: "ak", Cluster: "ck"})
	if err != nil {
		t.Fatal(err)
	}
	tags := n.Tags()
	tags[0] = "MUTATED"
	if n.Tags()[0] != "a" {
		t.Error("Tags() not defensive-copied on read")
	}
}
