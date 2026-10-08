package server

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// TestEndpointToNodeInfo_AppsPropagate locks that HealthReport.Apps
// flows through endpointToNodeInfo's providers projection.
func TestEndpointToNodeInfo_AppsPropagate(t *testing.T) {
	ep := &mesh.Endpoint{
		URL:      "http://worker:9090",
		NodeName: "worker",
		Status:   mesh.StatusUp,
		Snapshot: mesh.EndpointSnapshot{
			Version: "1.0.0",
			HealthReport: mesh.HealthReport{
				ClusterRole: "worker",
				Apps: []prov_apps.LocalProviderInfo{
					{Key: "vllm", Type: "on-demand", Name: "vLLM"},
					{Key: "ollama", Type: "always-on", Name: "Ollama"},
				},
			},
		},
	}

	info := (&NodesService{}).endpointToNodeInfo(ep)

	providers, ok := info["providers"].([]map[string]any)
	if !ok {
		t.Fatalf("providers missing or wrong type: %T = %v", info["providers"], info["providers"])
	}
	if len(providers) != 2 {
		t.Fatalf("want 2 providers, got %d", len(providers))
	}
	if providers[0]["key"] != "vllm" || providers[1]["key"] != "ollama" {
		t.Errorf("provider keys: got %v / %v", providers[0]["key"], providers[1]["key"])
	}
	if info["version"] != "1.0.0" {
		t.Errorf("want version=1.0.0, got %v", info["version"])
	}
}

func TestEndpointToNodeInfo_UnprobedEndpoint(t *testing.T) {
	// A freshly-registered endpoint has no Snapshot — verify we still
	// render the URL-derived ip_address/port without a typed-field panic.
	ep := &mesh.Endpoint{
		URL:    "http://newworker:9090",
		Name:   "newworker",
		Status: mesh.StatusUnknown,
	}

	info := (&NodesService{}).endpointToNodeInfo(ep)

	if info["ip_address"] != "newworker" {
		t.Errorf("ip_address: got %v", info["ip_address"])
	}
	if info["port"] != 9090 {
		t.Errorf("port: got %v", info["port"])
	}
	if info["health_status"] != "unknown" {
		t.Errorf("health_status: got %v", info["health_status"])
	}
	for _, key := range []string{"memory", "disk", "gpu", "providers", "last_error"} {
		if _, ok := info[key]; ok {
			t.Errorf("%q block should be absent before first probe", key)
		}
	}
	if !ep.Snapshot.CollectedAt.IsZero() {
		t.Error("CollectedAt should be zero before first successful probe")
	}
}
