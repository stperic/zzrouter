package server

import (
	"context"
	"net/url"
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func TestProviderBrowserObservationsUseServingOrigin(t *testing.T) {
	manager, err := role.NewManager(role.RoleCoordinator)
	require.NoError(t, err)
	s := &Server{node: &NodeIdentity{name: "local"}, role: manager}
	origin, err := url.Parse("https://comfy.example.com")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctrl := NewRunsController(nil, nil, nil, nil, nil, s.providerBrowserURL)
	info := instance.InstanceInfo{ID: "run-a", Node: "local", Browser: &instance.BrowserInfo{Name: "Fixture", URL: "https://untrusted.example/"}}
	ctrl.annotateBrowserURL(&info)
	require.Empty(t, info.Browser.URL, "a worker cannot provide the public origin")
	s.providerBrowser.gateway = &providerBrowserGateway{origin: origin, ctx: ctx}
	ctrl.annotateBrowserURL(&info)
	require.Equal(t, "https://comfy.example.com/runs/local/run-a/", info.Browser.URL)
	require.Empty(t, s.providerBrowserURL("127.0.0.1:9090", "run-a"))
	require.Empty(t, s.providerBrowserURL("local", ".."))
	s.draining.Store(true)
	ctrl.annotateBrowserURL(&info)
	require.Empty(t, info.Browser.URL)
	s.draining.Store(false)
	cancel()
	ctrl.annotateBrowserURL(&info)
	require.Empty(t, info.Browser.URL)
}
