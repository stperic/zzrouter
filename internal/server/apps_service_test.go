package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// The list is assembled from two maps — the endpoint registry keyed by URL
// and the provider manager keyed by name — so it came back in a different
// order on every call. The TUI acts on the row under the cursor, so a
// refresh landing between selecting a provider and pressing a key sent the
// action to whichever one took that row.
func TestListAppsOrdersProvidersDeterministically(t *testing.T) {
	provider := &mockClusterState{
		localApps: []prov_apps.LocalProviderInfo{
			{Key: "openrouter", Name: "openrouter", Type: "openrouter", Node: "macbook-pro", State: prov_apps.StateCloudAvailable},
			{Key: "ollama", Name: "ollama", Type: "ollama", Node: "macbook-pro", State: prov_apps.StateLive},
			{Key: "mlx", Name: "mlx", Type: "mlx", Node: "macbook-pro", State: prov_apps.StateLive},
		},
		endpoints: []*mesh.Endpoint{
			{
				NodeName: "zeta",
				Snapshot: mesh.EndpointSnapshot{HealthReport: mesh.HealthReport{Apps: []prov_apps.LocalProviderInfo{
					{Key: "vllm", Name: "vllm", Type: "vllm", State: prov_apps.StateLive},
				}}},
			},
			{
				NodeName: "worker-1",
				Snapshot: mesh.EndpointSnapshot{HealthReport: mesh.HealthReport{Apps: []prov_apps.LocalProviderInfo{
					{Key: "vllm", Name: "vllm", Type: "vllm", State: prov_apps.StateLive},
					{Key: "llamacpp", Name: "llamacpp", Type: "llamacpp", State: prov_apps.StateLive},
					{Key: "ollama", Name: "ollama", Type: "ollama", State: prov_apps.StateLive},
				}}},
			},
		},
	}
	svc := NewAppsService(provider, &mockRouter{})

	resp, err := svc.ListApps(context.Background(), &ListAppsRequest{})
	require.NoError(t, err)

	got := make([]string, 0, len(resp.Data))
	for _, a := range resp.Data {
		got = append(got, a.Name+"@"+a.Node)
	}

	// The caller's own node leads, then worker nodes by name, then providers
	// by name within each. Leading with the local node matches where an
	// operator acts most and keeps the grouping the list already shows.
	assert.Equal(t, []string{
		"mlx@macbook-pro", "ollama@macbook-pro", "openrouter@macbook-pro",
		"llamacpp@worker-1", "ollama@worker-1", "vllm@worker-1",
		"vllm@zeta",
	}, got)
}

// Nodes advertise a display name while platform overrides key on GOOS, so a
// remote Mac would never have matched a "darwin" override.
func TestGoosFromNodeOS(t *testing.T) {
	for name, want := range map[string]string{
		"macOS": "darwin", "Linux": "linux", "Windows": "windows",
		"darwin": "darwin", "linux": "linux", "  Linux  ": "linux",
		// Unknown resolves to the base source rather than a guessed platform.
		"": "", "Plan9": "", "macos x": "",
	} {
		assert.Equal(t, want, goosFromNodeOS(name), "input %q", name)
	}
}

// The nodes list is built the same way the provider list was: local node,
// then workers out of a map. Unsorted, it reshuffles between calls, and any
// caller acting on a row by position acts on whichever node took that row.
func TestListNodesOrdersWorkersDeterministically(t *testing.T) {
	provider := &mockClusterState{
		localNode: map[string]any{"name": "macbook-pro"},
		endpoints: []*mesh.Endpoint{
			{NodeName: "zeta"},
			{NodeName: "alpha"},
			{NodeName: "worker-1"},
		},
	}
	svc := NewNodesService(provider, &mockRouter{}, nil)

	resp, err := svc.ListNodes(context.Background(), &ListNodesRequest{})
	require.NoError(t, err)

	got := make([]string, 0, len(resp.Data))
	for _, n := range resp.Data {
		name, _ := n["name"].(string)
		got = append(got, name)
	}

	// The local node keeps the head; the workers are ordered by name.
	assert.Equal(t, []string{"macbook-pro", "alpha", "worker-1", "zeta"}, got)
}
