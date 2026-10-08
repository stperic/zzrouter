package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config/templates"
)

const (
	base    = "Qwen3.8-27B-Q8_0"
	agent   = "Qwen3.8-27B-Q8_0+agent"
	chatbot = "Qwen3.8-27B-Q8_0+chat"
)

// variantConfig is a provider with one base, a variant that changes the
// launch, and one that only adds request defaults.
func variantConfig() ServiceConfig {
	return ServiceConfig{
		Defaults: &AppDefaultsConfig{Parameters: map[string]string{"ctx-size": "8192"}},
		Models: map[string]ModelSpec{
			base: {Parameters: map[string]string{"ctx-size": "32768", "threads": "8"}, Request: map[string]any{"temperature": 0.6}},
			agent: {
				From:       base,
				Parameters: map[string]string{"ctx-size": "131072"},
				Request:    map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
			},
			chatbot: {From: base, Request: map[string]any{"temperature": 0.9}},
		},
		Nodes: map[string]NodeSpec{"n1": {Models: map[string]NodeModelSpec{
			base: {Parameters: map[string]string{"n-gpu-layers": "99"}},
		}}},
	}
}

// A variant inherits its base's cells and overrides only what it names.
func TestVariant_WalksTheBaseThenItself(t *testing.T) {
	sc := variantConfig()
	got := sc.Resolve("n1", agent)

	assert.Equal(t, base, got.From)
	p := got.Parameters
	assert.Equal(t, "131072", p["ctx-size"].Value)
	assert.Equal(t, agent, p["ctx-size"].Model)
	assert.Equal(t, "8", p["threads"].Value, "the base's model cell is inherited")
	assert.Equal(t, base, p["threads"].Model)
	assert.Equal(t, "99", p["n-gpu-layers"].Value, "the base's node x model cell is inherited")
	assert.Equal(t, TierNodeModel, p["n-gpu-layers"].Tier)

	r := got.Request
	assert.Equal(t, 0.6, r["temperature"].Value, "the base's request defaults are inherited")
	assert.Equal(t, map[string]any{"enable_thinking": false}, r["chat_template_kwargs"].Value)

	assert.Equal(t, 0.9, sc.Resolve("", chatbot).Request["temperature"].Value, "the variant's own default wins")

	// The base's cells follow the weights: a variant named anything gets them.
	sc.Models["my-agent"] = ModelSpec{From: base}
	assert.Equal(t, "8", sc.Resolve("", "my-agent").Parameters["threads"].Value)

	// The base itself is untouched by its variants.
	b := sc.Resolve("n1", base)
	assert.Empty(t, b.From)
	assert.Equal(t, "32768", b.Parameters["ctx-size"].Value)
	assert.NotContains(t, b.Request, "chat_template_kwargs")
}

// The router's decision: a variant is served by its base's process
// exactly when it resolves to the same launch there.
func TestVariant_SameLaunchDecidesTheProcess(t *testing.T) {
	sc := variantConfig()
	assert.True(t, sc.SameLaunch("n1", chatbot, base), "request defaults alone do not need a process")
	assert.False(t, sc.SameLaunch("n1", agent, base), "a different ctx-size is a different process")

	// Repeating the base's value is still the same launch.
	sc.Models[agent] = ModelSpec{From: base, Parameters: map[string]string{"ctx-size": "32768"}}
	assert.True(t, sc.SameLaunch("n1", agent, base))

	// A node-scoped cell can split them on one node and not another.
	sc.Nodes["n2"] = NodeSpec{Models: map[string]NodeModelSpec{agent: {Parameters: map[string]string{"parallel": "4"}}}}
	assert.True(t, sc.SameLaunch("n1", agent, base))
	assert.False(t, sc.SameLaunch("n2", agent, base))

	// An overlay on another endpoint counts too.
	sc.Models[chatbot] = ModelSpec{From: base, Endpoints: map[string]EndpointOverlay{
		"embeddings": {Parameters: map[string]string{"pooling": "last"}},
	}}
	assert.False(t, sc.SameLaunch("n1", chatbot, base))

	// "auto" means the flag is dropped, which is what an unset key gets.
	sc.Models[chatbot] = ModelSpec{From: base, Parameters: map[string]string{"batch-size": "auto"}}
	assert.True(t, sc.SameLaunch("n1", chatbot, base))
}

func TestVariant_LookupAndWeights(t *testing.T) {
	sc := variantConfig()
	name, b, ok := sc.Variant("qwen3.8-27b-q8_0+AGENT")
	require.True(t, ok)
	assert.Equal(t, agent, name, "the key as spelled")
	assert.Equal(t, base, b)
	_, _, ok = sc.Variant(base)
	assert.False(t, ok, "a plain model entry is not a variant")
	assert.Equal(t, base, sc.WeightsOf(agent))
	assert.Equal(t, base, sc.WeightsOf(base))
	// The runs API may append the file to load; it reaches the base.
	assert.Equal(t, base+"#Qwen3.8-27B-Q8_0.gguf", sc.WeightsOf(agent+"#Qwen3.8-27B-Q8_0.gguf"))
	// A variant may pin a file; a request's own hint replaces it.
	sc.Models["pinned"] = ModelSpec{From: base + "#Q4_K_M.gguf"}
	assert.Equal(t, base+"#Q4_K_M.gguf", sc.WeightsOf("pinned"))
	assert.Equal(t, base+"#Q8_0.gguf", sc.WeightsOf("pinned#Q8_0.gguf"))
	delete(sc.Models, "pinned")
	assert.Equal(t, base, sc.Resolve("", agent+"#x.gguf").From)
	// An exact plain key is not a variant, whatever another spelling says.
	sc.Models["qwen3.8-27b-q8_0+AGENT"] = ModelSpec{Parameters: map[string]string{"k": "v"}}
	_, _, ok = sc.Variant("qwen3.8-27b-q8_0+AGENT")
	assert.False(t, ok)
	delete(sc.Models, "qwen3.8-27b-q8_0+AGENT")
	assert.Equal(t, map[string]string{agent: base, chatbot: base}, sc.Variants())

	cfg := &AppsConfig{}
	svc := variantConfig()
	svc.Mode = "on-demand"
	svc.Protocol = ProtocolOpenAI
	svc.Runtime = &AppRuntimeConfig{PortRange: []int{8080, 8081}, BasePort: 8080,
		Execution: ExecutionConfig{Type: "cli", Command: "llama-server"}}
	svc.Capabilities = &AppCapabilities{WireEndpoints: []string{"chat_completions"}}
	require.NoError(t, cfg.AddApp("llamacpp", svc))
	provider, b, ok := cfg.LookupVariant(agent)
	require.True(t, ok)
	assert.Equal(t, "llamacpp", provider)
	assert.Equal(t, base, b)
	_, _, ok = cfg.LookupVariant(base)
	assert.False(t, ok)

	// Saving values equal to the defaults clears the variant's launch
	// values and keeps what makes it a variant.
	require.NoError(t, cfg.SaveModelDefaults("llamacpp", agent, map[string]string{"ctx-size": "8192"}, nil))
	saved, _ := cfg.LookupApp("llamacpp")
	assert.Empty(t, saved.Models[agent].Parameters)
	assert.Equal(t, base, saved.Models[agent].From)
	assert.NotEmpty(t, saved.Models[agent].Request)
}

func TestValidateVariantName(t *testing.T) {
	for _, ok := range []string{agent, "my-agent", "qwen3.8_chat", "a"} {
		assert.NoError(t, ValidateVariantName(ok), ok)
	}
	for _, bad := range []string{
		"Qwen3.8-27B-Q8_0[claude]", // glob class
		"qwen3.8:agent",            // Ollama tag
		"qwen@worker-1",            // node
		"worker-1::qwen",           // node
		"qwen#file.gguf",           // GGUF file
		"unsloth/qwen+agent",       // repository
		"qwen*", "", "+agent", "-agent", "qwen agent",
	} {
		assert.ErrorIs(t, ValidateVariantName(bad), ErrInvalidVariant, bad)
	}
}

// Dropping a variant's launch values must not drop the variant.
func TestModelSpec_IsEmpty(t *testing.T) {
	assert.True(t, ModelSpec{}.IsEmpty())
	assert.False(t, ModelSpec{From: base}.IsEmpty())
	assert.False(t, ModelSpec{Request: map[string]any{"temperature": 1}}.IsEmpty())
}

// The rules a tree must keep hold wherever a tree comes in, so they are
// checked on the tree, not on the write that produced it.
func TestCheckModels(t *testing.T) {
	pathsOf := func(err error) []string {
		var problems ModelProblems
		require.ErrorAs(t, err, &problems)
		require.ErrorIs(t, err, ErrInvalidVariant)
		out := make([]string, len(problems))
		for i, p := range problems {
			out[i] = p.Path
		}
		return out
	}

	assert.NoError(t, checkModels(modelsLaunched, variantConfig().Models))

	chain := map[string]ModelSpec{"w": {}, "v1": {From: "w"}, "v2": {From: "V1"}}
	assert.ElementsMatch(t, []string{"models.v2.from", "models.v1.from"}, pathsOf(checkModels(modelsLaunched, chain)),
		"a chain is reported at both ends of the link, whichever the write touched")

	assert.Equal(t, []string{"models.loop.from"}, pathsOf(checkModels(modelsLaunched, map[string]ModelSpec{"loop": {From: "LOOP"}})))
	assert.Equal(t, []string{"models.a/b.from"}, pathsOf(checkModels(modelsLaunched, map[string]ModelSpec{"a/b": {From: "w"}})))

	reserved := map[string]ModelSpec{"v": {From: "w", Request: map[string]any{"Tools": []any{}, "top_p": 0.9}}}
	assert.Equal(t, []string{"models.v.request.Tools"}, pathsOf(checkModels(modelsLaunched, reserved)),
		"case does not hide a field the client owns")

	twins := map[string]ModelSpec{"V": {From: "w"}, "v": {Parameters: map[string]string{"k": "x"}}}
	assert.Equal(t, []string{"models.v"}, pathsOf(checkModels(modelsLaunched, twins)), "a variant is found ignoring case")

	assert.Equal(t, []string{"models.m"}, pathsOf(checkModels(modelsNotLaunched, map[string]ModelSpec{"m": {Request: map[string]any{"top_p": 0.9}}})),
		"request defaults alone need a launch too")

	assert.Equal(t, []string{"models.v"}, pathsOf(checkModels(modelsNotLaunched, map[string]ModelSpec{"v": {From: "w"}})),
		"only a provider the router launches can hold a variant")
	assert.NoError(t, checkModels(modelsNotLaunched, map[string]ModelSpec{"m": {Parameters: map[string]string{"k": "v"}}}))
}

// A hand-edited or synced tree is held to the same rules as a write.
func TestLoad_RefusesABrokenVariant(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	path := filepath.Join(dir, "on-demand", "llamacpp", "config.yaml")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	body = append(body, []byte("models:\n  v1:\n    from: w\n  v2:\n    from: v1\n")...)
	require.NoError(t, os.WriteFile(path, body, 0o600))

	_, err = LoadAppsConfig(dir)
	var problems ModelProblems
	require.ErrorAs(t, err, &problems)
	assert.ElementsMatch(t, []string{"models.v2.from", "models.v1.from"}, []string{problems[0].Path, problems[1].Path})
}

// writeVariant gives an installed provider's config a variant.
func writeVariant(t *testing.T, dir, provider, name string) {
	t.Helper()
	path := filepath.Join(dir, "on-demand", provider, "config.yaml")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	body = append(body, []byte("models:\n  "+name+":\n    from: w\n")...)
	require.NoError(t, os.WriteFile(path, body, 0o600))
}

// Two files can each be valid and still claim one variant name; the
// load, which alone sees both, refuses them and names a file.
func TestLoad_RefusesAVariantNameTwoProvidersClaim(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	writeVariant(t, dir, "llamacpp", "shared+v")
	writeVariant(t, dir, "vllm", "shared+v")

	_, err := LoadAppsConfig(dir)
	require.ErrorIs(t, err, ErrInvalidVariant)
	assert.Contains(t, err.Error(), "on-demand/")
}

func TestAddApp_RefusesAVariantNameAnotherProviderHas(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	writeVariant(t, dir, "llamacpp", "shared+v")
	cfg, err := LoadAppsConfig(dir)
	require.NoError(t, err)

	sc, ok := cfg.LookupApp("llamacpp")
	require.True(t, ok)
	assert.ErrorIs(t, cfg.AddApp("llamacpp-copy", sc), ErrInvalidVariant)
}

// One variant name, one engine, whichever provider was written last.
func TestVariantNames_UniqueAcrossProviders(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	define := func(provider string) error {
		return store.ApplyParameterPatch(provider, func(sc *ServiceConfig) error {
			if sc.Models == nil {
				sc.Models = map[string]ModelSpec{}
			}
			sc.Models["shared+v"] = ModelSpec{From: "w"}
			return nil
		})
	}
	require.NoError(t, define("llamacpp"))
	assert.ErrorIs(t, define("vllm"), ErrInvalidVariant)
}
