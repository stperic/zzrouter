package prov_apps

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

// pinnedProvider is the smallest ServiceConfig AddApp will accept for
// the given mode, carrying a pin.
func pinnedProvider(mode, pin string) config.ServiceConfig {
	rt := &config.AppRuntimeConfig{Endpoint: "http://127.0.0.1:11434"}
	if mode == constants.AppModeOnDemand {
		rt.BasePort = 18080
		rt.PortRange = []int{18080, 18089}
		rt.Execution = config.ExecutionConfig{Type: "cli", Command: "engine-server"}
	}
	return config.ServiceConfig{
		Protocol:      config.ProtocolOllama,
		Mode:          mode,
		PinnedVersion: pin,
		Runtime:       rt,
		Capabilities:  &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}
}

// A pin is written through LookupApp, which accepts any provider mode.
// Reading it back through GetOnDemand meant a pin on ollama — an
// external provider that zzRouter nonetheless installs and versions —
// was persisted, reported as "pinned to X", and then ignored by every
// install.
func TestResolveVersionHonorsPinRegardlessOfMode(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("ollama", pinnedProvider(constants.AppModeExternal, "0.32.14")))
	require.NoError(t, cfg.AddApp("llamacpp", pinnedProvider(constants.AppModeOnDemand, "b10549")))

	m := &ProviderAppManager{appsConfig: cfg}

	assert.Equal(t, "0.32.14", m.ResolveVersion("ollama", ""),
		"an external provider zzRouter installs must honor its pin")
	assert.Equal(t, "b10549", m.ResolveVersion("llamacpp", ""),
		"on-demand pins must keep working")
}

// An explicit version outranks the pin, and an unpinned or unknown
// provider resolves to nothing rather than guessing.
func TestResolveVersionExplicitAndUnpinned(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("ollama", pinnedProvider(constants.AppModeExternal, "0.32.14")))
	require.NoError(t, cfg.AddApp("vllm", pinnedProvider(constants.AppModeOnDemand, "")))

	m := &ProviderAppManager{appsConfig: cfg}

	assert.Equal(t, "0.99.0", m.ResolveVersion("ollama", "0.99.0"),
		"an explicit version outranks the pin")
	assert.Empty(t, m.ResolveVersion("vllm", ""), "no pin, no version")
	assert.Empty(t, m.ResolveVersion("nosuch", ""), "unknown provider resolves to nothing")
}
