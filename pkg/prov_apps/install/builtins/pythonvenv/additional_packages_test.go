package pythonvenv

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/interpreter"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

// Wheels and archive/report provenance are synthetic; pip installs the actual wheel bytes offline.
func extensionWheel(t *testing.T, directory, name, dependency string) install.ResolvedPackage {
	t.Helper()
	module := strings.ReplaceAll(name, "-", "_")
	metadata := "Metadata-Version: 2.1\nName: " + name + "\nVersion: 1.0\n"
	if dependency != "" {
		metadata += "Requires-Dist: " + dependency + "\n"
	}
	info := module + "-1.0.dist-info/"
	files := map[string]string{
		module + "/__init__.py": "VALUE = 'approved wheel'\n",
		info + "METADATA":       metadata,
		info + "WHEEL":          "Wheel-Version: 1.0\nGenerator: zzrouter-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
	}
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	var record strings.Builder
	for path, body := range files {
		f, err := w.Create(path)
		require.NoError(t, err)
		_, err = f.Write([]byte(body))
		require.NoError(t, err)
		hash := sha256.Sum256([]byte(body))
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", path, base64.RawURLEncoding.EncodeToString(hash[:]), len(body))
	}
	f, err := w.Create(info + "RECORD")
	require.NoError(t, err)
	_, err = f.Write([]byte(record.String() + info + "RECORD,,\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	filename := module + "-1.0-py3-none-any.whl"
	require.NoError(t, os.WriteFile(filepath.Join(directory, filename), archive.Bytes(), 0600))
	return install.ResolvedPackage{Name: name, Version: "1.0", Requested: true, ArtifactURL: "https://files.pythonhosted.org/packages/" + filename, SHA256: fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))}
}

func extensionArchive(t *testing.T, snapshot *install.RecipeSnapshot, directory string) {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	w := tar.NewWriter(gz)
	for path, body := range map[string]string{"release/requirements.txt": "zzrouter-test-base>=1,<2\n", "release/main.py": "import argparse\nargparse.ArgumentParser().parse_args()\n"} {
		require.NoError(t, w.WriteHeader(&tar.Header{Name: path, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(body))}))
		_, err := w.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	require.NoError(t, gz.Close())
	snapshot.Recipe.Source.SHA256 = fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))
	snapshot.Fingerprint = install.Fingerprint(snapshot.Recipe)
	cache := filepath.Join(directory, config.SubdirCandidate, snapshot.Recipe.Source.SHA256)
	require.NoError(t, os.MkdirAll(cache, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(cache, filepath.Base(snapshot.Recipe.Source.URL)), archive.Bytes(), 0600))
}

func extensionReport(t *testing.T, inventory []install.ResolvedPackage, roots map[string]string) []install.ResolvedPackage {
	t.Helper()
	entries := []map[string]any{}
	for _, p := range inventory {
		entries = append(entries, map[string]any{"metadata": map[string]string{"name": p.Name, "version": p.Version}, "requested": true, "is_direct": false, "download_info": map[string]any{"url": p.ArtifactURL, "archive_info": map[string]any{"hashes": map[string]string{"sha256": p.SHA256}}}})
	}
	data, err := json.Marshal(map[string]any{"version": "1", "install": entries})
	require.NoError(t, err)
	resolved, err := install.ParsePipReport(data, roots)
	require.NoError(t, err)
	return resolved
}

func extensionLive(t *testing.T, snapshot install.RecipeSnapshot, directory string) map[string]map[string]savedFile {
	t.Helper()
	before := map[string]map[string]savedFile{}
	for _, name := range append(snapshot.PayloadDirectories(), "data", "python") {
		before[name] = savedTree(t, filepath.Join(directory, name))
	}
	return before
}

func TestApprovedAdditionalPackageIsHashInstalledIntoVenv(t *testing.T) {
	for _, outcome := range []string{"approved", "unapproved", "wrong hash"} {
		t.Run(outcome, func(t *testing.T) {
			i, snapshot, tx, python := sourceLifecycle(t)
			snapshot.Authority = "release"
			snapshot.AdditionalPackages = map[string]string{"zzrouter-test-extra": "==1.0"}
			grant := install.RuntimePolicy{AdditionalPackages: map[string]string{"zzrouter-test-extra": "==1.0"}}
			if outcome == "unapproved" {
				grant.AdditionalPackages = map[string]string{}
			}
			imports := []string{"json", "zzrouter_test_extra"}
			snapshot.Recipe.Verify.Imports = &imports
			extensionArchive(t, &snapshot, tx.directory)
			snapshot.Recheck = func(ctx context.Context) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return grant.Authorize(snapshot, nil)
			}
			i.cfg.Recipe = func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }
			roots, err := i.sourceRoots(t.Context(), snapshot, tx.directory)
			require.NoError(t, err)
			addCompanionRoots(snapshot, roots)
			wheels := t.TempDir()
			base := extensionWheel(t, wheels, "zzrouter-test-base", "")
			extra := extensionWheel(t, wheels, "zzrouter-test-extra", "zzrouter-test-base<2")
			inventory := extensionReport(t, []install.ResolvedPackage{base, extra}, roots)
			if outcome == "wrong hash" {
				inventory[1].SHA256 = strings.Repeat("a", 64)
			}
			plan := &install.Plan{Recipe: &snapshot, InstallDir: tx.directory, Version: snapshot.Recipe.Source.Version, PlanID: "approved-plan", Inventory: inventory}
			flags := []string{"--disable-pip-version-check", "install", "--no-input", "--no-index", "--find-links", wheels, "--only-binary=:all:"}
			requirements := rootRequirements(roots)
			flags = append(flags, requirements...)
			checks := snapshot.RuntimeChecks(schema.RuntimeChecks{Kernels: "unknown"})
			choice := &interpreter.Choice{Path: python, Realpath: python, Version: "3.14.0"}
			require.NoError(t, i.configureRecipePlan(plan, choice, nil, checks, flags, requirements, time.Minute, install.PlanOptions{Force: true}))
			require.Contains(t, plan.Steps[2].Command, "--require-hashes")
			require.Contains(t, plan.Steps[2].Command, "--no-deps")
			require.Contains(t, plan.Steps[2].Command, "--only-binary=:all:")
			// Transport is noninteractive; the hashed pip command and all candidate/activation hooks are unchanged.
			plan.Steps[2].UsePTY = false
			before := extensionLive(t, snapshot, tx.directory)
			oldManifest, err := os.ReadFile(fsroot.ProviderManifestPath(snapshot.Runtime))
			require.NoError(t, err)
			marker := filepath.Join(tx.directory, "install-py-ran")
			script := filepath.Join(tx.directory, "data", "custom_nodes", "install.py")
			require.NoError(t, os.WriteFile(script, []byte(fmt.Sprintf("from pathlib import Path\nPath(%q).touch()\n", marker)), 0600))
			before["data"] = savedTree(t, filepath.Join(tx.directory, "data"))
			var failed *install.StepResult
			for _, step := range plan.Steps {
				result := plan.ExecuteStep(t.Context(), step.Number)
				if !result.Passed {
					failed = result
					break
				}
			}
			if outcome != "approved" {
				require.NotNil(t, failed)
				if outcome == "unapproved" {
					require.Contains(t, failed.Message, "unapproved")
				} else {
					require.Contains(t, failed.Message, "HASHES")
				}
				require.NoError(t, plan.Rollback())
				require.Equal(t, before, extensionLive(t, snapshot, tx.directory))
				after, err := os.ReadFile(fsroot.ProviderManifestPath(snapshot.Runtime))
				require.NoError(t, err)
				require.Equal(t, oldManifest, after)
			} else {
				require.Nil(t, failed)
				require.NoError(t, runPython(t.Context(), fsroot.ProviderVenvPython(snapshot.Runtime), "", nil, "-I", "-c", "import zzrouter_test_extra; assert zzrouter_test_extra.VALUE == 'approved wheel'"))
				manifest, err := fsroot.ReadInstallManifest(snapshot.Runtime)
				require.NoError(t, err)
				require.Equal(t, snapshot.AdditionalPackages, manifest.AdditionalPackages)
				require.Equal(t, inventory, manifest.Inventory)
				require.True(t, i.IsInstalled())
				for _, name := range []string{"data", "python"} {
					require.Equal(t, before[name], savedTree(t, filepath.Join(tx.directory, name)))
				}
			}
			require.NoFileExists(t, marker, "node install.py must never execute")
		})
	}
}

func TestAdditionalPackageConflictAndNoWheelPreserveLiveInstall(t *testing.T) {
	for _, outcome := range []string{"conflict", "no wheel"} {
		t.Run(outcome, func(t *testing.T) {
			i, snapshot, tx, python := sourceLifecycle(t)
			snapshot.AdditionalPackages = map[string]string{"zzrouter-test-extra": "==1.0"}
			extensionArchive(t, &snapshot, tx.directory)
			roots, err := i.sourceRoots(t.Context(), snapshot, tx.directory)
			require.NoError(t, err)
			addCompanionRoots(snapshot, roots)
			wheels := t.TempDir()
			extensionWheel(t, wheels, "zzrouter-test-base", "")
			if outcome == "conflict" {
				extensionWheel(t, wheels, "zzrouter-test-extra", "zzrouter-test-base>=2")
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(wheels, "zzrouter-test-extra-1.0.tar.gz"), []byte("must not build this source artifact"), 0600))
			}
			before := extensionLive(t, snapshot, tx.directory)
			oldManifest, err := os.ReadFile(fsroot.ProviderManifestPath(snapshot.Runtime))
			require.NoError(t, err)
			flags := []string{"--disable-pip-version-check", "install", "--no-input", "--no-index", "--find-links", wheels, "--only-binary=:all:"}
			_, err = resolvePackages(t.Context(), tx.directory, python, []string{}, append(flags, rootRequirements(roots)...), roots)
			require.Error(t, err)
			require.True(t, strings.HasPrefix(err.Error(), "ERROR:"), "pip diagnosis must lead the shared resolver error: %s", err)
			if outcome == "conflict" {
				require.ErrorContains(t, err, "conflicting dependencies")
			} else {
				require.ErrorContains(t, err, "No matching distribution found")
			}
			require.Equal(t, before, extensionLive(t, snapshot, tx.directory))
			after, err := os.ReadFile(fsroot.ProviderManifestPath(snapshot.Runtime))
			require.NoError(t, err)
			require.Equal(t, oldManifest, after)
		})
	}
}

func TestAdditionalConstraintsIntersectReleaseAndCompanions(t *testing.T) {
	snapshot := sourceSnapshot()
	constraint, build := "<3", "cuda"
	snapshot.Recipe.Companions = map[string]config.InstallCompanion{"torch": {Constraint: &constraint, Build: &build}}
	snapshot.AdditionalPackages = map[string]string{"torch": ">=2.9", "transformers": "", "torchaudio": ""}
	roots := map[string]string{"torch": ">=2.8", "transformers": "<6"}
	addCompanionRoots(snapshot, roots)
	require.Equal(t, ">=2.8,<3,>=2.9", roots["torch"])
	require.Equal(t, "<6", roots["transformers"], "empty addition must not erase a release bound")
	require.Contains(t, snapshot.RuntimeChecks(schema.RuntimeChecks{}).Packages, "torchaudio")
	for n := range install.AdditionalPackagesLimit - len(snapshot.AdditionalPackages) {
		snapshot.AdditionalPackages[fmt.Sprintf("extension-%d", n)] = ""
	}
	require.LessOrEqual(t, len(snapshot.RuntimeChecks(schema.RuntimeChecks{}).Packages), 32)
}

func TestAdditionalPackagesCompletionRefusesMalformedSelection(t *testing.T) {
	i, snapshot, _, _ := sourceLifecycle(t)
	manifest, err := fsroot.ReadInstallManifest(snapshot.Runtime)
	require.NoError(t, err)
	manifest.AdditionalPackages = map[string]string{"extension": "https://example.org/unapproved.whl"}
	require.NoError(t, fsroot.WriteInstallManifest(snapshot.Runtime, manifest))
	_, err = i.InstalledVersion()
	require.ErrorIs(t, err, errSourceRecordIncomplete)
	require.NoError(t, i.checkSourceInstall(&snapshot, install.PlanOptions{Force: true}), "owned force repair remains possible")
}
