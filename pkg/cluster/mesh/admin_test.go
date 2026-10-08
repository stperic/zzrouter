package mesh

import (
	"strings"
	"testing"
)

// TestRegisterAdminEndpoint_DerivesClusterURL pins the post-af3e7214
// invariant: any endpoint admitted at runtime (e.g. via the pairing
// accept handler) must carry both URL (admin, registry key) and
// ClusterURL (mTLS, dispatch + probe target). Without ClusterURL,
// dispatch falls back to the admin URL — which is loopback-only on
// every paired worker since af3e7214 narrowed the bind.
//
// Mirrors the startup-from-config path (cluster.go:88) which has
// always populated ClusterURL via DeriveClusterURL.
func TestRegisterAdminEndpoint_DerivesClusterURL(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Enabled:  true,
		BindPort: 9091,
	}
	cluster, err := NewCluster(cfg, "http://localhost:9090", &MockLocalHandler{}, nil, nil)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}

	const adminURL = "http://192.0.2.10:9090"
	if err := cluster.RegisterAdminEndpoint(adminURL); err != nil {
		t.Fatalf("RegisterAdminEndpoint: %v", err)
	}

	ep, err := cluster.state.GetEndpoints().GetEndpointByURL(adminURL)
	if err != nil {
		t.Fatalf("GetEndpointByURL: %v", err)
	}
	if ep.URL != adminURL {
		t.Errorf("URL=%q want %q", ep.URL, adminURL)
	}
	if ep.ClusterURL == "" {
		t.Fatal("ClusterURL is empty — pairing-time admission did not derive it")
	}
	if !strings.HasPrefix(ep.ClusterURL, "https://192.0.2.10:9091") {
		t.Errorf("ClusterURL=%q expected https://192.0.2.10:9091...", ep.ClusterURL)
	}
}

// TestRegisterAdminEndpoint_BindPortZero_LeavesClusterURLEmpty pins
// the no-mTLS-configured case — a coord without a cluster bind_port
// (legacy / standalone) must still be able to register endpoints,
// just without ClusterURL.
func TestRegisterAdminEndpoint_BindPortZero_LeavesClusterURLEmpty(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Enabled:  true,
		BindPort: 0, // not configured
	}
	cluster, err := NewCluster(cfg, "http://localhost:9090", &MockLocalHandler{}, nil, nil)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	const adminURL = "http://192.0.2.10:9090"
	if err := cluster.RegisterAdminEndpoint(adminURL); err != nil {
		t.Fatalf("RegisterAdminEndpoint: %v", err)
	}
	ep, err := cluster.state.GetEndpoints().GetEndpointByURL(adminURL)
	if err != nil {
		t.Fatalf("GetEndpointByURL: %v", err)
	}
	if ep.ClusterURL != "" {
		t.Errorf("ClusterURL=%q expected empty when BindPort==0", ep.ClusterURL)
	}
}
