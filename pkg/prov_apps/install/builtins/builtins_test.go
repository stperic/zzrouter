package builtins_test

import (
	"errors"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/builtins"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDispatcher_RegisteredInstallers(t *testing.T) {
	d := builtins.NewDispatcher(nil, nil)
	names := d.List()

	assert.Contains(t, names, "ollama")
	assert.Contains(t, names, "llamacpp")
	entries, err := templates.AppsFS.ReadDir("files/providers/on-demand")
	require.NoError(t, err)
	for _, entry := range entries {
		data, err := templates.AppsFS.ReadFile("files/providers/on-demand/" + entry.Name() + "/config.yaml")
		require.NoError(t, err)
		var provider config.OnDemandProvider
		require.NoError(t, yaml.Unmarshal(data, &provider))
		if provider.Install == nil {
			continue
		}
		var platforms []fsroot.Platform
		for _, platform := range provider.Platforms {
			platforms = append(platforms, fsroot.Platform{OS: platform.OS, Arch: platform.Arch})
		}
		for name := range provider.Install.Runtimes {
			t.Run(name, func(t *testing.T) {
				require.Contains(t, names, name)
				inst, err := d.Get(name)
				require.NoError(t, err)
				assert.Equal(t, platforms, inst.SupportedPlatforms())
			})
		}
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
		{"comfyui", []fsroot.Platform{{OS: "linux", Arch: "amd64"}}},
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
			if !allowed {
				report := inst.Preflight(t.Context(), nil)
				assert.False(t, report.AllOK)
				require.Len(t, report.Results, 1)
				assert.Equal(t, "platform", report.Results[0].Check)
				assert.False(t, report.Results[0].Passed)
				assert.Contains(t, report.Results[0].Message, fsroot.CurrentPlatform().String())
				assert.NotEmpty(t, report.Results[0].Hint)
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
