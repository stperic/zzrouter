package clientcli

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views/search"
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

// The software-update view is reachable under Manage.
// it acts on one node's binary and does not belong beside the everyday
// entries.
func TestMenu_SoftwareUpdateIsUnderManage(t *testing.T) {
	var found bool
	for _, item := range mainMenuItems {
		if !item.isGroup {
			continue
		}
		for _, child := range item.children {
			if child.view == tui.ViewUpdate {
				found = true
				assert.Equal(t, "Manage", item.name)
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

func menuFixture(t *testing.T) *tuiModel {
	t.Helper()
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	styles := ui.NewStyles(ui.CatppuccinMocha())
	m := &tuiModel{
		styles: styles, root: tui.NewEmptyRoot(), termWidth: 80, termHeight: 24,
		search: search.NewSearchTUIModel(nil, styles, "", "", "", "", "", true),
	}
	t.Cleanup(m.search.Deactivate)
	t.Cleanup(m.root.TeardownAll)
	return m
}

func TestMenuEveryDestinationAndDefaultFocus(t *testing.T) {
	m := menuFixture(t)
	visible := m.syncMenu()
	var top []string
	for _, item := range visible {
		top = append(top, item.item.name)
	}
	require.Equal(t, []string{"Models", "Providers", "Activity", "Manage"}, top)
	require.Equal(t, "Models", m.menu.Selected().Title())
	_ = m.updateMenu(tea.KeyPressMsg{Code: tea.KeyEnter})
	visible = m.syncMenu()
	require.Equal(t, "Models", m.menu.Selected().Title(), "expanding retains the group selection")
	require.Equal(t, []string{"Available", "Running", "Discover"}, []string{visible[1].item.name, visible[2].item.name, visible[3].item.name})
	_ = m.updateMenu(tea.KeyPressMsg{Code: tea.KeyEnd})
	_ = m.updateMenu(tea.KeyPressMsg{Code: tea.KeyEnter})
	visible = m.syncMenu()
	want := map[tuiView]string{
		tuiViewModels: "Available", tuiViewRunningModels: "Running", tuiViewSearch: "Discover",
		tuiViewProviders: "Providers", tuiViewDeploys: "Activity", tuiViewVkeys: "Keys", tuiViewTeams: "Teams",
		tuiViewNodes: "Nodes", tuiViewLogs: "Logs", tuiViewPricing: "Pricing", tuiViewUpdate: "Software Update", tuiViewQuickstart: "Guided Setup",
	}
	for _, item := range visible {
		if item.item.isGroup {
			continue
		}
		require.Equal(t, want[item.item.view], item.item.name)
		delete(want, item.item.view)
	}
	require.Empty(t, want, "every previous destination remains reachable")
	require.Equal(t, []string{"Models", "Available"}, m.viewForKind(tuiViewModels).(tui.Breadcrumber).Breadcrumb())
	require.Equal(t, []string{"Models", "Running"}, m.viewForKind(tuiViewRunningModels).(tui.Breadcrumber).Breadcrumb())
}

func TestMenuScrollResizeAndMouseUseSameRows(t *testing.T) {
	m := menuFixture(t)
	for i, item := range mainMenuItems {
		if item.isGroup {
			m.toggleMenuGroup(i)
		}
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 100, Height: 36}, {Width: 42, Height: 18}, {Width: 40, Height: 10}} {
		m.Update(size)
		_ = m.updateMenu(tea.KeyPressMsg{Code: tea.KeyEnd})
		frame := m.View().Content
		require.Contains(t, frame, "Guided Setup", "the selected row must remain visible")
		require.LessOrEqual(t, lipgloss.Width(frame), size.Width)
		require.LessOrEqual(t, lipgloss.Height(frame), size.Height)
		start, end := m.menu.VisibleRange()
		if size.Height < 36 {
			require.Greater(t, start, 0)
		}
		header := strings.Count(m.menuHeader(), "\n")
		m.menuMouseClick(tea.MouseClickMsg{Button: tea.MouseLeft, Y: header})
		require.Equal(t, start, m.menu.Cursor(), "clicking the first rendered row uses its scrolled index")
		m.menuMouseClick(tea.MouseClickMsg{Button: tea.MouseLeft, Y: header + 2*(end-start)})
		require.Equal(t, start, m.menu.Cursor(), "footer clicks do not select an off-screen row")
		m.menuMouseWheel(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
		cursor := m.menu.Cursor()
		m.renderMenu()
		start, end = m.menu.VisibleRange()
		require.GreaterOrEqual(t, cursor, start)
		require.Less(t, cursor, end)
	}
}

func TestMenuResizeKeepsCurrentSelectionVisible(t *testing.T) {
	m := menuFixture(t)
	for i, item := range mainMenuItems {
		if item.isGroup {
			m.toggleMenuGroup(i)
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 36})
	_ = m.updateMenu(tea.KeyPressMsg{Code: tea.KeyEnd})
	require.Contains(t, m.View().Content, "Guided Setup")
	for _, size := range []tea.WindowSizeMsg{{Width: 40, Height: 10}, {Width: 42, Height: 18}, {Width: 100, Height: 36}} {
		m.Update(size)
		frame := m.View().Content
		assert.Contains(t, frame, "Guided Setup", "resize must reveal the existing selection without another key")
		assert.Equal(t, "Guided Setup", m.menu.Selected().Title())
		start, end := m.menu.VisibleRange()
		assert.GreaterOrEqual(t, m.menu.Cursor(), start)
		assert.Less(t, m.menu.Cursor(), end)
	}
}

func TestMenuCollapseKeepsCorrectGroupAndNavigationCancelsViews(t *testing.T) {
	m := menuFixture(t)
	for i, item := range mainMenuItems {
		if item.isGroup {
			m.toggleMenuGroup(i)
		}
	}
	visible := m.syncMenu()
	for i, item := range visible {
		if item.item.name == "Manage" {
			m.menu.SetCursor(i)
			m.menuActivate(visible, i)
			break
		}
	}
	require.Equal(t, "Manage", m.menu.Selected().Title())
	m.enterMenuItem(tuiViewRunningModels)
	running := m.root.Active().(*views.ModelsViewModel)
	ctx := running.Context()
	m.Update(tui.NavBackMsg{})
	require.Error(t, ctx.Err())
	require.Equal(t, paneMenu, m.pane)
	m.enterMenuItem(tuiViewSearch)
	require.Equal(t, "Discover", m.search.Breadcrumb()[0])
	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 2})
	require.Equal(t, paneMenu, m.pane)
}
