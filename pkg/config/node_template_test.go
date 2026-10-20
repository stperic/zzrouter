package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	providerschema "github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

func TestComfyUITemplateAndSchemaLoad(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	cfg, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	provider := cfg.GetOnDemand("comfyui")
	require.NotNil(t, provider)
	view, _ := cfg.LookupApp("comfyui")
	require.True(t, view.IsExplicitlyDisabled())
	require.Empty(t, provider.Protocol)
	require.Equal(t, config.RunScopeNode, provider.RunScope)
	require.Equal(t, []config.InstallPlatform{{OS: "linux", Arch: "amd64"}}, provider.Platforms)
	require.Equal(t, "nvidia", provider.Requirements.GPU.Vendor)
	require.Equal(t, []int{8188, 8188}, provider.Runtime.PortRange)
	data, err := os.ReadFile(filepath.Join(dir, "on-demand", "comfyui", "schema.yaml"))
	require.NoError(t, err)
	declared, err := providerschema.LoadYAMLSchema(data)
	require.NoError(t, err)
	_, errs := providerschema.Merge("comfyui", declared, config.ServiceConfig{})
	require.Empty(t, errs)
	require.Equal(t, "reserve_mib", declared.Diagnostics.Memory.Kind)
	require.Equal(t, "reserve-vram", declared.Diagnostics.Memory.ComplementParameter)
}
