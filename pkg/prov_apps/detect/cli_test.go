package detect

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProviderCLI_PrefersProviderBinDir is the load-bearing case: when
// coord ships `command: "llama-server"` and the worker has installed
// llama.cpp under providers/<name>/bin, spawn must resolve to the
// absolute path even though the worker's PATH does not include the
// provider tree.
func TestProviderCLI_PrefersProviderBinDir(t *testing.T) {
	name := "probe-cli-bin"
	tmp := t.TempDir()
	fsroot.SetProviderRootOverride(tmp)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	bin := filepath.Join(fsroot.ProviderBinDir(name), "llama-server")
	require := assert.New(t)
	require.NoError(os.MkdirAll(filepath.Dir(bin), 0o755))
	require.NoError(os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	sc := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{Type: "cli", Command: "llama-server"},
		},
	}
	assert.Equal(t, bin, ProviderCLI(name, sc))
}

// TestProviderCLI_FallsBackToVenvBin covers pythonvenv-style installers
// (vllm, mlx_lm.*) that drop console scripts under providers/<name>/venv/bin.
func TestProviderCLI_FallsBackToVenvBin(t *testing.T) {
	name := "probe-cli-venv"
	tmp := t.TempDir()
	fsroot.SetProviderRootOverride(tmp)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	bin := filepath.Join(fsroot.ProviderVenvDir(name), "bin", "vllm")
	require := assert.New(t)
	require.NoError(os.MkdirAll(filepath.Dir(bin), 0o755))
	require.NoError(os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	sc := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{Type: "cli", Command: "vllm"},
		},
	}
	assert.Equal(t, bin, ProviderCLI(name, sc))
}

// TestProviderCLI_HonorsAbsolutePath confirms operator-supplied absolute
// paths win over any managed-tree probe.
func TestProviderCLI_HonorsAbsolutePath(t *testing.T) {
	name := "probe-cli-abs"
	tmp := t.TempDir()
	fsroot.SetProviderRootOverride(tmp)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	abs := "/opt/custom/llama-server"
	sc := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{Type: "cli", Command: abs},
		},
	}
	assert.Equal(t, abs, ProviderCLI(name, sc))
}

// TestProviderCLI_ReturnsBareNameOnMiss preserves PATH-lookup behavior
// for system-installed providers (e.g. Ollama from a package manager).
func TestProviderCLI_ReturnsBareNameOnMiss(t *testing.T) {
	name := "probe-cli-miss"
	tmp := t.TempDir()
	fsroot.SetProviderRootOverride(tmp)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	sc := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{Type: "cli", Command: "ollama"},
		},
	}
	assert.Equal(t, "ollama", ProviderCLI(name, sc))
}

// TestProviderCLI_NilGuards keeps the resolver safe to call without a
// runtime block — early shutdown / partially-loaded configs should not
// panic.
func TestProviderCLI_NilGuards(t *testing.T) {
	assert.Equal(t, "", ProviderCLI("any", nil))
	assert.Equal(t, "", ProviderCLI("any", &config.ServiceConfig{}))
	assert.Equal(t, "", ProviderCLI("any", &config.ServiceConfig{Runtime: &config.AppRuntimeConfig{}}))
}

// TestProviderCLI_ResolvesAnotherNodesAbsolutePath is the cluster case.
// execution.command is cluster-shared config, so an absolute path
// written on one node is handed to every other node, where it names
// nothing. Falling back to this node's own copy of the same binary is
// what keeps the worker running when the coordinator's path arrives.
func TestProviderCLI_ResolvesAnotherNodesAbsolutePath(t *testing.T) {
	name := "probe-cli-foreign"
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	bin := filepath.Join(fsroot.ProviderBinDir(name), "llama-server")
	require.NoError(t, os.MkdirAll(filepath.Dir(bin), 0o755))
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	sc := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{
				Type: "cli",
				// A macOS coordinator's path, as seen by a Linux worker.
				Command: "/Users/someone/Library/Application Support/zzrouter/providers/" + name + "/bin/llama-server",
			},
		},
	}
	assert.Equal(t, bin, ProviderCLI(name, sc))
}

// TestProviderCLI_ExistingAbsolutePathWinsOverManaged pins the operator
// override against the fallback added for the cluster case: when the
// path in config resolves here, it is used even though a managed binary
// of the same name is present.
func TestProviderCLI_ExistingAbsolutePathWinsOverManaged(t *testing.T) {
	name := "probe-cli-override"
	tmp := t.TempDir()
	fsroot.SetProviderRootOverride(tmp)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	managed := filepath.Join(fsroot.ProviderBinDir(name), "llama-server")
	require.NoError(t, os.MkdirAll(filepath.Dir(managed), 0o755))
	require.NoError(t, os.WriteFile(managed, []byte("#!/bin/sh\n"), 0o755))

	custom := filepath.Join(tmp, "llama-server")
	require.NoError(t, os.WriteFile(custom, []byte("#!/bin/sh\n"), 0o755))

	sc := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{Type: "cli", Command: custom},
		},
	}
	assert.Equal(t, custom, ProviderCLI(name, sc))
}

// TestProviderCLI_BareNameIgnoresWorkingDirectory guards the stat added
// for the cluster case. A bare command must never be stat-ed relative to
// the process working directory, or an unrelated file that happens to
// share its name gets spawned instead of the installed provider.
//
// A managed binary is staged as well as the working-directory file,
// because a bare name resolved from the working directory returns the
// bare name itself — indistinguishable from the correct answer unless
// there is a managed path to return instead.
func TestProviderCLI_BareNameIgnoresWorkingDirectory(t *testing.T) {
	name := "probe-cli-cwd"
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	managed := filepath.Join(fsroot.ProviderBinDir(name), "llama-server")
	require.NoError(t, os.MkdirAll(filepath.Dir(managed), 0o755))
	require.NoError(t, os.WriteFile(managed, []byte("#!/bin/sh\n"), 0o755))

	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile("llama-server", []byte("#!/bin/sh\n"), 0o755))

	sc := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{Type: "cli", Command: "llama-server"},
		},
	}
	assert.Equal(t, managed, ProviderCLI(name, sc))
}

// TestProviderCLI_WindowsVenvScripts covers the two spellings Windows
// needs: console scripts land in venv\Scripts, and the installed file
// carries the .exe that provider config does not. Runs only on Windows,
// where those paths are the real ones.
func TestProviderCLI_WindowsVenvScripts(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("resolution of Scripts\\ and .exe applies to Windows only")
	}
	name := "probe-cli-windows"
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	bin := filepath.Join(fsroot.ProviderVenvDir(name), "Scripts", "vllm.exe")
	require.NoError(t, os.MkdirAll(filepath.Dir(bin), 0o755))
	require.NoError(t, os.WriteFile(bin, []byte("MZ"), 0o755))

	sc := &config.ServiceConfig{
		Runtime: &config.AppRuntimeConfig{
			Execution: config.ExecutionConfig{Type: "cli", Command: "vllm"},
		},
	}
	assert.Equal(t, bin, ProviderCLI(name, sc))
}
