package install

import (
	"fmt"
	"slices"

	"github.com/stperic/zzrouter/pkg/config"
)

// SelectComponents resolves an omitted selection from the installed snapshot once.
func (s *RecipeSnapshot) SelectComponents(requested *[]string) error {
	if s.Recipe.Source == nil && requested == nil && len(s.SelectedComponents) == 0 {
		return nil
	}
	selected := s.SelectedComponents
	if requested != nil {
		selected = *requested
	}
	canonical, err := ValidateComponents(s.Recipe, selected)
	if err != nil {
		return err
	}
	s.SelectedComponents = canonical
	return nil
}

// ValidateComponents returns a detached, sorted selection from a declared recipe.
func ValidateComponents(recipe config.InstallRecipe, selected []string) ([]string, error) {
	if len(selected) > 16 || selected != nil && recipe.Source == nil {
		return nil, fmt.Errorf("components: select only components declared by this source runtime")
	}
	canonical := append([]string{}, selected...)
	slices.Sort(canonical)
	for index, id := range canonical {
		if _, ok := recipe.Components[id]; !ok {
			return nil, fmt.Errorf("components: %q is not declared by this release; explicitly select the components to install", id)
		}
		if index > 0 && id == canonical[index-1] {
			return nil, fmt.Errorf("components: duplicate selection %q", id)
		}
	}
	return canonical, nil
}

// ComponentRequirements lists the release-owned members for a validated selection.
func (s RecipeSnapshot) ComponentRequirements() []string {
	files := make([]string, 0, len(s.SelectedComponents))
	for _, id := range s.SelectedComponents {
		files = append(files, s.Recipe.Components[id].Requirements)
	}
	return files
}
