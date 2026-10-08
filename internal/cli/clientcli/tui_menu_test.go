package clientcli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views"
	"github.com/stperic/zzrouter/pkg/ui"
)

// A view nobody can reach is a view that does not exist. The menu entry
// and the constructor are two separate edits, and only one of them is
// the one you remember: a missing case in viewForKind leaves the item on
// the menu and does nothing when it is chosen.
func TestMenu_EveryEntryBuildsAView(t *testing.T) {
	m := &tuiModel{styles: ui.NewStyles(ui.CatppuccinMocha())}

	var checked int
	for _, item := range mainMenuItems {
		entries := []menuItem{item}
		if item.isGroup {
			entries = item.children
		}
		for _, entry := range entries {
			if entry.isGroup {
				continue
			}
			checked++
			if entry.view == tuiViewSearch {
				// Search is off-stack: enterMenuItem switches pane
				// instead of pushing, and viewForKind says so with nil.
				assert.Nil(t, m.viewForKind(entry.view), "search is not pushed on the root stack")
				continue
			}
			assert.NotNil(t, m.viewForKind(entry.view),
				"menu entry %q has no case in viewForKind, so choosing it does nothing", entry.name)
		}
	}
	require.Greater(t, checked, 5, "the menu walk found almost nothing; the shape changed")
}

// The software-update view is reachable, and reachable from Advanced —
// it acts on one node's binary and does not belong beside the everyday
// entries.
func TestMenu_SoftwareUpdateIsUnderAdvanced(t *testing.T) {
	var found bool
	for _, item := range mainMenuItems {
		if !item.isGroup {
			continue
		}
		for _, child := range item.children {
			if child.view == tui.ViewUpdate {
				found = true
				assert.Equal(t, "Advanced", item.name)
				assert.Contains(t, child.description, "node")
			}
		}
	}
	require.True(t, found, "no menu entry reaches the software-update view")

	m := &tuiModel{styles: ui.NewStyles(ui.CatppuccinMocha())}
	view := m.viewForKind(tuiViewUpdate)
	require.NotNil(t, view)
	_, ok := view.(*views.UpdateViewModel)
	assert.True(t, ok, "the update menu entry built something else")
}
