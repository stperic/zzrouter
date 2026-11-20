package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/routing"
)

// tierProbeConfig builds a provider whose node tier disagrees with its
// defaults, which is the only shape that can show a tier inversion: a
// value present at both ends means the wrong answer is still a value.
func tierProbeConfig(t *testing.T) *pkgConfig.AppsConfig {
	t.Helper()
	cfg := &pkgConfig.AppsConfig{}
	enabled := true
	require.NoError(t, cfg.AddApp("vllm", pkgConfig.ServiceConfig{
		Enabled:  &enabled,
		Name:     "vLLM",
		Protocol: pkgConfig.ProtocolOpenAI,
		Mode:     "on-demand",
		Runtime: &pkgConfig.AppRuntimeConfig{
			PortRange: []int{8100, 8105},
			BasePort:  8100,
			Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "vllm"},
		},
		Defaults: &pkgConfig.AppDefaultsConfig{
			Parameters: map[string]string{"max-model-len": "8192"},
		},
		Nodes: map[string]pkgConfig.NodeSpec{
			"worker1": {Parameters: map[string]string{"max-model-len": "65536"}},
		},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	return cfg
}

// tierProbeNode builds the minimal Server the launch-side resolver needs:
// a node identity to walk the tree with, and the tree itself.
func tierProbeNode(t *testing.T, cfg *pkgConfig.AppsConfig, name string) *Server {
	t.Helper()
	nc := &pkgConfig.NodeConfig{}
	nc.Node.Name = name
	return &Server{node: NewNodeIdentity(nc), configStore: pkgConfig.NewMemoryAppsConfigStore(cfg)}
}

// The node that runs a model resolves its own tiers: a request naming no
// parameter launches with the value nodes[N] sets for that node, and a
// parameter the caller sent wins over every tier.
func TestLaunchResolvesOnTheRunningNode(t *testing.T) {
	target := tierProbeNode(t, tierProbeConfig(t), "worker1")

	own, err := launchParameters(t, target, "m", "vllm", "chat", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "65536", own.Params["max-model-len"])
	assert.Equal(t, pkgConfig.TierNode.String(), own.Sources["max-model-len"])

	sent, err := launchParameters(t, target, "m", "vllm", "chat", map[string]string{"max-model-len": "1024"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "1024", sent.Params["max-model-len"])
	assert.Equal(t, pkgConfig.TierRequest.String(), sent.Sources["max-model-len"])
}

// capturingCache stands in for the cluster: it records the body a
// request leaves with and answers as the target node would.
type capturingCache struct {
	fakeRouter
	owner string
	body  []byte
}

func (c *capturingCache) modelOwner(context.Context, string) (string, error) { return c.owner, nil }

func (c *capturingCache) RouteToModelOrBroadcast(_ context.Context, _, _, _ string, body []byte) (*routing.Response, error) {
	c.body = body
	reply, err := json.Marshal(map[string]string{"id": "i1", "status": "starting", "node": c.owner})
	return &routing.Response{StatusCode: 200, Body: reply}, err
}

func (c *capturingCache) Route(_ context.Context, req *routing.Request) (*routing.Response, error) {
	if req.Node != c.owner {
		return nil, fmt.Errorf("unexpected start target %q", req.Node)
	}
	c.body = req.Body
	reply, err := json.Marshal(map[string]string{"id": "i1", "status": "starting", "node": c.owner})
	return &routing.Response{StatusCode: 202, Body: reply}, err
}

func TestRunsServiceRemoteRequestsCarryOnlyWhatTheCallerSent(t *testing.T) {
	cfg := tierProbeConfig(t)
	parameters := schema.ParameterValues{"seed": "42"}
	environment := map[string]string{"DEVICE": "1"}
	for _, preview := range []bool{true, false} {
		t.Run(map[bool]string{true: "preview", false: "start"}[preview], func(t *testing.T) {
			cc := &capturingCache{owner: "worker1"}
			svc := NewRunsService(cc).WithModelRouter(cc, func() *pkgConfig.AppsConfig { return cfg })
			if preview {
				_, err := svc.PreviewRun(t.Context(), &PreviewRunRequest{Provider: "vllm", ModelName: "m", Parameters: parameters, Environment: environment})
				require.NoError(t, err)
			} else {
				_, err := svc.LaunchRun(t.Context(), &LaunchRunRequest{Provider: "vllm", Model: "m", Parameters: parameters, Environment: environment, Replace: true})
				require.NoError(t, err)
			}
			var sent LaunchRunRequest
			require.NoError(t, json.Unmarshal(cc.body, &sent))
			assert.Equal(t, parameters, sent.Parameters)
			assert.Equal(t, environment, sent.Environment)
			assert.Equal(t, !preview, sent.Replace)
		})
	}
}

// launchParameters reads the launch owner's tiers without requiring weights or a process.
func launchParameters(t *testing.T, s *Server, model, provider, endpoint string, parameters, environment map[string]string) (prov_apps.LaunchParameters, error) {
	t.Helper()
	if s.providers.appMgr == nil {
		manager, err := prov_apps.NewProviderAppManager(s.appsConfig(), prov_apps.WithNodename(s.node.Nodename))
		require.NoError(t, err)
		s.providers.appMgr = manager
		t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	}
	return s.providers.appMgr.ResolveLaunchParameters(t.Context(), prov_apps.EnsureRequest{Provider: provider, Model: model, Endpoint: prov_apps.Endpoint(endpoint), Parameters: parameters, EnvVars: environment})
}
