package pythonvenv

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

type savedFile struct {
	Data string
	Mode fs.FileMode
}

func savedTree(t *testing.T, root string) map[string]savedFile {
	t.Helper()
	result := map[string]savedFile{}
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := savedFile{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value.Data = string(data)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			value.Data, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		result[relative] = value
		return nil
	}))
	return result
}

func TestSourceDataCreatesOnlyRootAndModelStore(t *testing.T) {
	for _, models := range []bool{false, true} {
		t.Run(fmt.Sprintf("model_store=%t", models), func(t *testing.T) {
			directory := t.TempDir()
			runtime := &config.AppRuntimeConfig{DataDir: "data"}
			want := []string{"."}
			if models {
				runtime.ModelStore = &config.ModelStoreConfig{Directory: "models", Categories: []string{"vae", "checkpoints"}}
				want = append(want, "models", "models/checkpoints", "models/vae")
			}
			require.NoError(t, ensureSourceData(runtime, directory, false))
			_, err := os.Stat(filepath.Join(directory, "data"))
			require.ErrorIs(t, err, os.ErrNotExist, "admission must not create data")
			require.NoError(t, ensureSourceData(runtime, directory, true))
			var got []string
			require.NoError(t, filepath.WalkDir(filepath.Join(directory, "data"), func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				require.True(t, entry.IsDir())
				relative, err := filepath.Rel(filepath.Join(directory, "data"), path)
				got = append(got, relative)
				return err
			}))
			require.Equal(t, want, got, "application-owned folders must be left to the application")
			before := savedTree(t, filepath.Join(directory, "data"))
			require.NoError(t, ensureSourceData(runtime, directory, true))
			require.Equal(t, before, savedTree(t, filepath.Join(directory, "data")))
		})
	}
}

func sourceLifecycle(t *testing.T) (*Installer, install.RecipeSnapshot, *runtimeTransaction, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	fsroot.SetHardenVenvDefault(true)
	t.Cleanup(func() { fsroot.SetHardenVenvDefault(true) })
	snapshot := sourceSnapshot()
	snapshot.WorkingDir = "payload/source"
	snapshot.DataRuntime = &config.AppRuntimeConfig{DataDir: "data", ModelStore: &config.ModelStoreConfig{Directory: "models", Categories: []string{"vae", "checkpoints"}}}
	directory := fsroot.ProviderDir(snapshot.Runtime)
	stage := filepath.Join(directory, config.SubdirCandidate, "plan")
	require.NoError(t, os.MkdirAll(stage, 0700))
	t.Cleanup(func() { _ = setTreeWritable(directory, true, os.Chmod) })
	for _, name := range snapshot.PayloadDirectories() {
		for _, root := range []string{directory, stage} {
			require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0750))
			marker := "old"
			if root == stage {
				marker = "new"
			}
			require.NoError(t, os.WriteFile(filepath.Join(root, name, "marker"), []byte(marker), 0640))
		}
		require.NoError(t, setTreeWritable(filepath.Join(directory, name), false, os.Chmod))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(stage, "venv", "bin"), 0700))
	probe := "#!/bin/sh\nif [ \"$1\" = \"-I\" ]; then\n printf '%s\\n' 'ZZROUTER_RUNTIME_CHECKS=[{\"name\":\"imports\",\"passed\":true},{\"name\":\"pip_check\",\"passed\":true}]'\nelse\n exec " + fsroot.ShellQuote(python) + " \"$@\"\nfi\n"
	require.NoError(t, os.WriteFile(venvPython(filepath.Join(stage, "venv")), []byte(probe), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(stage, snapshot.WorkingDir, "main.py"), []byte("import argparse\nargparse.ArgumentParser().parse_args()\n"), 0600))
	require.NoError(t, ensureSourceData(snapshot.DataRuntime, directory, true))
	for _, name := range []string{"models/vae", "models/checkpoints", "input", "output", "user", "custom_nodes", "temp"} {
		require.NoError(t, os.MkdirAll(filepath.Join(directory, "data", name), 0750))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "data", name, "operator.bin"), []byte("operator data: "+name), 0640))
		require.NoError(t, os.Chmod(filepath.Join(directory, "data", name), 0750))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(directory, "python"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "python", "keep"), []byte("operator interpreter"), 0600))
	require.NoError(t, os.WriteFile(fsroot.ProviderVersionFile(snapshot.Runtime), []byte("1.0.0\n"), 0440))
	manifest := &fsroot.InstallManifest{WorkingDir: snapshot.WorkingDir, Version: "1.0.0", PlanID: "previous", Recipe: &snapshot.Recipe, RecipeFingerprint: install.Fingerprint(snapshot.Recipe), Interpreter: &fsroot.ManifestInterpreter{Path: python, Version: "3.12.1"}, Inventory: []fsroot.ManifestPackage{{Name: "dependency", Version: "1", SHA256: strings.Repeat("b", 64)}}}
	require.NoError(t, fsroot.WriteInstallManifest(snapshot.Runtime, manifest))
	require.NoError(t, os.Chmod(fsroot.ProviderManifestPath(snapshot.Runtime), 0440))
	source := *snapshot.Recipe.Source
	source.Version = "2.0.0"
	snapshot.Recipe.Source = &source
	snapshot.Fingerprint = install.Fingerprint(snapshot.Recipe)
	installer := New(Config{Name: snapshot.Runtime, Recipe: func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }})
	return installer, snapshot, &runtimeTransaction{directory: directory, stage: stage}, python
}

func activateSource(t *testing.T, tx *runtimeTransaction, snapshot install.RecipeSnapshot, python string) error {
	t.Helper()
	return tx.activate(t.Context(), schema.RuntimeChecks{Checks: []string{"imports", "pip_check"}, Imports: []string{"json"}, Kernels: "unknown"}, nil, nil, "2.0.0", snapshot, python, python, "3.12.1", []install.ResolvedPackage{{Name: "dependency", Version: "1", SHA256: strings.Repeat("b", 64)}}, "plan")
}

func sourceDirectoryFailure(failure, path string, mode os.FileMode, tx *runtimeTransaction) bool {
	oldSource := filepath.Join(tx.directory, "payload/source")
	oldVenv := filepath.Join(tx.directory, "venv")
	marker, _ := os.ReadFile(filepath.Join(path, "marker"))
	switch failure {
	case "park source":
		return path == oldSource && mode&0200 != 0
	case "park venv":
		return path == oldVenv && mode&0200 != 0
	case "activate source":
		return path == filepath.Join(tx.stage, "payload/source")
	case "activate venv":
		return path == filepath.Join(tx.stage, "venv")
	case "source moved mode":
		return path == oldSource && mode == 0750 && string(marker) == "new"
	case "venv moved mode":
		return path == oldVenv && mode == 0750 && string(marker) == "new"
	case "harden source":
		return path == filepath.Join(oldSource, "marker") && mode == 0440
	case "harden venv":
		return path == filepath.Join(oldVenv, "marker") && mode == 0440
	}
	return false
}

func TestSourcePairRestoresPayloadsRecordsAndDataAtEveryFailure(t *testing.T) {
	for _, failure := range []string{"park source", "park venv", "activate source", "activate venv", "source moved mode", "venv moved mode", "park version", "park manifest", "relocate", "runtime check", "help", "write version", "write manifest", "harden source", "harden venv", "finalize"} {
		t.Run(failure, func(t *testing.T) {
			_, snapshot, tx, python := sourceLifecycle(t)
			snapshot.AdditionalPackages = map[string]string{"next-addon": ">=2"}
			previous, err := fsroot.ReadInstallManifest(snapshot.Runtime)
			require.NoError(t, err)
			previous.AdditionalPackages = map[string]string{"previous-addon": ">=1"}
			require.NoError(t, fsroot.WriteInstallManifest(snapshot.Runtime, previous))
			require.NoError(t, os.Chmod(fsroot.ProviderManifestPath(snapshot.Runtime), 0440))
			before := map[string]map[string]savedFile{}
			for _, name := range append(snapshot.PayloadDirectories(), "data", "python") {
				before[name] = savedTree(t, filepath.Join(tx.directory, name))
			}
			records := map[string]savedFile{}
			for _, name := range []string{config.ProviderVersionName, config.ProviderManifestName} {
				path := filepath.Join(tx.directory, name)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				info, err := os.Stat(path)
				require.NoError(t, err)
				records[name] = savedFile{string(data), info.Mode()}
			}
			denied := errors.New("synthetic failure")
			injected := false
			tx.chmod = func(path string, mode os.FileMode) error {
				fail := sourceDirectoryFailure(failure, path, mode, tx)
				if fail && !injected {
					injected = true
					return denied
				}
				if !injected && (failure == "park version" || failure == "park manifest") {
					name := "version"
					if failure == "park manifest" {
						name = "install.json"
					}
					require.NoError(t, os.Mkdir(filepath.Join(tx.directory, ".previous", name), 0700))
					injected = true
				}
				return os.Chmod(path, mode)
			}
			main := filepath.Join(tx.stage, snapshot.WorkingDir, "main.py")
			switch failure {
			case "relocate":
				require.NoError(t, os.RemoveAll(filepath.Join(tx.stage, "venv", "bin")))
			case "runtime check":
				require.NoError(t, os.Remove(venvPython(filepath.Join(tx.stage, "venv"))))
				require.NoError(t, os.WriteFile(venvPython(filepath.Join(tx.stage, "venv")), []byte("#!/bin/sh\nexit 1\n"), 0700))
			case "help":
				require.NoError(t, os.WriteFile(main, []byte("raise RuntimeError('bad help')\n"), 0600))
			case "write version", "write manifest":
				name := "version"
				if failure == "write manifest" {
					name = "install.json.tmp"
				}
				script := fmt.Sprintf("import os, argparse\nos.mkdir(%q)\nargparse.ArgumentParser().parse_args()\n", filepath.Join(tx.directory, name))
				require.NoError(t, os.WriteFile(main, []byte(script), 0600))
			}
			err = activateSource(t, tx, snapshot, python)
			if failure == "finalize" {
				require.NoError(t, err)
				require.NoError(t, tx.rollback())
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), "rollback completed")
				if strings.Contains(failure, "mode") || strings.HasPrefix(failure, "harden") || strings.HasPrefix(failure, "activate") || strings.HasPrefix(failure, "park") {
					require.True(t, injected, "failure must reach its specified stage")
				}
			}
			for name, want := range before {
				require.Equal(t, want, savedTree(t, filepath.Join(tx.directory, name)), name)
			}
			for name, want := range records {
				path := filepath.Join(tx.directory, name)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.Equal(t, want, savedFile{string(data), info.Mode()}, name)
			}
			require.NoError(t, tx.rollback(), "abort is idempotent")
		})
	}
}

func TestSourceUpgradeAndUninstallPreserveDataAndPython(t *testing.T) {
	installer, snapshot, tx, python := sourceLifecycle(t)
	data := savedTree(t, filepath.Join(tx.directory, "data"))
	operatorPython := savedTree(t, filepath.Join(tx.directory, "python"))
	require.NoError(t, ensureSourceData(snapshot.DataRuntime, tx.directory, true))
	require.NoError(t, activateSource(t, tx, snapshot, python))
	require.True(t, installer.IsInstalled())
	version, err := installer.InstalledVersion()
	require.NoError(t, err)
	require.Equal(t, "2.0.0", version)
	for _, name := range snapshot.PayloadDirectories() {
		marker, err := os.ReadFile(filepath.Join(tx.directory, name, "marker"))
		require.NoError(t, err)
		require.Equal(t, "new", string(marker))
		info, err := os.Stat(filepath.Join(tx.directory, name))
		require.NoError(t, err)
		require.Zero(t, info.Mode().Perm()&0222)
	}
	require.DirExists(t, filepath.Join(tx.directory, ".previous", snapshot.WorkingDir), "backup survives until finalize/next activation")
	require.Equal(t, data, savedTree(t, filepath.Join(tx.directory, "data")))
	require.NoError(t, installer.Uninstall(t.Context()))
	for _, name := range append(snapshot.PayloadDirectories(), "version", "install.json", "install.json.tmp", ".managed", ".candidate", ".previous") {
		_, err := os.Lstat(filepath.Join(tx.directory, name))
		require.ErrorIs(t, err, os.ErrNotExist, name)
	}
	require.Equal(t, data, savedTree(t, filepath.Join(tx.directory, "data")))
	require.Equal(t, operatorPython, savedTree(t, filepath.Join(tx.directory, "python")))
	require.DirExists(t, tx.directory)
}

func TestSourceHandInstallAndDisposableRefuseBeforeWork(t *testing.T) {
	for _, payload := range []string{"source", "venv", "both"} {
		t.Run(payload, func(t *testing.T) {
			fsroot.SetProviderRootOverride(t.TempDir())
			t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
			snapshot := sourceSnapshot()
			directory := fsroot.ProviderDir(snapshot.Runtime)
			for _, name := range []string{"data", "python"} {
				require.NoError(t, os.MkdirAll(filepath.Join(directory, name), 0700))
				require.NoError(t, os.WriteFile(filepath.Join(directory, name, "keep"), []byte(name), 0600))
			}
			for _, name := range []string{"source", "venv"} {
				if payload == name || payload == "both" {
					require.NoError(t, os.Mkdir(filepath.Join(directory, name), 0700))
					require.NoError(t, os.WriteFile(filepath.Join(directory, name, "hand"), []byte("hand install"), 0600))
				}
			}
			before := savedTree(t, directory)
			installer := New(Config{Name: snapshot.Runtime, Recipe: func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }})
			for _, force := range []bool{false, true} {
				_, err := installer.InstallPlan(install.WithPlanOptions(t.Context(), install.PlanOptions{Force: force}), "")
				require.ErrorContains(t, err, "Operator installation found in "+directory+"; stop it and move source and venv aside before a managed install.")
			}
			_, err := installer.InstallPlan(install.WithPlanOptions(t.Context(), install.PlanOptions{Disposable: true}), "")
			require.ErrorContains(t, err, "disposable")
			require.Equal(t, before, savedTree(t, directory))
		})
	}
	fsroot.SetProviderRootOverride(t.TempDir())
	defer fsroot.SetProviderRootOverride("")
	snapshot := sourceSnapshot()
	installer := New(Config{Name: snapshot.Runtime, Recipe: func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }})
	_, err := installer.InstallPlan(install.WithPlanOptions(context.Background(), install.PlanOptions{Disposable: true}), "")
	require.ErrorContains(t, err, "disposable")
	_, err = os.Stat(fsroot.ProviderDir(snapshot.Runtime))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSourceLifecycleRefusesSymlinkAliasesAndChangedOwnership(t *testing.T) {
	for _, variant := range []string{"owned ancestor", "data root alias", "model alias", "changed owned path"} {
		t.Run(variant, func(t *testing.T) {
			installer, snapshot, tx, _ := sourceLifecycle(t)
			outside := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(outside, "source"), 0750))
			require.NoError(t, os.WriteFile(filepath.Join(outside, "source", "marker"), []byte("outside data"), 0640))
			switch variant {
			case "owned ancestor":
				require.NoError(t, removeManagedTree(filepath.Join(tx.directory, "payload")))
				require.NoError(t, os.Symlink(outside, filepath.Join(tx.directory, "payload")))
			case "data root alias":
				require.NoError(t, os.RemoveAll(filepath.Join(tx.directory, "data")))
				require.NoError(t, os.Symlink("payload/source", filepath.Join(tx.directory, "data")))
			case "model alias":
				require.NoError(t, os.RemoveAll(filepath.Join(tx.directory, "data", "models")))
				require.NoError(t, os.Symlink("../..//venv", filepath.Join(tx.directory, "data", "models")))
			case "changed owned path":
				snapshot.WorkingDir = "operator"
				require.NoError(t, os.Mkdir(filepath.Join(tx.directory, snapshot.WorkingDir), 0750))
				require.NoError(t, os.WriteFile(filepath.Join(tx.directory, snapshot.WorkingDir, "marker"), []byte("operator payload"), 0640))
				installer.cfg.Recipe = func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }
			}
			before := savedTree(t, tx.directory)
			external := savedTree(t, outside)
			_, err := installer.InstallPlan(install.WithPlanOptions(t.Context(), install.PlanOptions{Force: true}), "")
			require.Error(t, err)
			if variant == "changed owned path" {
				require.ErrorIs(t, err, errSourceDirectoryChanged)
				require.Equal(t, before, savedTree(t, tx.directory), "activation cannot replace an unowned destination")
				operator := savedTree(t, filepath.Join(tx.directory, snapshot.WorkingDir))
				data := savedTree(t, filepath.Join(tx.directory, "data"))
				require.NoError(t, installer.Uninstall(t.Context()))
				require.Equal(t, operator, savedTree(t, filepath.Join(tx.directory, snapshot.WorkingDir)))
				require.Equal(t, data, savedTree(t, filepath.Join(tx.directory, "data")))
				require.NoDirExists(t, filepath.Join(tx.directory, "payload/source"))
				require.NoDirExists(t, filepath.Join(tx.directory, "venv"))
				return
			}
			require.Error(t, installer.Uninstall(t.Context()))
			require.Equal(t, before, savedTree(t, tx.directory))
			require.Equal(t, external, savedTree(t, outside))
			if variant == "owned ancestor" {
				require.NoError(t, os.MkdirAll(filepath.Join(tx.directory, ".previous", "payload"), 0700))
				moved, err := tx.moveDirectory(filepath.Join(tx.directory, "payload", "source"), filepath.Join(tx.directory, ".previous", "payload", "source"))
				require.Error(t, err)
				require.False(t, moved)
				require.Equal(t, external, savedTree(t, outside))
			}
		})
	}
}

func TestSourceIncompleteManagedRecordAllowsOnlyOwnedForceRepair(t *testing.T) {
	for _, defect := range []string{"no identity", "no inventory", "wrong fingerprint", "corrupt JSON", "missing ownership", "data ownership"} {
		t.Run(defect, func(t *testing.T) {
			installer, snapshot, tx, python := sourceLifecycle(t)
			manifest, err := fsroot.ReadInstallManifest(snapshot.Runtime)
			require.NoError(t, err)
			switch defect {
			case "no identity":
				manifest.PlanID = ""
			case "no inventory":
				manifest.Inventory = nil
			case "wrong fingerprint":
				manifest.RecipeFingerprint = "wrong"
			case "missing ownership":
				manifest.WorkingDir = ""
			case "data ownership":
				manifest.WorkingDir = "data"
			}
			require.NoError(t, fsroot.WriteInstallManifest(snapshot.Runtime, manifest))
			if defect == "corrupt JSON" {
				require.NoError(t, os.WriteFile(fsroot.ProviderManifestPath(snapshot.Runtime), []byte("{broken"), 0600))
			}
			before := savedTree(t, tx.directory)
			_, err = installer.InstallPlan(t.Context(), "")
			if defect == "data ownership" {
				require.ErrorIs(t, err, errSourceDirectoryChanged)
				_, err = installer.InstallPlan(install.WithPlanOptions(t.Context(), install.PlanOptions{Force: true}), "")
				require.ErrorIs(t, err, errSourceDirectoryChanged)
				require.ErrorContains(t, installer.Uninstall(t.Context()), "overlaps reserved path")
				require.Equal(t, before, savedTree(t, tx.directory))
				return
			}
			require.ErrorContains(t, err, "managed installation record is incomplete; reinstall with force")
			require.NotContains(t, err.Error(), "Operator installation found")
			_, err = installer.InstallPlan(install.WithPlanOptions(t.Context(), install.PlanOptions{Force: true}), "")
			if defect == "corrupt JSON" || defect == "missing ownership" || defect == "data ownership" {
				require.ErrorContains(t, err, "managed installation record is incomplete")
				require.Error(t, installer.Uninstall(t.Context()), "invalid ownership cannot authorize deletion")
				require.Equal(t, before, savedTree(t, tx.directory))
				return
			}
			require.NoError(t, err, "safe recorded ownership permits force even when completion is incomplete")
			require.Equal(t, before, savedTree(t, tx.directory), "plan construction does not change payloads or records")
			require.NoError(t, activateSource(t, tx, snapshot, python))
			require.True(t, installer.IsInstalled())
		})
	}
}

func TestSourceWorkingDirectoryDriftRequiresUninstall(t *testing.T) {
	installer, snapshot, tx, _ := sourceLifecycle(t)
	old := snapshot.WorkingDir
	data := savedTree(t, filepath.Join(tx.directory, "data"))
	operatorPython := savedTree(t, filepath.Join(tx.directory, "python"))
	snapshot.WorkingDir = "operator"
	require.NoError(t, os.Mkdir(filepath.Join(tx.directory, snapshot.WorkingDir), 0750))
	require.NoError(t, os.WriteFile(filepath.Join(tx.directory, snapshot.WorkingDir, "keep"), []byte("operator data"), 0600))
	operator := savedTree(t, filepath.Join(tx.directory, snapshot.WorkingDir))
	installer.cfg.Recipe = func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }
	before := savedTree(t, tx.directory)
	for _, force := range []bool{false, true} {
		// A request action cannot turn installation into uninstall admission.
		ctx := install.WithPlanOptions(t.Context(), install.PlanOptions{Force: force, Action: "uninstall"})
		_, err := installer.InstallPlan(ctx, "")
		require.ErrorIs(t, err, errSourceDirectoryChanged)
		_, err = installer.UpgradePlan(ctx, "")
		require.ErrorIs(t, err, errSourceDirectoryChanged)
	}
	require.Equal(t, before, savedTree(t, tx.directory), "refusal must precede any install work")
	require.NoError(t, installer.Uninstall(t.Context()))
	require.NoDirExists(t, filepath.Join(tx.directory, old))
	require.NoDirExists(t, filepath.Join(tx.directory, "venv"))
	require.Equal(t, operator, savedTree(t, filepath.Join(tx.directory, snapshot.WorkingDir)))
	require.Equal(t, data, savedTree(t, filepath.Join(tx.directory, "data")))
	require.Equal(t, operatorPython, savedTree(t, filepath.Join(tx.directory, "python")))
}

func TestSourceGuardRechecksHandInstallBeforeCandidateWork(t *testing.T) {
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	snapshot := sourceSnapshot()
	installer := New(Config{Name: snapshot.Runtime, Recipe: func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }})
	plan, err := installer.InstallPlan(t.Context(), "")
	require.NoError(t, err)
	directory := fsroot.ProviderDir(snapshot.Runtime)
	require.NoError(t, os.MkdirAll(filepath.Join(directory, "venv"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "venv", "marker"), []byte("late hand install"), 0600))
	before := savedTree(t, directory)
	require.ErrorContains(t, plan.Steps[0].PostExec(t.Context()), "Operator installation found")
	require.Equal(t, before, savedTree(t, directory), "execution guard must run before data/candidate creation")
}

func TestSourcePartialCandidateRetryPreservesLiveInstall(t *testing.T) {
	installer, snapshot, tx, _ := sourceLifecycle(t)
	before := map[string]map[string]savedFile{}
	for _, name := range append(snapshot.PayloadDirectories(), "data", "python") {
		before[name] = savedTree(t, filepath.Join(tx.directory, name))
	}
	require.NoError(t, os.WriteFile(filepath.Join(tx.stage, "plan_id"), []byte("plan"), 0600))
	require.NoError(t, installer.checkSourceInstall(&snapshot, install.PlanOptions{}))
	require.NoError(t, prepareCandidate(tx.stage))
	entries, err := os.ReadDir(tx.stage)
	require.NoError(t, err)
	require.Empty(t, entries)
	for name, want := range before {
		require.Equal(t, want, savedTree(t, filepath.Join(tx.directory, name)))
	}
	require.True(t, installer.IsInstalled())
	version, err := installer.InstalledVersion()
	require.NoError(t, err)
	require.Equal(t, "1.0.0", version)
}

func TestSourcePairHonorsDisabledHardeningPolicy(t *testing.T) {
	_, snapshot, tx, python := sourceLifecycle(t)
	fsroot.SetHardenVenvDefault(false)
	require.NoError(t, activateSource(t, tx, snapshot, python))
	for _, name := range snapshot.PayloadDirectories() {
		info, err := os.Stat(filepath.Join(tx.directory, name, "marker"))
		require.NoError(t, err)
		require.NotZero(t, info.Mode().Perm()&0200)
	}
}
