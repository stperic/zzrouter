package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/cache"
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
	return &Server{node: NewNodeIdentity(nc), appsConfig: cfg}
}

// tierProbeService builds a LoadService whose cache reports `model` as
// living on `owner`, with this node answering to "localhost".
func tierProbeService(t *testing.T, cfg *pkgConfig.AppsConfig, model, owner string) *LoadService {
	t.Helper()
	mc := &mockModelCache{models: []*cache.CachedModel{{Name: model, Node: owner}}}
	return NewLoadService(mc, nil).
		WithTierResolution(func() *pkgConfig.AppsConfig { return cfg })
}

// A preview is answered by resolving the tier tree here, for the node
// that would run the model, and the node rendering it layers what
// arrives over its own walk as the request tier — the last word. So a
// resolution that could not see nodes[N] does not merely disagree with
// N: it outranks it, and the preview describes a launch that would
// never happen. A value from `defaults` arrives stamped as something
// the caller sent and beats the `nodes[N]` value written for exactly
// this node.
//
// `node` is optional on /runs/load and /runs/preview and the target is
// chosen afterwards, so the omitted-node case is the common one.
//
// The invariant, whatever the merge does: a request carrying no
// parameters of its own must resolve to what the running node's own
// four-tier walk yields.
func TestLoadService_PreMergeDoesNotOutrankTheRunningNodesTier(t *testing.T) {
	cfg := tierProbeConfig(t)
	target := tierProbeNode(t, cfg, "worker1")

	// What worker1 resolves for itself. This is the answer any route
	// has to arrive at.
	own, err := target.resolveForLaunch("m", "vllm", "chat", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "65536", own.Params["max-model-len"],
		"fixture is wrong: the node tier is not being read at all")

	// A caller who names no node, for a model the cache places on
	// worker1 — which is where the router will send it.
	svc := tierProbeService(t, cfg, "m", "worker1")
	req := &LoadModelRequest{Provider: "vllm", ModelName: "m"}
	sources := svc.preMerge(req)

	assert.Equal(t, "65536", req.Parameters["max-model-len"],
		"the request left carrying a value resolved for nobody")
	assert.Equal(t, pkgConfig.TierNode.String(), sources["max-model-len"],
		"a value nobody sent was reported as coming from the request")

	got, err := target.resolveForLaunch("m", "vllm", "chat", req.Parameters, req.Environment)
	require.NoError(t, err)
	assert.Equal(t, "65536", got.Params["max-model-len"],
		"a partial resolution defeated the node tier written for the node that ran the model")
}

// A model this node would run itself needs no pre-merge: it reads the
// live tree. Resolving anyway would flatten the tiers away and report
// values as caller-supplied that the caller never sent.
func TestLoadService_LeavesLocalWorkToResolveItself(t *testing.T) {
	cfg := tierProbeConfig(t)
	svc := tierProbeService(t, cfg, "m", "localhost")

	req := &LoadModelRequest{Provider: "vllm", ModelName: "m"}
	assert.Nil(t, svc.preMerge(req))
	assert.Empty(t, req.Parameters)
}

// A model no node has claimed fans out, so there is no single node to
// resolve for. Merging one node's answer would hand it to whichever
// node happens to reply.
func TestLoadService_ResolvesNothingForAFanOut(t *testing.T) {
	cfg := tierProbeConfig(t)
	svc := tierProbeService(t, cfg, "other-model", "worker1")

	req := &LoadModelRequest{Provider: "vllm", ModelName: "m"}
	assert.Nil(t, svc.preMerge(req))
	assert.Empty(t, req.Parameters)
}

// The node named by the caller wins over the one the cache reports:
// that is the node the router will unicast to.
func TestLoadService_ResolvesForTheNodeTheCallerNamed(t *testing.T) {
	cfg := tierProbeConfig(t)
	svc := tierProbeService(t, cfg, "m", "worker2")

	req := &LoadModelRequest{Provider: "vllm", ModelName: "m", Node: "worker1"}
	sources := svc.preMerge(req)
	assert.Equal(t, "65536", req.Parameters["max-model-len"])
	assert.Equal(t, pkgConfig.TierNode.String(), sources["max-model-len"])
}

// A parameter the caller really did send still wins over every tier —
// tier fidelity must not be bought by dropping the request tier.
func TestLoadService_CallerOverrideStillWins(t *testing.T) {
	cfg := tierProbeConfig(t)
	target := tierProbeNode(t, cfg, "worker1")
	svc := tierProbeService(t, cfg, "m", "worker1")

	req := &LoadModelRequest{
		Provider:   "vllm",
		ModelName:  "m",
		Parameters: map[string]string{"max-model-len": "1024"},
	}
	sources := svc.preMerge(req)
	assert.Equal(t, pkgConfig.TierRequest.String(), sources["max-model-len"])

	got, err := target.resolveForLaunch("m", "vllm", "chat", req.Parameters, req.Environment)
	require.NoError(t, err)
	assert.Equal(t, "1024", got.Params["max-model-len"])
	assert.Equal(t, pkgConfig.TierRequest.String(), got.ParamSources["max-model-len"])
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

func (c *capturingCache) TargetNodeForModel(string) string { return c.owner }

func (c *capturingCache) IsLocalNode(host string) bool { return host == "" || host == "localhost" }

func (c *capturingCache) RouteToModelOrBroadcast(ctx context.Context, modelName, path, method string, body []byte) (*routing.Response, error) {
	c.body = body
	return &routing.Response{StatusCode: 200, Body: []byte(`{"instance_id":"i1","status":"loading"}`), Node: c.owner}, nil
}

// A launch is resolved by the node that runs it, and only by it. What
// arrives in a launch request is stored as the caller's own request
// tier and replayed on every restart, so a value resolved here would
// be pinned against the config it was read from — the config a
// restart exists to re-read. Preview has no instance to pin, which is
// why it still resolves here.
func TestLoadService_ALaunchCarriesOnlyWhatTheCallerSent(t *testing.T) {
	cfg := tierProbeConfig(t)
	cc := &capturingCache{owner: "worker1"}
	svc := NewLoadService(cc, nil).
		WithTierResolution(func() *pkgConfig.AppsConfig { return cfg })

	_, err := svc.LoadModel(context.Background(), &LoadModelRequest{
		Provider:   "vllm",
		ModelName:  "m",
		Parameters: map[string]string{"seed": "42"},
	})
	require.NoError(t, err)

	var sent LoadModelRequest
	require.NoError(t, json.Unmarshal(cc.body, &sent))
	assert.Equal(t, map[string]string{"seed": "42"}, sent.Parameters,
		"a resolved value in a launch request is stored as the caller's own and outranks the tree on every restart")
}

// "*" is the caller asking every node, so there is no single node to
// resolve for — and resolving with the literal star silently produces
// a tree with no `nodes[*]` in it, which is tiers 0 and 1 wearing the
// authority of a request parameter. The same inversion, spelled
// differently.
func TestLoadService_ResolvesNothingForAnExplicitStar(t *testing.T) {
	cfg := tierProbeConfig(t)
	svc := tierProbeService(t, cfg, "m", "worker1")

	req := &LoadModelRequest{Provider: "vllm", ModelName: "m", Node: "*"}
	assert.Nil(t, svc.preMerge(req))
	assert.Empty(t, req.Parameters)
}
