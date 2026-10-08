package discovery

import "testing"

// TestNewService tests creating a new discovery service.
//
// Tool registry / DiscoverTools / DiscoverProviderTools tests were
// retired alongside the tools subsystem itself — provider discovery
// now reads the providers/<kind>/<name>/config.yaml tree via
// /runs/capabilities, not a hardcoded definitions list. Hardware
// + network probes are unit-tested in their own packages and
// exercised by the live cluster join flow.
func TestNewService(t *testing.T) {
	service := NewService()

	if service == nil {
		t.Fatal("Expected service instance, got nil")
	}

	if service.Hardware == nil {
		t.Error("Expected Hardware to be initialized")
	}

	if service.Network == nil {
		t.Error("Expected Network to be initialized")
	}
}
