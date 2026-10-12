package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunSettingsRoundTripAndValidation(t *testing.T) {
	for _, mode := range []string{"on-demand", "external"} {
		t.Run(mode, func(t *testing.T) {
			sc := ServiceConfig{Name: "test", Mode: mode, Protocol: ProtocolOllama, Enabled: new(true), Runtime: &AppRuntimeConfig{KeepAlive: "1m", Endpoint: "http://daemon:11434", BasePort: 8100, PortRange: []int{8100, 8110}, Execution: ExecutionConfig{Type: "cli"}}, Capabilities: &AppCapabilities{WireEndpoints: []string{"chat_completions"}}, Defaults: &AppDefaultsConfig{Parameters: map[string]string{KeepAliveParameter: "2m", EvictIdleParameter: "true"}}, Models: map[string]ModelSpec{"m": {Parameters: map[string]string{KeepAliveParameter: "3m"}}}, Nodes: map[string]NodeSpec{"n": {Parameters: map[string]string{KeepAliveParameter: "4m"}, Models: map[string]NodeModelSpec{"m": {Parameters: map[string]string{KeepAliveParameter: "-1", EvictIdleParameter: "false"}}}}}}
			cfg := &AppsConfig{}
			require.NoError(t, cfg.AddApp("test", sc))
			dir := t.TempDir()
			require.NoError(t, cfg.SaveToDir(dir))
			loaded, err := LoadAppsConfig(dir)
			require.NoError(t, err)
			got, ok := loaded.LookupApp("test")
			require.True(t, ok)
			settings, err := ParseRunSettings(FlattenParameters(got.Resolve("n", "m").Parameters), true)
			require.NoError(t, err)
			assert.Negative(t, settings.KeepAlive)
			assert.True(t, settings.KeepAliveExplicit)
			assert.False(t, settings.EvictIdle, "literal false survives typed view and disk")
			require.NoError(t, loaded.UpdateApp("test", func(s *ServiceConfig) error {
				delete(s.Nodes["n"].Models["m"].Parameters, EvictIdleParameter)
				return nil
			}))
			require.NoError(t, loaded.SaveToDir(dir))
			loaded, err = LoadAppsConfig(dir)
			require.NoError(t, err)
			got, _ = loaded.LookupApp("test")
			settings, err = ParseRunSettings(FlattenParameters(got.Resolve("n", "m").Parameters), false)
			require.NoError(t, err)
			assert.True(t, settings.EvictIdle, "deleted cell inherits defaults")
			err = loaded.UpdateApp("test", func(s *ServiceConfig) error { s.Defaults.Parameters[KeepAliveParameter] = "invalid"; return nil })
			require.ErrorContains(t, err, "keep_alive", "invalid lower tier cannot hide behind a valid upper tier")
			err = loaded.UpdateApp("test", func(s *ServiceConfig) error {
				s.Defaults.Endpoints = map[string]EndpointOverlay{"chat": {Parameters: map[string]string{EvictIdleParameter: "yes"}}}
				return nil
			})
			require.ErrorContains(t, err, EvictIdleParameter)
		})
	}
	s, err := ParseRunSettings(nil, false)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, s.KeepAlive)
	assert.False(t, s.KeepAliveExplicit)
}

func TestRunSettingsRejectReservedAliases(t *testing.T) {
	for _, key := range []string{"keep_alive", "evict_idle_on_memory_pressure"} {
		_, err := ParseRunSettings(map[string]string{key: "true"}, false)
		require.ErrorContains(t, err, "canonical")
	}
}
