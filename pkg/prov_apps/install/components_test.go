package install

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestComponentSelectionPreservesAndReplacesInstalledSelection(t *testing.T) {
	recipe := config.InstallRecipe{Source: &config.InstallSource{}, Components: map[string]config.InstallComponent{
		"manager": {Requirements: "manager_requirements.txt"}, "audio": {Requirements: "audio/requirements.txt"},
	}}
	for _, tc := range []struct {
		name      string
		installed []string
		requested *[]string
		want      []string
	}{
		{"fresh", nil, nil, []string{}},
		{"preserve", []string{"manager"}, nil, []string{"manager"}},
		{"remove", []string{"manager"}, new([]string{}), []string{}},
		{"replace and sort", []string{"manager"}, new([]string{"manager", "audio"}), []string{"audio", "manager"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := RecipeSnapshot{ResolvedInstall: config.ResolvedInstall{Recipe: recipe}, SelectedComponents: tc.installed}
			require.NoError(t, snapshot.SelectComponents(tc.requested))
			require.Equal(t, tc.want, snapshot.SelectedComponents)
			if len(snapshot.SelectedComponents) > 0 {
				snapshot.SelectedComponents[0] = "detached"
				if tc.requested != nil {
					require.NotContains(t, *tc.requested, "detached")
				}
				require.NotContains(t, tc.installed, "detached")
			}
		})
	}
	for _, ids := range [][]string{{"unknown"}, {"manager", "manager"}, {"manager", "unknown", "audio"}} {
		_, err := ValidateComponents(recipe, ids)
		require.Error(t, err)
	}
	_, err := ValidateComponents(config.InstallRecipe{}, []string{})
	require.Error(t, err, "even an explicit empty component option requires a source recipe")
}

func TestEffectiveComponentSelectionHasOnePlanIdentity(t *testing.T) {
	recipe := config.InstallRecipe{Source: &config.InstallSource{}, Components: map[string]config.InstallComponent{"manager": {Requirements: "manager_requirements.txt"}}}
	implicit := RecipeSnapshot{ResolvedInstall: config.ResolvedInstall{Recipe: recipe}, SelectedComponents: []string{"manager"}}
	explicit := implicit
	require.NoError(t, implicit.SelectComponents(nil))
	selected := []string{"manager"}
	require.NoError(t, explicit.SelectComponents(&selected))
	require.Equal(t, Fingerprint(implicit), Fingerprint(explicit))
	require.Equal(t, BoundPlanID(Fingerprint(implicit), "install", PlanOptions{}), BoundPlanID(Fingerprint(explicit), "install", PlanOptions{Components: &selected}))
	empty := []string{}
	require.NoError(t, explicit.SelectComponents(&empty))
	require.NotEqual(t, Fingerprint(implicit), Fingerprint(explicit))
}

func TestComponentDiscoveryDoesNotGrantInstallAuthority(t *testing.T) {
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/comfyui/config.yaml")
	require.NoError(t, err)
	var provider config.OnDemandProvider
	require.NoError(t, yaml.Unmarshal(data, &provider))
	sc := config.ServiceConfig{Name: "comfyui", Runtime: &config.AppRuntimeConfig{Execution: provider.Runtime.Execution, DataDir: provider.Runtime.DataDir}, Install: provider.Install, Requirements: provider.Requirements}
	snapshot, err := ResolveRecipe(sc, "worker", "comfyui", "", nil)
	require.NoError(t, err)
	recipe := provider.Install.Runtimes["comfyui"]
	component := recipe.Components["manager"]
	component.Requirements = "unapproved.txt"
	recipe.Components["manager"] = component
	provider.Install.Runtimes["comfyui"] = recipe
	require.Equal(t, snapshot.Fingerprint, RecipeFingerprint(recipe.ForOS(fsroot.CurrentPlatform().OS)), "available choices are discovery, not the installed runtime identity")
	_, err = ResolveRecipe(sc, "worker", "comfyui", "", nil)
	require.ErrorIs(t, err, ErrInstallPolicy, "unchanged base fingerprint cannot approve altered component declarations")
}
