package server

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// TestNodeIdentity_IsKnownNode_StaticOnly pins the static behavior so a
// registry regression doesn't accidentally break standalone mode.
func TestNodeIdentity_IsKnownNode_StaticOnly(t *testing.T) {
	id := NewNodeIdentity(&pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Name: "coord",
			Bind: "198.51.100.10",
			Port: 9090,
		},
		Cluster: pkgConfig.ClusterConfig{
			Endpoints: pkgConfig.PeersFromAddresses("192.0.2.10:9090"),
		},
	})

	cases := []struct {
		name string
		want bool
	}{
		{"", true},                   // empty = "all nodes"
		{"*", true},                  // wildcard
		{"coord", true},              // configured name
		{"198.51.100.10:9090", true}, // bind:port
		{"192.0.2.10:9090", true},    // configured endpoint
		{"192.0.2.10", true},         // endpoint host (port stripped)
		{"worker-1", false},          // not in static config
		{"unknown", false},
	}
	for _, tc := range cases {
		if got := id.IsKnownNode(tc.name); got != tc.want {
			t.Errorf("IsKnownNode(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMeshAwareNodeValidator_RegistryFallback exercises the runtime
// registry consultation. A name not in static config but registered via
// the mesh registry should pass IsKnownNode.
func TestMeshAwareNodeValidator_RegistryFallback(t *testing.T) {
	id := NewNodeIdentity(&pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{Name: "coord", Bind: "198.51.100.10", Port: 9090},
	})
	endpoints := []*mesh.Endpoint{
		{
			URL:      "http://192.0.2.10:9090",
			Name:     "worker-1",
			NodeName: "worker-1",
			Alias:    "linux-gpu",
		},
		{
			URL:      "http://203.0.113.77:9090",
			Name:     "windows-worker",
			NodeName: "windows-worker",
		},
	}
	v := &MeshAwareNodeValidator{
		NodeIdentity: id,
		EndpointsFn:  func() []*mesh.Endpoint { return endpoints },
	}

	cases := []struct {
		name string
		want bool
	}{
		// Static path (NodeIdentity coverage).
		{"coord", true},
		{"", true},

		// Registry path — by name.
		{"worker-1", true},
		{"windows-worker", true},

		// Registry path — by alias.
		{"linux-gpu", true},

		// Registry path — by URL (full).
		{"http://192.0.2.10:9090", true},

		// Registry path — by URL host+port (no scheme).
		{"192.0.2.10:9090", true},
		{"203.0.113.77:9090", true},

		// Registry path — by bare host (no scheme, no port).
		{"192.0.2.10", true},
		{"203.0.113.77", true},

		// Unknowns.
		{"never-paired", false},
		{"http://1.2.3.4:80", false},
		{"1.2.3.4", false},
	}
	for _, tc := range cases {
		if got := v.IsKnownNode(tc.name); got != tc.want {
			t.Errorf("IsKnownNode(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMeshAwareNodeValidator_NilEndpointsFn ensures the wrapper degrades
// gracefully to NodeIdentity-only behavior on workers (no registry).
func TestMeshAwareNodeValidator_NilEndpointsFn(t *testing.T) {
	id := NewNodeIdentity(&pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{Name: "worker", Bind: "192.0.2.10", Port: 9090},
	})
	v := &MeshAwareNodeValidator{NodeIdentity: id, EndpointsFn: nil}
	if !v.IsKnownNode("worker") {
		t.Error("static name should still match")
	}
	if v.IsKnownNode("paired-name") {
		t.Error("nil EndpointsFn should NOT consult runtime — got false-positive")
	}
}

// TestMeshAwareNodeValidator_EmptyEndpointsFn pins the realistic worker
// shape: GetClusterEndpoints returns nil/empty on workers (hosts.go:499).
// The wrapper must degrade to static-only behavior identically.
func TestMeshAwareNodeValidator_EmptyEndpointsFn(t *testing.T) {
	id := NewNodeIdentity(&pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{Name: "worker", Bind: "192.0.2.10", Port: 9090},
	})
	cases := []func() []*mesh.Endpoint{
		func() []*mesh.Endpoint { return nil },
		func() []*mesh.Endpoint { return []*mesh.Endpoint{} },
	}
	for i, fn := range cases {
		v := &MeshAwareNodeValidator{NodeIdentity: id, EndpointsFn: fn}
		if !v.IsKnownNode("worker") {
			t.Errorf("case %d: static name should still match", i)
		}
		if v.IsKnownNode("paired-name") {
			t.Errorf("case %d: empty EndpointsFn should NOT consult runtime", i)
		}
	}
}

// TestMeshAwareNodeValidator_KnownNodes_UnionAndAliases pins the
// diagnostic surface: every form a peer is reachable by appears in
// KnownNodes (Name, Alias, URL, host:port, host) — but each at most once.
func TestMeshAwareNodeValidator_KnownNodes_UnionAndAliases(t *testing.T) {
	id := NewNodeIdentity(&pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{Name: "coord", Bind: "198.51.100.10", Port: 9090},
	})
	endpoints := []*mesh.Endpoint{
		{URL: "http://192.0.2.10:9090", Name: "worker-1", NodeName: "worker-1", Alias: "linux-gpu"},
		{URL: "http://1.1.1.1:9090", Name: "coord", NodeName: "coord"}, // dup of static name
	}
	v := &MeshAwareNodeValidator{
		NodeIdentity: id,
		EndpointsFn:  func() []*mesh.Endpoint { return endpoints },
	}
	got := v.KnownNodes()
	seen := map[string]int{}
	for _, n := range got {
		seen[n]++
	}
	for _, n := range got {
		if seen[n] > 1 {
			t.Errorf("KnownNodes() duplicates %q: %v", n, got)
		}
	}
	for _, must := range []string{"worker-1", "linux-gpu", "http://192.0.2.10:9090", "192.0.2.10:9090", "192.0.2.10"} {
		if seen[must] == 0 {
			t.Errorf("KnownNodes() missing %q: %v", must, got)
		}
	}
}
