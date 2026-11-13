package prov_apps

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
)

func TestInstallBuilderPreservesComponentsThroughProductionResolver(t *testing.T) {
	isolateProviderRoot(t)
	root := t.TempDir()
	require.NoError(t, templates.InstallDefaults(root))
	apps, err := config.LoadAppsConfig(root)
	require.NoError(t, err)
	mgr, err := NewProviderAppManager(apps)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(t.Context()) })
	snapshot, err := mgr.resolveRecipe("comfyui", "comfyui")
	require.NoError(t, err)
	require.Empty(t, snapshot.SelectedComponents, "the desired resolver is independent of installed state")
	manifest := &fsroot.InstallManifest{Version: snapshot.Recipe.Source.Version, WorkingDir: snapshot.WorkingDir, Recipe: &snapshot.Recipe, RecipeFingerprint: snapshot.Fingerprint, PlanID: "installed", SelectedComponents: []string{"manager"}, Interpreter: &fsroot.ManifestInterpreter{Path: "python", Version: "3.14.0"}, Inventory: []fsroot.ManifestPackage{{Name: "torch", Version: "2.9"}}}
	require.NoError(t, fsroot.WriteInstallManifest("comfyui", manifest))
	installer, err := mgr.Install().Installer("comfyui")
	require.NoError(t, err)
	for _, action := range []string{"install", "upgrade"} {
		t.Run(action, func(t *testing.T) {
			ctx := install.WithPlanOptions(t.Context(), install.PlanOptions{Action: action, Force: true})
			plan, err := installer.InstallPlan(ctx, "")
			require.NoError(t, err)
			require.Equal(t, []string{"manager"}, plan.Recipe.SelectedComponents)
			empty := []string{}
			plan, err = installer.InstallPlan(install.WithPlanOptions(t.Context(), install.PlanOptions{Components: &empty, Force: true}), "")
			require.NoError(t, err)
			require.Equal(t, []string{}, plan.Recipe.SelectedComponents)
		})
	}
	manifest.SelectedComponents = []string{"retired"}
	require.NoError(t, fsroot.WriteInstallManifest("comfyui", manifest))
	_, err = installer.InstallPlan(t.Context(), "")
	require.ErrorContains(t, err, "explicitly select", "an unavailable installed choice must never be silently removed")
}
