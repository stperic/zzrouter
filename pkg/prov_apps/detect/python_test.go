package detect

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/assert"
)

// TestProviderPython_PrefersVenvOverRuntimeCommand covers the load-bearing
// case: a provider with a managed venv on disk must resolve to the venv's
// python, not the literal Runtime.Execution.Command from YAML. Skipping
// this check was the original "provider not registered" bug.
func TestProviderPython_PrefersVenvOverRuntimeCommand(t *testing.T) {
	providerName := "probe-venv-provider"
	tmp := t.TempDir()
	fsroot.SetProviderRootOverride(tmp)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	// Stage a venv python where fsroot expects it.
	venvPython := fsroot.ProviderVenvPython(providerName)
	require := assert.New(t)
	require.NoError(os.MkdirAll(filepath.Dir(venvPython), 0o755))
	require.NoError(os.WriteFile(venvPython, []byte("#!/bin/sh\n"), 0o755))

	sc := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{Type: "python", Command: "python3"},
		},
	}
	got := ProviderPython(providerName, sc)
	assert.Equal(t, venvPython, got)
}

func TestProviderPython_FallsBackToRuntimeCommand(t *testing.T) {
	providerName := "probe-no-venv-provider"
	tmp := t.TempDir()
	fsroot.SetProviderRootOverride(tmp)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	// No venv staged. Runtime.Execution.Command is honored.
	sc := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{Type: "python", Command: "/opt/custom/python3"},
		},
	}
	assert.Equal(t, "/opt/custom/python3", ProviderPython(providerName, sc))
}

func TestProviderPython_NilConfigFallsBackToDefault(t *testing.T) {
	// Caller with no config and no venv gets the "python3" default so the
	// spawn path never returns an empty command.
	tmp := t.TempDir()
	fsroot.SetProviderRootOverride(tmp)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	assert.Equal(t, "python3", ProviderPython("ghost-provider", nil))
}

// Windows resolution is covered by fsroot.ProviderVenvPython — the test
// above exercises the darwin/linux shape; guard here so future refactors
// don't drop the platform-aware venv path silently.
func TestProviderPython_UsesPlatformVenvLayout(t *testing.T) {
	providerName := "probe-platform-provider"
	tmp := t.TempDir()
	fsroot.SetProviderRootOverride(tmp)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	venvPython := fsroot.ProviderVenvPython(providerName)
	switch runtime.GOOS {
	case "windows":
		assert.Contains(t, venvPython, filepath.Join("venv", "Scripts", "python.exe"))
	default:
		assert.Contains(t, venvPython, filepath.Join("venv", "bin", "python3"))
	}
}
