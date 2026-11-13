package pythonvenv

import (
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

func TestUnknownComponentRefusedBeforeInterpreterOrSourceIO(t *testing.T) {
	snapshot := sourceSnapshot()
	snapshot.Requirements = &config.AppRequirements{Python: &config.PythonRequirement{Min: "99.0", Max: "100.0"}}
	i := &Installer{}
	i.cfg.Name = snapshot.Runtime
	i.cfg.Recipe = func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }
	requested := []string{"unapproved"}
	_, err := i.recipePlan(install.WithPlanOptions(t.Context(), install.PlanOptions{Components: &requested}), "")
	require.ErrorContains(t, err, "components")
	require.NotContains(t, err.Error(), "Python")
}

// Synthetic source and wheels exercise the real hash installer and activation transaction offline.
func TestSourceComponentSelectionUsesHashInstallAndRollback(t *testing.T) {
	for _, wrongHash := range []bool{false, true} {
		t.Run(fmt.Sprint(wrongHash), func(t *testing.T) {
			i, snapshot, tx, python := sourceLifecycle(t)
			snapshot.Recipe.Components = map[string]config.InstallComponent{"extra": {Description: "Optional test libraries", Requirements: "optional.txt"}}
			selected := []string{"extra"}
			require.NoError(t, snapshot.SelectComponents(&selected))
			extensionArchive(t, &snapshot, tx.directory, map[string]string{"release/optional.txt": "zzrouter-test-extra==1.0\nzzrouter-test-base<2\n"})
			i.cfg.Recipe = func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }
			roots, err := i.sourceRoots(t.Context(), snapshot, tx.directory)
			require.NoError(t, err)
			require.Equal(t, ">=1,<2,<2", roots["zzrouter-test-base"])
			require.Contains(t, roots, "zzrouter-test-extra")
			wheels := t.TempDir()
			base := extensionWheel(t, wheels, "zzrouter-test-base", "")
			extra := extensionWheel(t, wheels, "zzrouter-test-extra", "zzrouter-test-base<2")
			inventory := extensionReport(t, []install.ResolvedPackage{base, extra}, roots)
			if wrongHash {
				inventory[1].SHA256 = strings.Repeat("a", 64)
			}
			plan := &install.Plan{Recipe: &snapshot, InstallDir: tx.directory, Version: snapshot.Recipe.Source.Version, PlanID: "component-plan", Inventory: inventory}
			flags := []string{"--disable-pip-version-check", "install", "--no-input", "--no-index", "--find-links", wheels, "--only-binary=:all:"}
			requirements := rootRequirements(roots)
			flags = append(flags, requirements...)
			choice := &interpreter.Choice{Path: python, Realpath: python, Version: "3.14.0"}
			require.NoError(t, i.configureRecipePlan(plan, choice, nil, snapshot.RuntimeChecks(schema.RuntimeChecks{Kernels: "unknown"}), flags, requirements, time.Minute, install.PlanOptions{Force: true, Components: &selected}))
			plan.Steps[2].UsePTY = false
			before := extensionLive(t, snapshot, tx.directory)
			oldManifest, err := os.ReadFile(fsroot.ProviderManifestPath(snapshot.Runtime))
			require.NoError(t, err)
			var failed *install.StepResult
			for _, step := range plan.Steps {
				result := plan.ExecuteStep(t.Context(), step.Number)
				if !result.Passed {
					failed = result
					break
				}
			}
			if wrongHash {
				require.NotNil(t, failed)
				require.Contains(t, failed.Message, "HASHES")
				require.NoError(t, plan.Rollback())
				require.Equal(t, before, extensionLive(t, snapshot, tx.directory))
				after, err := os.ReadFile(fsroot.ProviderManifestPath(snapshot.Runtime))
				require.NoError(t, err)
				require.Equal(t, oldManifest, after)
				return
			}
			require.Nil(t, failed)
			manifest, err := fsroot.ReadInstallManifest(snapshot.Runtime)
			require.NoError(t, err)
			require.Equal(t, selected, manifest.SelectedComponents)
			require.Empty(t, manifest.AdditionalPackages, "release components do not need an operator package grant")
			require.Equal(t, inventory, manifest.Inventory)
			require.True(t, i.IsInstalled())
			require.NoError(t, runPython(t.Context(), fsroot.ProviderVenvPython(snapshot.Runtime), "", nil, "-I", "-c", "import zzrouter_test_extra"))
			for _, name := range []string{"data", "python"} {
				require.Equal(t, before[name], savedTree(t, filepath.Join(tx.directory, name)))
			}
		})
	}
}
