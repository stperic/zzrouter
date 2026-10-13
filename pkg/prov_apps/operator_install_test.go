package prov_apps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
)

func TestNodeProviderOperatorInstallStateAndRefusal(t *testing.T) {
	root := fsroot.ProviderRootDir()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride(root) })
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("operator-studio", config.ServiceConfig{
		Name: "operator-studio", RunScope: config.RunScopeNode, Mode: "on-demand", Enabled: new(true),
		Runtime: &config.AppRuntimeConfig{BasePort: 8900, PortRange: []int{8900, 8901}, Execution: config.ExecutionConfig{Type: "python", Command: "python", Args: []string{"main.py"}, WorkingDir: "source"}},
	}))
	mgr, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	cleanupProviderManager(t, mgr)
	mgr.SetProviderVersion("operator-studio", "external-version")
	status, err := mgr.ProviderStatus("operator-studio")
	require.NoError(t, err)
	require.False(t, status.Installed)
	_, err = mgr.Install().Installer("operator-studio")
	require.ErrorIs(t, err, ErrParameterValidation)
	require.Contains(t, err.Error(), "operator-provided")
	require.Contains(t, err.Error(), fsroot.ProviderDir("operator-studio"))
	interpreter := fsroot.ProviderVenvPython("operator-studio")
	source := filepath.Join(fsroot.ProviderDir("operator-studio"), "source")
	require.NoError(t, os.MkdirAll(filepath.Dir(interpreter), 0o700))
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.WriteFile(interpreter, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	status, err = mgr.ProviderStatus("operator-studio")
	require.NoError(t, err)
	require.False(t, status.Installed, "an interpreter without the source is not an install")
	require.NoError(t, os.WriteFile(filepath.Join(source, "main.py"), nil, 0o600))
	status, err = mgr.ProviderStatus("operator-studio")
	require.NoError(t, err)
	require.True(t, status.Installed)
	require.False(t, status.Managed)
	infos := mgr.LocalInfo("node")
	require.Len(t, infos, 1)
	require.Equal(t, config.RunScopeNode, infos[0].RunScope)
	require.Equal(t, StateLive, infos[0].State)
	require.False(t, infos[0].SupportsRouting)
	require.False(t, infos[0].SupportsDownload)
}
