package prov_apps

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/schema"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

// The parameter tree is documented as THE parameter model, but for a
// long time the launch path read only Defaults — so a models[X] or
// nodes[N] value persisted, peer-synced, and reported correctly from
// /resolved while the launched process never saw it. These tests pin
// the full precedence at the exact point where the answer becomes the
// process's argv.

const tierTestNode = "worker-a"

// tierParameters declares the keys tierServiceConfig sets, and seed, which
// only requests set; their values name tiers, so each takes any string.
const tierParameters = `
parameters:
  ctx-size: {}
  only-default: {}
  seed: {}
endpoints:
  embeddings:
    parameters:
      pooling: {}
`

// tierServiceConfig builds a provider whose every tier sets the same
// key to a distinguishable value, so a precedence bug names the tier
// that wrongly won rather than just failing.
func tierServiceConfig() config.ServiceConfig {
	return config.ServiceConfig{
		Enabled:  new(true),
		Name:     "vLLM",
		Protocol: config.ProtocolOpenAI,
		Mode:     "on-demand",
		// on-demand providers must declare a port range to validate.
		Runtime: &config.AppRuntimeConfig{
			PortRange: []int{8100, 8105},
			BasePort:  8100,
			Execution: config.ExecutionConfig{
				Type:    "python",
				Command: "python3",
				Args:    []string{"-m", "vllm.entrypoints.openai.api_server", "--port", "${PORT}"},
			},
		},
		Defaults: &config.AppDefaultsConfig{
			Parameters:  map[string]string{"ctx-size": "tier0", "only-default": "d"},
			Environment: map[string]string{"ZZ_ENV": "tier0"},
		},
		Models: map[string]config.ModelSpec{
			"llama3": {
				Parameters:  map[string]string{"ctx-size": "tier1"},
				Environment: map[string]string{"ZZ_ENV": "tier1"},
			},
		},
		Capabilities: &config.AppCapabilities{
			WireEndpoints: []string{"chat_completions", "embeddings"},
		},
		Nodes: map[string]config.NodeSpec{
			tierTestNode: {
				Parameters: map[string]string{"ctx-size": "tier2"},
				Models: map[string]config.NodeModelSpec{
					"llama3": {Parameters: map[string]string{"ctx-size": "tier3"}},
				},
			},
		},
	}
}

func tierManager(t *testing.T, node string) *ProviderAppManager {
	t.Helper()
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", tierServiceConfig()))
	opts := []Option{declaredSchema(t, tierParameters)}
	if node != "" {
		opts = append(opts, WithNodename(func() string { return node }))
	}
	m, err := NewProviderAppManager(cfg, opts...)
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	return m
}

// endpointAwareManager builds a provider that DECLARES an endpoints
// overlay. That matters: EnsureRun collapses the endpoint to chat
// for providers with no overlays (one instance per provider+model), so
// endpoint behaviour is only observable on an endpoint-aware provider.
func endpointAwareManager(t *testing.T) *ProviderAppManager {
	t.Helper()
	svc := tierServiceConfig()
	svc.Defaults.Endpoints = map[string]config.EndpointOverlay{
		string(EndpointEmbeddings): {Parameters: map[string]string{"pooling": "mean"}},
	}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m, err := NewProviderAppManager(cfg, WithNodename(func() string { return tierTestNode }), declaredSchema(t, tierParameters))
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	return m
}

func TestResolveLaunchParams_TierPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		node     string
		model    string
		reqParam map[string]string
		want     string
	}{
		{
			name: "defaults only when no model or node matches",
			node: "", model: "other-model",
			want: "tier0",
		},
		{
			name: "model tier beats defaults",
			node: "", model: "llama3",
			want: "tier1",
		},
		{
			name: "node tier beats model",
			node: tierTestNode, model: "other-model",
			want: "tier2",
		},
		{
			name: "node x model cell beats node",
			node: tierTestNode, model: "llama3",
			want: "tier3",
		},
		{
			name: "request beats every configured tier",
			node: tierTestNode, model: "llama3",
			reqParam: map[string]string{"ctx-size": "tier4"},
			want:     "tier4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tierManager(t, tc.node)
			svc, ok := m.appsConfig.LookupApp("vllm")
			require.True(t, ok)

			params := rawLaunchParams(m, svc, EnsureRequest{
				Provider:   "vllm",
				Model:      tc.model,
				Parameters: tc.reqParam,
			}, EndpointChat)

			assert.Equal(t, tc.want, params["ctx-size"])
		})
	}
}

// rawLaunchParams is resolveRawLaunchParams flattened to the values a launch passes.
func rawLaunchParams(m *ProviderAppManager, svc config.ServiceConfig, req EnsureRequest, endpoint Endpoint) map[string]string {
	params, _ := m.resolveRawLaunchParams(svc, req, string(endpoint))
	return config.FlattenParameters(params)
}

// A node-tier value is only reachable if the manager knows which node
// it is. Without the name the walk must fall back to the lower tiers
// rather than pick some other node's values.
func TestResolveLaunchParams_UnknownNodeFallsBackNotSideways(t *testing.T) {
	m := tierManager(t, "")
	svc, ok := m.appsConfig.LookupApp("vllm")
	require.True(t, ok)

	params := rawLaunchParams(m, svc, EnsureRequest{Provider: "vllm", Model: "llama3"}, EndpointChat)
	assert.Equal(t, "tier1", params["ctx-size"], "no nodename must not reach tier2/tier3")
}

// Lower-tier keys that no higher tier mentions must survive the merge;
// a tier is an overlay, not a replacement.
func TestResolveLaunchParams_HigherTierDoesNotDropLowerKeys(t *testing.T) {
	m := tierManager(t, tierTestNode)
	svc, ok := m.appsConfig.LookupApp("vllm")
	require.True(t, ok)

	params := rawLaunchParams(m, svc, EnsureRequest{Provider: "vllm", Model: "llama3"}, EndpointChat)
	assert.Equal(t, "tier3", params["ctx-size"])
	assert.Equal(t, "d", params["only-default"], "defaults-only key must survive the overlay")
}

func TestResolveLaunchParams_EnvironmentFollowsTheSameWalk(t *testing.T) {
	m := tierManager(t, tierTestNode)
	svc, ok := m.appsConfig.LookupApp("vllm")
	require.True(t, ok)

	_, env := m.resolveRawLaunchParams(svc, EnsureRequest{Provider: "vllm", Model: "llama3"}, string(EndpointChat))
	assert.Equal(t, "tier1", env["ZZ_ENV"], "environment must walk the tiers too, not read Defaults only")
}

// "auto" means "provider decides", so the flag must be dropped rather
// than passed as the literal string. Filtering AFTER the request tier
// is what lets a caller neutralise a configured value.
func TestResolveLaunchParams_AutoIsFilteredAfterRequestTier(t *testing.T) {
	m := tierManager(t, tierTestNode)
	preview, err := m.PreviewLaunch(t.Context(), EnsureRequest{Provider: "vllm", Model: "llama3", Parameters: map[string]string{"ctx-size": "auto"}}, 8100)
	require.NoError(t, err)
	assert.NotContains(t, preview.Params, "ctx-size")
	assert.NotContains(t, preview.Args, "auto")
	assert.Equal(t, "d", preview.Params["only-default"])
}

// Endpoint overlays are applied WITHIN each tier (defaults + its
// overlay, then model + its overlay, and so on), not as a tier above
// them all. So a node-tier value still beats an overlay declared under
// defaults. That is subtle enough to be worth pinning: the natural
// misreading is that naming an endpoint makes a value win outright.
func TestResolveLaunchParams_EndpointOverlayAppliesWithinItsTier(t *testing.T) {
	svc := tierServiceConfig()
	svc.Defaults.Endpoints = map[string]config.EndpointOverlay{
		string(EndpointEmbeddings): {Parameters: map[string]string{
			"ctx-size": "embed", // collides with the tier ladder
			"pooling":  "mean",  // set by nothing else
		}},
	}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m, err := NewProviderAppManager(cfg, WithNodename(func() string { return tierTestNode }), declaredSchema(t, tierParameters))
	require.NoError(t, err)
	cleanupProviderManager(t, m)

	resolved, ok := m.appsConfig.LookupApp("vllm")
	require.True(t, ok)

	embed := rawLaunchParams(m, resolved, EnsureRequest{Provider: "vllm", Model: "other"}, EndpointEmbeddings)
	assert.Equal(t, "mean", embed["pooling"],
		"a key only the overlay sets must reach the launch")
	assert.Equal(t, "tier2", embed["ctx-size"],
		"the node tier still outranks an overlay declared under defaults")

	chat := rawLaunchParams(m, resolved, EnsureRequest{Provider: "vllm", Model: "other"}, EndpointChat)
	_, leaked := chat["pooling"]
	assert.False(t, leaked, "chat must not inherit the embeddings overlay")
}

// RestartInstance rebuilds its EnsureRequest from the stored instance.
// instance.Config carries no endpoint, so it has to be read off the
// instance itself; dropping it relaunched an embeddings instance as a
// chat one, and now that launch resolves the endpoints overlay it would
// silently pick the wrong parameters too.
func TestRestartRequestPreservesEndpoint(t *testing.T) {
	m := endpointAwareManager(t)

	instResult, err := m.EnsureRun(t.Context(), EnsureRequest{
		Provider: "vllm",
		Model:    "llama3",
		Endpoint: EndpointEmbeddings,
	})
	inst := instResult.Run
	require.NoError(t, err)
	require.Equal(t, string(EndpointEmbeddings), inst.Endpoint)

	// Drive the real request builder, not a copy of it: a test that
	// re-implements the logic passes no matter what the code does.
	req := restartRequest(inst)
	assert.Equal(t, EndpointEmbeddings, req.Endpoint,
		"restart must carry the endpoint, or the relaunch resolves as chat")
	assert.Equal(t, "vllm", req.Provider)
	assert.Equal(t, "llama3", req.Model)
	assert.Zero(t, req.Port, "port must be left unset so the pool reallocates")
	assert.Equal(t, instance.StartedByAPI, inst.StartedBy)
	assert.Equal(t, OriginAPI, req.Origin)

	// A run a request started stays a request's run across a restart.
	instResult, err = m.EnsureRun(t.Context(), EnsureRequest{Provider: "vllm", Model: "other", Origin: OriginRequest})
	require.NoError(t, err)
	assert.Equal(t, instance.StartedByRequest, instResult.Run.ToInfo("node").StartedBy)
	assert.Equal(t, OriginRequest, restartRequest(instResult.Run).Origin)
}

// The stored Config must hold the REQUEST tier only. If it held the
// merged result, a restart would replay tiers 0-3 as if the caller had
// passed them explicitly, and they would then outrank a config change —
// which is exactly the bug that made restart unable to pick up an edit.
func TestLaunchStoresRequestTierNotMergedResult(t *testing.T) {
	m := tierManager(t, tierTestNode)

	instResult, err := m.EnsureRun(t.Context(), EnsureRequest{
		Provider:   "vllm",
		Model:      "llama3",
		Parameters: map[string]string{"seed": "42"},
	})
	inst := instResult.Run
	require.NoError(t, err)

	stored := inst.SnapshotConfig().Parameters
	assert.Equal(t, "42", stored["seed"], "the caller's explicit override must survive a restart")
	_, hasTierValue := stored["ctx-size"]
	assert.False(t, hasTierValue,
		"a configured tier value must NOT be stored as if the caller had sent it")
}

// localizerManager builds the tier-test provider with a ParamLocalizer.
func localizerManager(t *testing.T, l ParamLocalizer) (*ProviderAppManager, config.ServiceConfig) {
	t.Helper()
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", tierServiceConfig()))
	m, err := NewProviderAppManager(cfg, WithParamLocalizer(l), declaredSchema(t, tierParameters, "parameters:\n  template: {type: asset}\n"))
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	svc, ok := cfg.LookupApp("vllm")
	require.True(t, ok)
	return m, svc
}

// What the node's ParamLocalizer returns is what the engine is given.
func TestBuildLaunch_LocalizerOutputReachesArgv(t *testing.T) {
	m, svc := localizerManager(t, func(_, _, _ string, params map[string]string, _ map[string]schema.ParamShape) (Localized, error) {
		out := maps.Clone(params)
		out["template"] = "/node/local/t.jinja"
		return Localized{Params: out, Files: map[string]string{"template": "digest"}}, nil
	})
	launch, files, err := m.buildLaunch(svc, "vllm", string(EndpointChat), process.ResolveModel(svc, "llama3"), 8100, map[string]string{"template": "t.jinja"}, requireLaunchSchema(t, m, "vllm"))
	require.NoError(t, err)
	assert.Contains(t, strings.Join(launch.Args, " "), "--template /node/local/t.jinja")
	assert.NotContains(t, launch.Args, "t.jinja")
	assert.Equal(t, map[string]string{"template": "digest"}, files, "the localizer's file digests are part of the launch")
}

// Validation checks what the operator configured, so a refused value
// never reaches the localizer.
func TestBuildLaunch_ValidatesBeforeLocalizing(t *testing.T) {
	called := false
	m, svc := localizerManager(t, func(_, _, _ string, params map[string]string, _ map[string]schema.ParamShape) (Localized, error) {
		called = true
		return Localized{Params: params}, nil
	})
	_, _, err := m.buildLaunch(svc, "vllm", string(EndpointChat), process.ResolveModel(svc, "llama3"), 8100, map[string]string{"key;inject": "v"}, requireLaunchSchema(t, m, "vllm"))
	require.ErrorIs(t, err, ErrParameterValidation)
	assert.False(t, called)
}

// A localizer refusal refuses the launch and leaves no instance behind.
// The localizer is keyed by the provider's config key and the launch's
// endpoint, and the instance keeps the configured values, so /runs
// reports names rather than node-local paths.
func TestEnsureRun_LocalizerRefusalRefusesLaunch(t *testing.T) {
	var gotProvider, gotEndpoint string
	var gotParams map[string]string
	m, _ := localizerManager(t, func(provider, endpoint, model string, params map[string]string, _ map[string]schema.ParamShape) (Localized, error) {
		gotProvider, gotEndpoint, gotParams = provider, endpoint, params
		return Localized{}, errors.New("asset not on this node")
	})
	_, err := m.EnsureRun(t.Context(), EnsureRequest{
		Provider: "vllm", Model: "llama3",
		Parameters: map[string]string{"template": "t.jinja"},
	})
	require.ErrorIs(t, err, ErrParameterValidation)
	assert.Contains(t, err.Error(), "asset not on this node")
	assert.Equal(t, "vllm", gotProvider)
	assert.Equal(t, string(EndpointChat), gotEndpoint)
	assert.Equal(t, "t.jinja", gotParams["template"])
	_, exists := m.instances.GetByKey(instance.NewKey("", "llama3", string(EndpointChat)))
	assert.False(t, exists)
}
