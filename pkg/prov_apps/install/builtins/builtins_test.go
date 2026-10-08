package builtins_test

import (
	"errors"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/builtins"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDispatcher_RegisteredInstallers(t *testing.T) {
	d := builtins.NewDispatcher(nil, nil)
	names := d.List()

	// ollama and llama.cpp should always be registered
	assert.Contains(t, names, "ollama")
	assert.Contains(t, names, "llamacpp")
	assert.Contains(t, names, "mlx-vlm")

	// Platform-specific
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		assert.Contains(t, names, "mlx")
	}
	if runtime.GOOS == "linux" {
		assert.Contains(t, names, "vllm")
	}
}

func TestManagedRuntimeOwnPlatformGuards(t *testing.T) {
	sentinel := errors.New("recipe resolution reached")
	d := builtins.NewDispatcher(nil, nil, func(string, string) (install.RecipeSnapshot, error) { return install.RecipeSnapshot{}, sentinel })
	for _, tc := range []struct {
		runtime   string
		platforms []fsroot.Platform
	}{
		{"vllm", []fsroot.Platform{{OS: "linux", Arch: "amd64"}}},
		{"mlx", []fsroot.Platform{{OS: "darwin", Arch: "arm64"}}},
		{"mlx-vlm", []fsroot.Platform{{OS: "darwin", Arch: "arm64"}}},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			inst, err := d.Get(tc.runtime)
			require.NoError(t, err)
			assert.Equal(t, tc.platforms, inst.SupportedPlatforms())
			allowed := false
			for _, platform := range tc.platforms {
				allowed = allowed || platform == fsroot.CurrentPlatform()
			}
			_, err = inst.InstallPlan(t.Context(), "1.0.0")
			if allowed {
				assert.ErrorIs(t, err, sentinel)
			} else {
				assert.ErrorIs(t, err, install.ErrUnsupportedPlatform)
			}
			_, err = inst.UpgradePlan(t.Context(), "1.0.0")
			if allowed {
				assert.ErrorIs(t, err, sentinel)
			} else {
				assert.ErrorIs(t, err, install.ErrUnsupportedPlatform)
			}
		})
	}
}

func TestDispatcher_Get(t *testing.T) {
	d := builtins.NewDispatcher(nil, nil)

	inst, err := d.Get("llamacpp")
	require.NoError(t, err)
	assert.Equal(t, "llamacpp", inst.ProviderName())

	// Normalized lookup: the dotted alias "llama.cpp" still resolves.
	inst, err = d.Get("llama.cpp")
	require.NoError(t, err)
	assert.Equal(t, "llamacpp", inst.ProviderName())

	_, err = d.Get("nonexistent")
	assert.Error(t, err)
}
