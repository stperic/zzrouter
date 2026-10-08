package pythonvenv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/interpreter"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCandidateFailureDoesNotRestoreStaleBackupMetadata(t *testing.T) {
	directory := t.TempDir()
	previous := filepath.Join(directory, ".previous")
	require.NoError(t, os.MkdirAll(previous, 0700))
	for _, name := range []string{"version", "install.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte("current"), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(previous, name), []byte("stale"), 0600))
	}
	tx := runtimeTransaction{directory: directory, stage: filepath.Join(directory, ".candidate")}
	require.NoError(t, tx.rollback())
	for _, name := range []string{"version", "install.json"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		require.NoError(t, err)
		assert.Equal(t, "current", string(data))
	}
}

func TestActivationFailureRestoresExactPreviousRuntime(t *testing.T) {
	root := t.TempDir()
	fsroot.SetProviderRootOverride(root)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	directory := fsroot.ProviderDir("test-transaction")
	stage := filepath.Join(directory, ".candidate", "test")
	live := filepath.Join(directory, "venv")
	candidate := filepath.Join(stage, "venv")
	require.NoError(t, os.MkdirAll(filepath.Join(live, "bin"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(live, "bin", "old"), []byte("old runtime"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(candidate, "bin"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(candidate, "bin", "python"), []byte("#!/bin/sh\nexit 1\n"), 0700))
	for _, name := range []string{"version", "install.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte("old metadata"), 0600))
	}
	if runtime.GOOS != "windows" {
		require.NoError(t, setTreeWritable(live, false))
		t.Cleanup(func() { _ = setTreeWritable(directory, true) })
	}
	tx := runtimeTransaction{directory: directory, stage: stage}
	checks := schema.RuntimeChecks{Checks: []string{"imports"}, Imports: []string{"json"}, Kernels: "unknown"}
	err := tx.activate(context.Background(), checks, nil, nil, "new", install.RecipeSnapshot{Runtime: "test-transaction"}, "unused", "unused", "3.13", nil, "identity")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rollback completed")
	data, err := os.ReadFile(filepath.Join(live, "bin", "old"))
	require.NoError(t, err)
	assert.Equal(t, "old runtime", string(data))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(live)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0500), info.Mode().Perm())
	}
	for _, name := range []string{"version", "install.json"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		require.NoError(t, err)
		assert.Equal(t, "old metadata", string(data))
	}
	require.NoError(t, tx.rollback(), "a second abort must not restore somebody else's backup")
}

func TestHardenedRuntimeMovePreservesPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permission semantics")
	}
	directory := t.TempDir()
	t.Cleanup(func() { _ = setTreeWritable(directory, true) })
	from, to := filepath.Join(directory, "venv"), filepath.Join(directory, "previous", "venv")
	require.NoError(t, os.MkdirAll(filepath.Dir(to), 0700))
	require.NoError(t, os.Mkdir(from, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(from, "marker"), []byte("preserve"), 0444))
	require.NoError(t, os.Chmod(from, 0555))
	tx := runtimeTransaction{}
	moved, err := tx.moveDirectory(from, to)
	require.NoError(t, err)
	require.True(t, moved)
	for _, path := range []string{to, filepath.Join(to, "marker")} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		want := os.FileMode(0444)
		if path == to {
			want = 0555
		}
		assert.Equal(t, want, info.Mode().Perm())
	}
	moved, err = tx.moveDirectory(to, filepath.Join(directory, "absent", "venv"))
	require.Error(t, err)
	assert.False(t, moved)
	info, err := os.Stat(to)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0555), info.Mode().Perm(), "failed move must restore source hardening")
}

func TestRollbackContinuesAfterMovedDirectoryModeFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permission semantics")
	}
	directory := t.TempDir()
	t.Cleanup(func() { _ = setTreeWritable(directory, true) })
	stage, previous := filepath.Join(directory, "candidate"), filepath.Join(directory, ".previous")
	live := filepath.Join(directory, "venv")
	for _, path := range []string{stage, live, filepath.Join(previous, "venv")} {
		require.NoError(t, os.MkdirAll(path, 0700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(previous, "venv", "old"), []byte("old runtime"), 0444))
	for _, name := range []string{"version", "install.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(previous, name), []byte("old metadata"), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte("new metadata"), 0600))
	}
	require.NoError(t, os.Chmod(live, 0555))
	require.NoError(t, os.Chmod(filepath.Join(previous, "venv"), 0555))
	denied := errors.New("restore mode denied")
	failed := filepath.Join(stage, "failed-venv")
	refused := false
	tx := runtimeTransaction{directory: directory, stage: stage, started: true, activated: true, previous: true, newRecords: true, recordsMoved: []string{"version", "install.json"}, chmod: func(path string, mode os.FileMode) error {
		if path == failed && mode == 0555 && !refused {
			refused = true
			return denied
		}
		return os.Chmod(path, mode)
	}}
	require.ErrorIs(t, tx.rollback(), denied)
	for _, name := range []string{"version", "install.json"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		require.NoError(t, err)
		assert.Equal(t, "old metadata", string(data))
	}
	data, err := os.ReadFile(filepath.Join(live, "old"))
	require.NoError(t, err)
	assert.Equal(t, "old runtime", string(data))
	for _, path := range []string{live, failed} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0555), info.Mode().Perm())
	}
	assert.False(t, tx.activated)
	assert.False(t, tx.previous)
	assert.Empty(t, tx.directoryModes)
	require.NoError(t, tx.rollback())
}

func TestRollbackRetriesPendingHardeningAfterDataRestoration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permission semantics")
	}
	dir := t.TempDir()
	t.Cleanup(func() { _ = setTreeWritable(dir, true) })
	live := filepath.Join(dir, "venv")
	require.NoError(t, os.Mkdir(live, 0755))
	denied := errors.New("hardening denied")
	refuse := true
	tx := runtimeTransaction{directoryModes: map[string]os.FileMode{live: 0555}, chmod: func(path string, mode os.FileMode) error {
		if refuse {
			return denied
		}
		return os.Chmod(path, mode)
	}}
	require.ErrorIs(t, tx.rollback(), denied)
	assert.NotEmpty(t, tx.directoryModes)
	refuse = false
	require.NoError(t, tx.rollback())
	info, err := os.Stat(live)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0555), info.Mode().Perm())
	assert.Empty(t, tx.directoryModes)
}

func TestScriptRelocationPreservesExecutable(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	live := filepath.Join(root, "venv")
	subdir := "bin"
	if runtime.GOOS == "windows" {
		subdir = "Scripts"
	}
	require.NoError(t, os.MkdirAll(filepath.Join(candidate, subdir), 0700))
	path := filepath.Join(candidate, subdir, "engine-script.py")
	require.NoError(t, os.WriteFile(path, []byte("#!"+filepath.Join(candidate, subdir, "python")+"\nprint('ok')\n"), 0750))
	require.NoError(t, relocateScripts(candidate, live))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), filepath.Join(live, subdir, "python"))
	assert.NotContains(t, string(data), candidate)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0750), info.Mode().Perm())
	} else {
		assert.NotZero(t, info.Mode().Perm()&0200)
	}
}

func TestFreshRollbackRemovesNewVersionAndManifest(t *testing.T) {
	directory := t.TempDir()
	stage := filepath.Join(directory, "candidate")
	require.NoError(t, os.MkdirAll(filepath.Join(directory, "venv"), 0700))
	require.NoError(t, os.MkdirAll(stage, 0700))
	for _, name := range []string{"version", "install.json", "install.json.tmp"} {
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte("new metadata"), 0600))
	}
	tx := runtimeTransaction{directory: directory, stage: stage, started: true, activated: true, newRecords: true}
	require.NoError(t, tx.rollback())
	for _, name := range []string{"version", "install.json", "install.json.tmp", "venv"} {
		_, err := os.Stat(filepath.Join(directory, name))
		assert.ErrorIs(t, err, os.ErrNotExist, name)
	}
	require.NoError(t, tx.rollback())
}

func TestRecipeVersionResolutionUsesSelectedPackageAndAction(t *testing.T) {
	root, feature := "engine", "vision-engine"
	empty, desired, approved := "", ">=1", "<3"
	for _, tc := range []struct{ name, pkg, pin, action, explicit, want string }{
		{"base install pin", root, "9", "install", "", "9"},
		{"base upgrade", root, "9", "upgrade", "", "2"},
		{"feature install", feature, "", "install", "", "2"},
		{"feature upgrade", feature, "", "upgrade", "", "2"},
		{"feature explicit", feature, "", "install", "1.5", "1.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			constraint := empty
			if tc.pkg == feature {
				constraint = desired
			}
			snapshot := install.RecipeSnapshot{ResolvedInstall: config.ResolvedInstall{Recipe: config.InstallRecipe{Package: &tc.pkg, VersionConstraint: &constraint}}, PinnedVersion: tc.pin}
			if tc.pkg == feature {
				snapshot.Policy = &install.RuntimePolicy{Packages: map[string]string{tc.pkg: approved}}
			}
			called := false
			version, err := resolveRecipeVersion(t.Context(), snapshot, tc.explicit, tc.action, func(_ context.Context, pkg, constraint string) (upstream.Release, error) {
				called = true
				assert.Equal(t, tc.pkg, pkg)
				if tc.pkg == feature {
					assert.Equal(t, ">=1,<3", constraint)
				}
				return upstream.Release{Tag: "2"}, nil
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, version)
			assert.Equal(t, tc.explicit == "" && tc.want != "9", called)
		})
	}
}

func TestRecipePlanDoesNotReuseCachedInterpreter(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	apps, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	sc, ok := apps.LookupApp("mlx")
	require.True(t, ok)
	sc.Requirements = nil
	snapshot, err := install.ResolveRecipe(sc, "worker", "mlx", "")
	require.NoError(t, err)
	i := New(Config{Name: "mlx", SchemaProvider: "mlx", Recipe: func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }})
	i.chosen = &interpreter.Choice{Path: "/cached/old/python", Realpath: "/cached/old/python", Version: "9.9.9"}
	plan, err := i.InstallPlan(t.Context(), "1.0")
	require.NoError(t, err)
	assert.NotContains(t, plan.Steps[1].Command, "/cached/old/python")
	assert.Contains(t, plan.Steps[1].Command, "-m")
}

func TestTypedUpgradeRetainsCandidateTransaction(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	apps, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	sc, ok := apps.LookupApp("mlx")
	require.True(t, ok)
	sc.Requirements = nil
	snapshot, err := install.ResolveRecipe(sc, "worker", "mlx", "")
	require.NoError(t, err)
	i := New(Config{Name: "mlx", SchemaProvider: "mlx", Platforms: []fsroot.Platform{fsroot.CurrentPlatform()}, Recipe: func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }})
	ctx := install.WithPlanOptions(t.Context(), install.PlanOptions{Action: "upgrade"})
	expected, err := i.InstallPlan(ctx, "1.0")
	require.NoError(t, err)
	actual, err := i.UpgradePlan(t.Context(), "1.0")
	require.NoError(t, err)
	assert.Equal(t, "upgrade", actual.Action)
	assert.Equal(t, expected.PlanID, actual.PlanID)
	expectedJSON, err := json.Marshal(expected)
	require.NoError(t, err)
	actualJSON, err := json.Marshal(actual)
	require.NoError(t, err)
	assert.JSONEq(t, string(expectedJSON), string(actualJSON), "typed upgrade must not add legacy chmod or whole-provider backups")
}

func TestManagedCleanupDoesNotFollowSymlinkAndUnlocksHardenedTree(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	marker := filepath.Join(outside, "marker")
	require.NoError(t, os.WriteFile(marker, []byte("preserve"), 0400))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "outside")))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "venv", "bin"), 0700))
	require.NoError(t, os.Chmod(filepath.Join(dir, "venv", "bin"), 0500))
	require.NoError(t, os.Chmod(filepath.Join(dir, "venv"), 0500))
	require.NoError(t, removeManagedTree(dir))
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "preserve", string(data))
	info, err := os.Stat(marker)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0400), info.Mode().Perm())
	} else {
		// Windows supports the read-only attribute, not POSIX owner bits.
		assert.Zero(t, info.Mode().Perm()&0200)
	}
}
