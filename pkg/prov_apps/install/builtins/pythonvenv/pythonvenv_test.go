package pythonvenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstaller_IsInstalled_GatesOnVersionFile(t *testing.T) {
	previousRoot := fsroot.ProviderRootDir()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previousRoot) })

	name := "test-c3-partial-install"

	inst := New(Config{
		Name:       name,
		PipPackage: "x",
		ImportName: "x",
		Platforms:  []fsroot.Platform{fsroot.CurrentPlatform()},
	})

	// Partial-install corpse: venv python skeleton present, version file absent.
	// Before C3, IsInstalled would return true here and block recovery.
	venvBin := filepath.Dir(fsroot.ProviderVenvPython(name))
	require.NoError(t, os.MkdirAll(venvBin, 0750))
	require.NoError(t, os.WriteFile(fsroot.ProviderVenvPython(name), []byte("#!/bin/sh\n"), 0755))
	assert.False(t, inst.IsInstalled(), "partial install (venv without version file) must report not installed")

	// Completed install: version file written as the terminal step.
	require.NoError(t, os.WriteFile(fsroot.ProviderVersionFile(name), []byte("1.0.0\n"), 0644))
	assert.True(t, inst.IsInstalled(), "version file present means installed")
}

func TestHardenVenvToggleShapesPlan(t *testing.T) {
	// Default: harden step present. Expect 6 steps for the default
	// verify-can-load path (no VerifyStep override in cfg).
	fsroot.SetHardenVenvDefault(true)
	t.Cleanup(func() { fsroot.SetHardenVenvDefault(true) })

	inst := New(Config{
		Name:       "test-harden-toggle",
		PipPackage: "x",
		ImportName: "x",
		Platforms:  []fsroot.Platform{fsroot.CurrentPlatform()},
	})

	planOn, err := inst.InstallPlan(context.Background(), "1.0.0")
	require.NoError(t, err)

	// Turn hardening off — the chmod step disappears, every other
	// step stays; numbering shifts by one.
	fsroot.SetHardenVenvDefault(false)
	planOff, err := inst.InstallPlan(context.Background(), "1.0.0")
	require.NoError(t, err)

	assert.Equal(t, len(planOn.Steps)-1, len(planOff.Steps), "hardening off should remove exactly one step")

	// Explicit: no step in the off-plan mentions hardening.
	for _, step := range planOff.Steps {
		assert.NotContains(t, step.Description, "read-only", "harden=false must omit the chmod step")
	}
	// Terminal two steps are Record-version then Record-manifest in both.
	// Manifest is terminal so HealthCheck can distinguish "partial install,
	// no manifest yet" from "complete install".
	assert.Contains(t, planOn.Steps[len(planOn.Steps)-2].Description, "Record installed version")
	assert.Contains(t, planOn.Steps[len(planOn.Steps)-1].Description, "Record install manifest")
	assert.Contains(t, planOff.Steps[len(planOff.Steps)-2].Description, "Record installed version")
	assert.Contains(t, planOff.Steps[len(planOff.Steps)-1].Description, "Record install manifest")
}

func TestInstaller_PlatformCheck(t *testing.T) {
	inst := New(Config{
		Name:       "mlx",
		PipPackage: "mlx-lm",
		ImportName: "mlx_lm",
		Platforms:  []fsroot.Platform{{OS: "darwin", Arch: "arm64"}},
	})

	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		_, err := inst.InstallPlan(context.Background(), "0.1.0")
		require.Error(t, err)
		assert.ErrorIs(t, err, install.ErrUnsupportedPlatform)
	} else {
		plan, err := inst.InstallPlan(context.Background(), "0.1.0")
		require.NoError(t, err)
		assert.Equal(t, "mlx", plan.Provider)
	}
}

func TestTypedRuntimePlatformCheckPrecedesRecipeResolution(t *testing.T) {
	unsupported := fsroot.Platform{OS: "linux", Arch: "arm64"}
	if runtime.GOOS == "linux" {
		unsupported.OS = "darwin"
	}
	called := false
	inst := New(Config{Name: "mlx-vlm", Platforms: []fsroot.Platform{unsupported}, Recipe: func(string, string) (install.RecipeSnapshot, error) {
		called = true
		return install.RecipeSnapshot{}, errors.New("recipe resolution must not run")
	}})
	_, err := inst.InstallPlan(t.Context(), "0.7.6")
	assert.ErrorIs(t, err, install.ErrUnsupportedPlatform)
	assert.False(t, called)
	_, err = inst.UpgradePlan(t.Context(), "0.7.6")
	assert.ErrorIs(t, err, install.ErrUnsupportedPlatform)
	assert.False(t, called)
}

func TestMLXVisionAppleSiliconPlatformPredicate(t *testing.T) {
	inst := New(Config{Name: "mlx-vlm", Platforms: []fsroot.Platform{{OS: "darwin", Arch: "arm64"}}})
	for _, platform := range []fsroot.Platform{{OS: "linux", Arch: "amd64"}, {OS: "windows", Arch: "amd64"}, {OS: "darwin", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"}} {
		t.Run(platform.String(), func(t *testing.T) {
			err := inst.checkPlatform(platform)
			if platform.OS == "darwin" && platform.Arch == "arm64" {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, install.ErrUnsupportedPlatform)
			}
		})
	}
}

func TestInstaller_Preflight(t *testing.T) {
	previousRoot := fsroot.ProviderRootDir()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previousRoot) })

	inst := New(Config{
		Name:       "test",
		PipPackage: "test-pkg",
		ImportName: "test_pkg",
		Platforms:  []fsroot.Platform{{OS: runtime.GOOS, Arch: runtime.GOARCH}},
	})
	reqs := &config.AppRequirements{Python: &config.PythonRequirement{Min: "3.9"}, DiskSpace: "100MB"}
	report := inst.Preflight(context.Background(), reqs)
	assert.Equal(t, "test", report.Provider)
	assert.NotEmpty(t, report.Results)
}
