package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/routing"
)

// noAppsConfig is a node with no providers/ tree.
func noAppsConfig() *pkgConfig.AppsConfig { return nil }

// recordingRouter answers like mockRouter and keeps every routed request.
type recordingRouter struct {
	mockRouter
	routed []routing.Request
}

func (r *recordingRouter) Route(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	r.routed = append(r.routed, *req)
	return r.mockRouter.Route(ctx, req)
}

// A provider the sync carries has one pin, written on the coordinator and
// carried to every node; its enable is still each node's own. A provider
// the sync never carries keeps a copy, and a pin, per node.
func TestAppsService_UpdateApp_PinIsClusterWide(t *testing.T) {
	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", pkgConfig.ServiceConfig{
		Protocol: pkgConfig.ProtocolOpenAI, Mode: "on-demand",
		Runtime: &pkgConfig.AppRuntimeConfig{BasePort: 8080, PortRange: []int{8080, 8089},
			Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "vllm"}},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	require.NoError(t, cfg.AddApp("openai", pkgConfig.ServiceConfig{
		Enabled: new(true), Mode: "cloud", Protocol: pkgConfig.ProtocolOpenAI,
		Runtime: &pkgConfig.AppRuntimeConfig{Endpoint: "https://example.test",
			API: &pkgConfig.APIConfig{AuthType: pkgConfig.AuthTypeBearer, Token: "t"}},
		Capabilities: &pkgConfig.AppCapabilities{Models: []string{"m"}, WireEndpoints: []string{"chat_completions"}},
	}))
	newService := func() (*AppsService, *recordingRouter) {
		router := &recordingRouter{mockRouter: mockRouter{response: &routing.Response{
			StatusCode: http.StatusOK, Body: []byte(`{"name":"vllm","enabled":true,"pinned_version":"0.29.1","message":"done"}`)}}}
		return NewAppsService(nil, router, func() *pkgConfig.AppsConfig { return cfg }), router
	}
	sent := func(t *testing.T, req routing.Request) UpdateAppRequest {
		t.Helper()
		var body UpdateAppRequest
		require.NoError(t, json.Unmarshal(req.Body, &body))
		return body
	}
	pin, on := "0.29.1", true

	t.Run("a node-addressed pin is refused", func(t *testing.T) {
		svc, router := newService()
		_, err := svc.UpdateApp(t.Context(), &UpdateAppRequest{Name: "vllm", Node: "localai", PinnedVersion: &pin})
		var routed *RoutedError
		require.True(t, errors.As(err, &routed), "%v", err)
		assert.Equal(t, http.StatusBadRequest, routed.StatusCode)
		assert.Equal(t, string(httperr.CodePinIsClusterWide), routed.ProblemDetails.Code)
		assert.Empty(t, router.routed, "a refused pin reaches no node")
	})

	t.Run("the pin goes to the coordinator, the enable to every node", func(t *testing.T) {
		svc, router := newService()
		_, err := svc.UpdateApp(t.Context(), &UpdateAppRequest{Name: "vllm", PinnedVersion: &pin, Enabled: &on})
		require.NoError(t, err)
		require.Len(t, router.routed, 2)
		assert.Equal(t, constants.Localhost, router.routed[0].Node)
		assert.Equal(t, &pin, sent(t, router.routed[0]).PinnedVersion)
		assert.Nil(t, sent(t, router.routed[0]).Enabled)
		assert.Empty(t, router.routed[1].Node)
		assert.Nil(t, sent(t, router.routed[1]).PinnedVersion, "no worker is sent the pin")
		assert.Equal(t, &on, sent(t, router.routed[1]).Enabled)
	})

	t.Run("a node's own copy keeps its node", func(t *testing.T) {
		svc, router := newService()
		_, err := svc.UpdateApp(t.Context(), &UpdateAppRequest{Name: "openai", Node: "localai", PinnedVersion: &pin})
		require.NoError(t, err)
		require.Len(t, router.routed, 1)
		assert.Equal(t, "localai", router.routed[0].Node)
	})
}

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
	svc := NewAppsService(provider, &mockRouter{}, noAppsConfig)

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
