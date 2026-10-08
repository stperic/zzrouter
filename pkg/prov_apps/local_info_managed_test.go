package prov_apps

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// "Installed" and "managed" are two questions, and the installer's
// IsInstalled answers only the second: it requires a .managed marker.
// Reading it as the whole answer made a provider the operator installed
// themselves report installed:false in the provider list, while
// /providers/{name}/status reported installed:true for the same binary
// because it asks the broader question.
func TestLocalInfoSeparatesManagedFromInstalled(t *testing.T) {
	infoFor := func(t *testing.T, m *ProviderAppManager, key string) LocalProviderInfo {
		t.Helper()
		for _, info := range m.LocalInfo("test-node") {
			if info.Key == key {
				return info
			}
		}
		t.Fatalf("provider %q missing from LocalInfo", key)
		return LocalProviderInfo{}
	}

	newManager := func(t *testing.T) *ProviderAppManager {
		t.Helper()
		isolateProviderRoot(t)
		m, err := NewProviderAppManager(testAppsConfig())
		require.NoError(t, err)
		return m
	}

	t.Run("no marker and no version is not installed at all", func(t *testing.T) {
		info := infoFor(t, newManager(t), "ollama")

		assert.False(t, info.State.IsInstalled())
		assert.False(t, info.Managed)
	})

	// The case the split exists for.
	t.Run("a detected version is installed but not managed", func(t *testing.T) {
		m := newManager(t)
		m.SetProviderVersion("ollama", "0.32.15")

		info := infoFor(t, m, "ollama")

		assert.True(t, info.State.IsInstalled(),
			"something usable is on disk; refusing to say so is what made the two endpoints disagree")
		assert.False(t, info.Managed,
			"zzRouter did not put it there, so removing it is not ours to offer")
	})

	// Without this guard, every provider enabled by default in the
	// template reports installed on the strength of being enabled --
	// vllm on macOS being the standing example.
	t.Run("the unknown sentinel is not a detection", func(t *testing.T) {
		m := newManager(t)
		m.SetProviderVersion("vllm", versionUnknown)

		info := infoFor(t, m, "vllm")

		assert.False(t, info.State.IsInstalled())
		assert.False(t, info.Managed)
	})
}
