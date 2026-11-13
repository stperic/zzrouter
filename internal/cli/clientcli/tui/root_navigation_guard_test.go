package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

type guardedView struct {
	*fakeView
	pending bool
}

func (v *guardedView) NavigationBlocked() bool { return v.pending }
func (v *guardedView) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	_, command := v.fakeView.Update(message)
	return v, command
}

func TestRootPendingWritePreservesOwnerUntilOutcome(t *testing.T) {
	for _, message := range []tea.Msg{NavToMsg{View: newFake("next")}, NavBackMsg{}, NavReplaceMsg{View: newFake("next")}, NavQuitMsg{}} {
		view := &guardedView{fakeView: newFake("writer"), pending: true}
		root := NewRoot(view)
		_, command := root.Update(message)
		require.Nil(t, command)
		require.Same(t, view, root.Active())
		require.False(t, view.cancelled)
		_, _ = root.Update(tea.WindowSizeMsg{Width: 42, Height: 18})
		require.Equal(t, 42, view.lastSize.Width, "non-navigation messages still reach the owner")
		require.True(t, root.NavigationBlocked())
		view.pending = false
		_, _ = root.Update(NavBackMsg{})
		require.Empty(t, root.stack)
		require.True(t, view.cancelled)
	}
}
