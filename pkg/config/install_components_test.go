package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallComponentsAreContainedSourceRequirements(t *testing.T) {
	recipe := sourceRecipeFixture()
	recipe.Components = map[string]InstallComponent{"manager": {Description: "Optional Manager libraries", Requirements: "manager_requirements.txt"}}
	require.NoError(t, recipe.Validate())
	for _, tc := range []struct{ id, description, file string }{
		{"Manager", "libraries", "manager.txt"}, {"../escape", "libraries", "manager.txt"},
		{"manager", "", "manager.txt"}, {"manager", "line\nbreak", "manager.txt"},
		{"manager", "libraries", "requirements.txt"}, {"manager", "libraries", "../requirements.txt"},
		{"manager", "libraries", ""}, {"manager", "libraries", "/requirements.txt"},
	} {
		t.Run(tc.id+"/"+tc.file, func(t *testing.T) {
			r := sourceRecipeFixture()
			r.Components = map[string]InstallComponent{tc.id: {Description: tc.description, Requirements: tc.file}}
			require.Error(t, r.Validate())
		})
	}
	recipe.Components["other"] = recipe.Components["manager"]
	require.Error(t, recipe.Validate(), "one requirements member cannot have two component IDs")
	pip := recipeFixture()
	pip.Components = map[string]InstallComponent{"manager": {Description: "libraries", Requirements: "manager.txt"}}
	require.Error(t, pip.Validate())
}
