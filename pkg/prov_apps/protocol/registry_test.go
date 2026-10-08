package protocol

import (
	"sync"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAppsConfig builds a minimal multi-kind config for registry tests.
// Each provider goes through AddApp so per-kind Validate runs — mirrors
// the production load path.
func testAppsConfig() *config.AppsConfig {
	caps := func(eps ...string) *config.AppCapabilities {
		return &config.AppCapabilities{WireEndpoints: eps}
	}
	apps := map[string]config.ServiceConfig{
		"ollama": {
			Enabled:  new(true),
			Name:     "Ollama",
			Protocol: config.ProtocolOllama,
			Mode:     "service",
			Runtime: &config.AppRuntimeConfig{
				Endpoint: "http://localhost:11434",
			},
			Capabilities: caps("chat_completions"),
		},
		"vllm": {
			Enabled:  new(true),
			Name:     "vLLM",
			Protocol: config.ProtocolOpenAI,
			Mode:     "on-demand",
			Runtime: &config.AppRuntimeConfig{
				BasePort: 8000,
				Execution: config.ExecutionConfig{
					Type:    "cli",
					Command: "vllm",
				},
			},
			Capabilities: caps("chat_completions"),
		},
		"disabled-app": {
			Enabled:  new(false),
			Name:     "Disabled",
			Protocol: config.ProtocolOpenAI,
			Mode:     "on-demand",
			Runtime: &config.AppRuntimeConfig{
				BasePort: 8100,
				Execution: config.ExecutionConfig{
					Type:    "cli",
					Command: "stub",
				},
			},
			Capabilities: caps("chat_completions"),
		},
	}
	cfg := &config.AppsConfig{}
	for name, sc := range apps {
		if err := cfg.AddApp(name, sc); err != nil {
			panic("testAppsConfig: AddApp(" + name + ") failed: " + err.Error())
		}
	}
	return cfg
}

func TestNewRegistry(t *testing.T) {
	reg := NewRegistry(testAppsConfig(), nil)

	assert.Equal(t, 2, reg.Count()) // disabled skipped
	assert.True(t, reg.Has("ollama"))
	assert.True(t, reg.Has("vllm"))
	assert.False(t, reg.Has("disabled-app"))
}

func TestNewRegistry_NilConfig(t *testing.T) {
	reg := NewRegistry(nil, nil)
	assert.Equal(t, 0, reg.Count())
}

func TestRegistry_Get(t *testing.T) {
	reg := NewRegistry(testAppsConfig(), nil)

	p, err := reg.Get("ollama")
	require.NoError(t, err)
	assert.Equal(t, "ollama", p.Name())

	p, err = reg.Get("vllm")
	require.NoError(t, err)
	assert.Equal(t, "vllm", p.Name())

	_, err = reg.Get("nonexistent")
	assert.Error(t, err)
}

func TestRegistry_List(t *testing.T) {
	reg := NewRegistry(testAppsConfig(), nil)

	keys := reg.List()
	assert.Len(t, keys, 2)
	assert.Contains(t, keys, "ollama")
	assert.Contains(t, keys, "vllm")
}

func TestRegistry_GetAll(t *testing.T) {
	reg := NewRegistry(testAppsConfig(), nil)

	all := reg.GetAll()
	assert.Len(t, all, 2)

	// Verify it's a copy (modifying it shouldn't affect registry)
	delete(all, "ollama")
	assert.True(t, reg.Has("ollama"))
}

func TestRegistry_Concurrent(t *testing.T) {
	reg := NewRegistry(testAppsConfig(), nil)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			_, _ = reg.Get("ollama")
			_, _ = reg.Get("vllm")
			reg.Has("ollama")
			reg.List()
			reg.GetAll()
			reg.Count()
		})
	}
	wg.Wait()
}

func TestRegistry_CloudProvider(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("openrouter", config.ServiceConfig{
		Enabled:  new(true),
		Name:     "OpenRouter",
		Protocol: config.ProtocolOpenAI,
		Mode:     "cloud",
		Runtime: &config.AppRuntimeConfig{
			Endpoint: "https://openrouter.ai/api",
			API: &config.APIConfig{
				AuthType: "bearer",
				Token:    "test-token",
			},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions", "completions"}},
	}))

	reg := NewRegistry(cfg, nil)
	p, err := reg.Get("openrouter")
	require.NoError(t, err)
	assert.Equal(t, "openrouter", p.Name())
}

func TestRegistry_UnknownProtocol(t *testing.T) {
	// Unknown protocol values used to fall through to the default OpenAI
	// provider in createProvider. The new typed-Provider path tightens
	// this: OnDemandProvider.Validate rejects protocols outside
	// {openai, ollama}, so AddApp fails fast.
	cfg := &config.AppsConfig{}
	err := cfg.AddApp("custom", config.ServiceConfig{
		Enabled:  new(true),
		Name:     "Custom",
		Protocol: "unknown-protocol",
		Mode:     "on-demand",
		Runtime: &config.AppRuntimeConfig{
			BasePort: 8200,
			Execution: config.ExecutionConfig{
				Type:    "cli",
				Command: "stub",
			},
		},
	})
	require.Error(t, err, "unknown protocol should be rejected by per-kind Validate")
}

func TestOllamaProvider_Identity(t *testing.T) {
	p := NewOllamaProvider(nil)
	assert.Equal(t, "ollama", p.Name())
	assert.Equal(t, "ollama", p.Type())
	assert.Equal(t, 11434, p.DefaultPort())
}

func TestOpenAIProvider_Identity(t *testing.T) {
	p := NewOpenAIProvider("vllm", 8000, "openai", nil)
	assert.Equal(t, "vllm", p.Name())
	assert.Equal(t, 8000, p.DefaultPort())
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "short", truncate("short", 15))
	assert.Equal(t, "123456789012...", truncate("1234567890123456", 15))
	assert.Equal(t, "ab", truncate("abcdef", 2))
}
