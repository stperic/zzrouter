package pythonvenv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/interpreter"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachedSourceRootsUseVerifiedArchiveAndExistingResolver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable Python fixture uses a POSIX shebang")
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	w := tar.NewWriter(gz)
	for name, body := range map[string]string{"release/requirements.txt": "Engine_Dep>=1\n", "release/main.py": "import argparse\nargparse.ArgumentParser().parse_args()\n"} {
		require.NoError(t, w.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := w.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	require.NoError(t, gz.Close())
	snapshot := sourceSnapshot()
	snapshot.Recipe.Source.SHA256 = fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))
	i := New(Config{Name: snapshot.Runtime})
	directory := filepath.Join(t.TempDir(), "absent", "provider")
	cache := filepath.Join(directory, config.SubdirCandidate, snapshot.Recipe.Source.SHA256)
	require.NoError(t, os.MkdirAll(cache, 0700))
	artifact := filepath.Join(cache, filepath.Base(snapshot.Recipe.Source.URL))
	require.NoError(t, os.WriteFile(artifact, archive.Bytes(), 0600))
	roots, err := i.sourceRoots(t.Context(), snapshot, directory)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"engine-dep": ">=1"}, roots)
	// This synthetic executable returns a pip report without network or dependency installation.
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	fake := filepath.Join(t.TempDir(), "python-fixture")
	script := "#!" + python + "\n" + `import json, os, pathlib, shutil, sys
args = sys.argv[1:]
if args[:2] == ["-m", "venv"]:
    target = pathlib.Path(args[2]) / "bin" / "python"
    target.parent.mkdir(parents=True)
    shutil.copyfile(__file__, target)
    target.chmod(0o700)
else:
    assert "--dry-run" in args and "--ignore-installed" in args
    assert "--only-binary=:all:" in args and "engine-dep>=1" in args
    report = {"version": "1", "install": [{"metadata": {"name": "engine_dep", "version": "1.5"}, "requested": True,
        "download_info": {"url": "https://files.pythonhosted.org/engine-dep.whl", "archive_info": {"hashes": {"sha256": "b" * 64}}}}]}
    pathlib.Path(args[args.index("--report") + 1]).write_text(json.dumps(report))
`
	require.NoError(t, os.WriteFile(fake, []byte(script), 0700))
	inventory, err := resolvePackages(t.Context(), directory, fake, []string{}, []string{"install", "--index-url", "https://pypi.org/simple", "--only-binary=:all:", "engine-dep>=1"}, roots)
	require.NoError(t, err)
	require.Equal(t, "engine-dep==1.5 --hash=sha256:"+strings.Repeat("b", 64)+"\n", install.InventoryRequirements(inventory))
	children, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, children, 1, "only the pinned archive cache survives preflight")
	require.Equal(t, config.SubdirCandidate, children[0].Name())
	stage := filepath.Join(directory, config.SubdirCandidate, "plan-id")
	require.NoError(t, prepareCandidate(stage))
	_, err = install.ReadSource(t.Context(), artifact, stage, snapshot.WorkingDir, *snapshot.Recipe.Source)
	require.NoError(t, err, "candidate preparation must preserve the cached archive")
	body, err := os.ReadFile(filepath.Join(stage, snapshot.WorkingDir, "requirements.txt"))
	require.NoError(t, err)
	require.Equal(t, "Engine_Dep>=1\n", string(body))
	// Cached bytes are reverified before every use, with no existence-based trust.
	require.NoError(t, os.WriteFile(artifact, []byte("corrupt archive"), 0600))
	_, err = i.sourceRoots(t.Context(), snapshot, directory)
	require.ErrorContains(t, err, "checksum mismatch")
	_, err = install.ReadSource(t.Context(), artifact, stage, "new-source", *snapshot.Recipe.Source)
	require.ErrorContains(t, err, "checksum mismatch")
	_, err = os.Lstat(filepath.Join(stage, "new-source"))
	require.True(t, os.IsNotExist(err))
}

func sourceSnapshot() install.RecipeSnapshot {
	primary, timeout, binary := "https://pypi.org/simple", "30m", true
	imports, checks := []string{"json"}, []string{"pip_check", "imports"}
	r := config.InstallRecipe{Source: &config.InstallSource{Version: "1.0.0", URL: "https://github.com/example/engine/archive/v1.0.0.tar.gz", SHA256: strings.Repeat("a", 64), StripComponents: 1, Requirements: "requirements.txt", Entrypoint: "main.py"}, Indexes: &config.InstallIndexes{Primary: &primary}, Verify: &config.InstallVerify{Imports: &imports, Checks: &checks}, OnlyBinary: &binary, Timeout: &timeout}
	return install.RecipeSnapshot{ResolvedInstall: config.ResolvedInstall{Recipe: r}, Provider: "source-engine", Runtime: "source-engine", Fingerprint: install.Fingerprint(r), WorkingDir: "source", Recheck: func(ctx context.Context) error { return ctx.Err() }}
}

func TestSourceVersionUsesOnlyPinAndPreservesRequirementConstraints(t *testing.T) {
	snapshot := sourceSnapshot()
	for _, action := range []string{"install", "upgrade"} {
		for _, requested := range []string{"", "1.0.0", "2.0.0"} {
			version, err := resolveRecipeVersion(t.Context(), snapshot, requested, action, func(context.Context, string, string) (upstream.Release, error) {
				t.Fatal("source recipes cannot perform latest lookups")
				return upstream.Release{}, nil
			})
			if requested == "2.0.0" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "1.0.0", version)
			}
		}
	}
	roots := map[string]string{"torch": ">=2.8", "torchvision": ""}
	constraint, build := "<3", "cuda"
	snapshot.Recipe.Companions = map[string]config.InstallCompanion{"torch": {Constraint: &constraint, Build: &build}, "torchvision": {Constraint: new(string), Build: &build}}
	addCompanionRoots(snapshot, roots)
	assert.Equal(t, ">=2.8,<3", roots["torch"])
	assert.Equal(t, "", roots["torchvision"])
	constraint = ""
	addCompanionRoots(snapshot, roots)
	assert.Equal(t, ">=2.8,<3", roots["torch"], "an empty companion constraint cannot discard release constraints")
}

func TestSourceCandidatePlanInstallsOnlyAcceptedInventory(t *testing.T) {
	snapshot := sourceSnapshot()
	i := New(Config{Name: snapshot.Runtime, Recipe: func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }})
	plan := &install.Plan{Recipe: &snapshot, InstallDir: t.TempDir(), PlanID: "bound", Version: "1.0.0", Inventory: []install.ResolvedPackage{{Name: "engine-dep", Version: "2.0", SHA256: strings.Repeat("b", 64)}}}
	checks := schema.RuntimeChecks{Imports: []string{"json"}, Checks: []string{"pip_check", "imports"}, Kernels: "unknown"}
	choice := &interpreter.Choice{Path: "/selected/python", Version: "3.12.1"}
	flags := []string{"install", "--index-url", "https://pypi.org/simple", "--only-binary=:all:", "engine-dep"}
	require.NoError(t, i.configureRecipePlan(plan, choice, nil, checks, flags, []string{"engine-dep"}, time.Minute, install.PlanOptions{}))
	for _, flag := range []string{"--require-hashes", "--no-deps", "--only-binary=:all:", "requirements.txt"} {
		assert.Contains(t, plan.Steps[2].Command, flag)
	}
	assert.NotContains(t, plan.Steps[2].Command, "engine-dep", "roots must be replaced by the hashed inventory file")
	require.NotNil(t, plan.Steps[3].PostExec, "source smoke belongs to the existing candidate verify step")
	plan.Inventory = nil
	require.NoError(t, i.configureRecipePlan(plan, choice, nil, checks, flags, []string{"engine-dep"}, time.Minute, install.PlanOptions{}))
	assert.Contains(t, plan.Steps[2].Command, "--require-hashes", "even an unprepared source plan cannot expose unpinned install commands")
}

func TestSourceCompletionNeverAdoptsVersionFilesOrBackfills(t *testing.T) {
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	snapshot := sourceSnapshot()
	i := New(Config{Name: snapshot.Runtime, Recipe: func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }})
	require.NoError(t, os.MkdirAll(fsroot.ProviderDir(snapshot.Runtime), 0700))
	require.NoError(t, os.WriteFile(fsroot.ProviderVersionFile(snapshot.Runtime), []byte("99.0"), 0600))
	assert.False(t, i.IsInstalled())
	_, err := i.InstalledVersion()
	require.Error(t, err)
	require.Error(t, i.HealthCheck(t.Context(), nil))
	_, err = os.Lstat(fsroot.ProviderManifestPath(snapshot.Runtime))
	require.True(t, os.IsNotExist(err), "health cannot backfill a source record")
	complete := fsroot.InstallManifest{PlanID: "complete", Version: "1.0.0", Recipe: &snapshot.Recipe, RecipeFingerprint: snapshot.Fingerprint, Interpreter: &fsroot.ManifestInterpreter{Path: "/recorded/python", Version: "3.12.1"}, Inventory: []fsroot.ManifestPackage{{Name: "dependency", Version: "1.0", SHA256: strings.Repeat("b", 64)}}}
	for name, mutate := range map[string]func(*fsroot.InstallManifest){
		"no identity":    func(m *fsroot.InstallManifest) { m.PlanID = "" },
		"no interpreter": func(m *fsroot.InstallManifest) { m.Interpreter = nil },
		"no inventory":   func(m *fsroot.InstallManifest) { m.Inventory = nil },
		"wrong version":  func(m *fsroot.InstallManifest) { m.Version = "99.0" },
		"wrong recipe":   func(m *fsroot.InstallManifest) { m.RecipeFingerprint = "changed" },
		"no recipe":      func(m *fsroot.InstallManifest) { m.Recipe = nil },
	} {
		t.Run(name, func(t *testing.T) {
			m := complete
			mutate(&m)
			require.NoError(t, fsroot.WriteInstallManifest(snapshot.Runtime, &m))
			assert.False(t, i.IsInstalled())
			_, err := i.InstalledVersion()
			require.Error(t, err)
		})
	}
	require.NoError(t, os.WriteFile(fsroot.ProviderManifestPath(snapshot.Runtime), []byte("{broken"), 0600))
	assert.False(t, i.IsInstalled())
	_, err = i.InstalledVersion()
	require.Error(t, err)
	require.NoError(t, fsroot.WriteInstallManifest(snapshot.Runtime, &complete))
	// A newer desired pin must still report the old completed release.
	snapshot.Recipe.Source = &config.InstallSource{Version: "2.0.0"}
	assert.True(t, i.IsInstalled(), "completion does not probe the interpreter or stray version file")
	version, err := i.InstalledVersion()
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", version)
}

func TestSourceSmokePreservesSiblingImportsAndIsBounded(t *testing.T) {
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	directory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(directory, "sibling.py"), []byte("message = 'entrypoint help'\n"), 0600))
	entry := filepath.Join(directory, "main.py")
	require.NoError(t, os.WriteFile(entry, []byte("import argparse, sibling, os\nassert os.path.isfile('sibling.py')\nargparse.ArgumentParser(description=sibling.message).parse_args()\n"), 0600))
	require.NoError(t, sourceSmoke(t.Context(), python, directory, "main.py", []string{}))
	_, err = os.Stat(filepath.Join(directory, "__pycache__"))
	require.True(t, os.IsNotExist(err), "smoke cannot create source bytecode caches")
	require.NoError(t, os.WriteFile(entry, []byte("raise RuntimeError('broken entrypoint')\n"), 0600))
	require.ErrorContains(t, sourceSmoke(t.Context(), python, directory, "main.py", []string{}), "broken entrypoint")
	// Bounded tail, then a hanging process exercises timeout/cancellation drainage.
	require.NoError(t, os.WriteFile(entry, []byte("import sys, time\nsys.stdout.write('x' * (1 << 20)); sys.stdout.flush()\ntime.sleep(60)\n"), 0600))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = sourceSmoke(ctx, python, directory, "main.py", []string{})
	require.Error(t, err)
	assert.Less(t, len(err.Error()), 70<<10)
	// Plan JSON remains unchanged: the captured layout is internal only.
	data, err := json.Marshal(sourceSnapshot())
	require.NoError(t, err)
	assert.NotContains(t, string(data), "working_dir")
}

func TestSourceCompletionPropagatesRecipeResolutionErrors(t *testing.T) {
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	snapshot := sourceSnapshot()
	require.NoError(t, os.MkdirAll(fsroot.ProviderDir(snapshot.Runtime), 0700))
	require.NoError(t, os.WriteFile(fsroot.ProviderVersionFile(snapshot.Runtime), []byte("99.0"), 0600))
	i := New(Config{Name: snapshot.Runtime, Recipe: func(string, string) (install.RecipeSnapshot, error) {
		return install.RecipeSnapshot{}, install.ErrInstallPolicy
	}})
	assert.False(t, i.IsInstalled())
	_, err := i.InstalledVersion()
	require.ErrorIs(t, err, install.ErrInstallPolicy)
}

func TestPipCompletionPreservesVersionMarkerDespiteRecipeResolutionError(t *testing.T) {
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	primary, binary, timeout, name, startup := "https://pypi.org/simple", true, "1m", "pip", "pip"
	imports, checks := []string{"pip"}, []string{"pip_check", "imports"}
	recipe := config.InstallRecipe{Package: &name, StartupImport: &startup, Indexes: &config.InstallIndexes{Primary: &primary}, OnlyBinary: &binary, Timeout: &timeout, Verify: &config.InstallVerify{Imports: &imports, Checks: &checks}}
	i := New(Config{Name: "pip-engine", Recipe: func(string, string) (install.RecipeSnapshot, error) {
		return install.RecipeSnapshot{}, install.ErrInstallPolicy
	}})
	require.NoError(t, os.MkdirAll(fsroot.ProviderDir(i.cfg.Name), 0700))
	require.NoError(t, os.WriteFile(fsroot.ProviderVersionFile(i.cfg.Name), []byte("1.0"), 0600))
	require.NoError(t, fsroot.WriteInstallManifest(i.cfg.Name, &fsroot.InstallManifest{Version: "1.0", Recipe: &recipe}))
	assert.True(t, i.IsInstalled(), "pip completion still follows its version marker")
	_, source, err := i.sourceCompletion()
	require.NoError(t, err)
	assert.False(t, source)
	// A successfully resolved source recipe cannot adopt a former pip completion.
	snapshot := sourceSnapshot()
	i.cfg.Recipe = func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }
	assert.False(t, i.IsInstalled())
	_, err = i.InstalledVersion()
	require.Error(t, err)
}

func TestSourcePrerequisitesUseThePlanInterpreter(t *testing.T) {
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	i := New(Config{Name: "source-engine"})
	i.chosen = &interpreter.Choice{Path: filepath.Join(t.TempDir(), "deleted-python"), Version: "3.12.1"}
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	report := i.preflight(t.Context(), nil, &interpreter.Choice{Path: python, Version: "3.12.1"}, nil)
	require.True(t, report.AllOK, "%v", report.Results)
	require.Equal(t, filepath.Base(i.chosen.Path), "deleted-python", "checking the plan must not mutate the installer's interpreter cache")
}
