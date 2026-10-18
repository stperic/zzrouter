package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/resolver"
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
	owner string
	body  []byte
}

func (c *capturingCache) LookupModel(ctx context.Context, name string) (*cache.CachedModel, error) {
	return &cache.CachedModel{Name: name, Node: c.owner}, nil
}

func (c *capturingCache) ListModels(ctx context.Context, host, repo, app, model string) ([]*cache.CachedModel, error) {
	return nil, nil
}

func (c *capturingCache) Invalidate() {}

func (c *capturingCache) ResolveModel(_ context.Context, ref resolver.Reference) (*resolver.Resolved, error) {
	return &resolver.Resolved{ModelName: ref.Model, Node: c.owner}, nil
}

func (c *capturingCache) IsLocalNode(host string) bool { return host == "" || host == "localhost" }

func (c *capturingCache) RouteToModelOrBroadcast(ctx context.Context, modelName, path, method string, body []byte) (*routing.Response, error) {
	c.body = body
	return &routing.Response{StatusCode: 200, Body: []byte(`{"instance_id":"i1","status":"loading"}`), Node: c.owner}, nil
}

// A preview and a start leave the coordinator alike, carrying only what the
// caller sent: the node that runs the model resolves its own synced tree, so
// its preview answers what a start there would launch, with its own sources.
func TestLoadService_RemoteRequestsCarryOnlyWhatTheCallerSent(t *testing.T) {
	cfg := tierProbeConfig(t)
	for name, send := range map[string]func(*LoadService, *LoadModelRequest) error{
		"preview": func(svc *LoadService, req *LoadModelRequest) error {
			_, err := svc.PreviewRun(context.Background(), req)
			return err
		},
		"start": func(svc *LoadService, req *LoadModelRequest) error {
			_, err := svc.LoadModel(context.Background(), req)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			cc := &capturingCache{owner: "worker1"}
			svc := NewLoadService(cc, nil).WithAppsConfig(func() *pkgConfig.AppsConfig { return cfg })
			require.NoError(t, send(svc, &LoadModelRequest{Provider: "vllm", ModelName: "m", Parameters: map[string]string{"seed": "42"}}))

			var sent LoadModelRequest
			require.NoError(t, json.Unmarshal(cc.body, &sent))
			assert.Equal(t, schema.ParameterValues{"seed": "42"}, sent.Parameters,
				"a value resolved here outranks the running node's own tiers as the request tier")
			assert.Empty(t, sent.Environment)
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
