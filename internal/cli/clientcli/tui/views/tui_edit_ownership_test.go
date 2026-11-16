package views

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPricingPendingWriteRetainsOwner(t *testing.T) {
	m := newPricingView(t, nil, nil)
	t.Cleanup(m.Cancel)
	m.openForm(pkgClient.PricingOverride{Model: "model"}, false)
	root := tui.NewRoot(m)
	require.NotNil(t, m.saveForm())
	require.Nil(t, m.saveForm())
	root.Update(tui.NavBackMsg{})
	assert.Same(t, m, root.Active())
	m.Update(pricingSavedMsg{ctx: context.Background()})
	assert.True(t, m.pending, "a foreign result cannot release the guard")
	m.Update(pricingSavedMsg{ctx: m.writeContext, err: errors.New("timeout")})
	assert.False(t, m.pending)
	assert.True(t, m.writeUncertain)
	assert.Nil(t, m.saveForm())
}

func TestKeyDraftProtectsEveryRootLeavePath(t *testing.T) {
	for _, leave := range []tea.Msg{tui.NavBackMsg{}, tui.NavReplaceMsg{View: NewVkeysViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))}, tui.NavMenuMsg{}} {
		m := NewVkeysViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))
		t.Cleanup(m.Cancel)
		m.openNewKeyForm()
		m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
		require.True(t, m.Dirty)
		root := tui.NewRoot(m)
		root.Update(leave)
		require.Same(t, m, root.Active())
		root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		assert.True(t, m.Dirty, "Enter keeps the draft")
		root.Update(leave)
		_, confirm := root.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		require.NotNil(t, confirm)
		root.Update(confirm())
		assert.False(t, root.Active() == m)
	}
}

func TestModelUsageRejectsOldSelectionAndGeneration(t *testing.T) {
	m := NewModelsViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))
	t.Cleanup(m.Cancel)
	loadModelRows(m, []modelRow{instanceRow("A", "a", "running"), instanceRow("B", "b", "running")}, nil)
	m.mode = modelsViewDetail
	_ = m.fetchModelUsage("A", "engine")
	oldCtx, oldTarget := m.usageContext, m.usageTarget
	m.list.SetCursor(1)
	_ = m.fetchModelUsage("B", "engine")
	m.Update(modelUsageMsg{ctx: oldCtx, target: oldTarget, idle: true})
	assert.False(t, m.modelUsageIdle)
	current := modelUsageMsg{ctx: m.usageContext, target: m.usageTarget, idle: true}
	m.Update(current)
	assert.True(t, m.modelUsageIdle)
	m.modelUsageIdle = false
	_ = m.fetchModelUsage("B", "engine")
	m.Update(current)
	assert.False(t, m.modelUsageIdle, "an older refresh of the same model is obsolete")
}

func TestNodeDetailRejectsPreviousSelection(t *testing.T) {
	m := NewNodesViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))
	t.Cleanup(m.Cancel)
	m.mode = nodesViewDetail
	_ = m.loadNodeDetail("A")
	old := m.detailContext
	_ = m.loadNodeDetail("B")
	m.Update(nodeDetailMsg{ctx: old, name: "A", node: shared.NodeInfo{Name: "A"}})
	assert.Nil(t, m.detailNode)
	m.Update(nodeDetailMsg{ctx: m.detailContext, name: "B", node: shared.NodeInfo{Name: "B"}})
	require.NotNil(t, m.detailNode)
	assert.Equal(t, "B", m.detailNode.Name)
}

func TestPricingObsoleteReadCannotClearUnknownWrite(t *testing.T) {
	m := newPricingView(t, []pkgClient.PricingOverride{{Model: "m"}}, nil)
	_ = m.fetchCmd()
	old := m.loadContext
	ctx := m.beginWrite()
	m.Update(pricingSavedMsg{ctx: ctx, err: context.DeadlineExceeded})
	m.Update(pricingLoadedMsg{owner: m, ctx: old})
	require.True(t, m.writeUncertain)
	m.confirmPending = true
	require.Nil(t, m.updateConfirm(tea.KeyPressMsg{Code: 'y', Text: "y"}))
	require.False(t, m.pending)
}

func TestRouteUnknownDeleteBlocksSubsequentDelete(t *testing.T) {
	m := NewModelsViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))
	t.Cleanup(m.Cancel)
	ctx := m.beginRouteWrite()
	m.Update(routesDeletedMsg{ctx: ctx, err: context.DeadlineExceeded})
	require.True(t, m.routeWriteUncertain)
	m.confirmKind = confirmDeleteRoute
	require.Nil(t, m.updateConfirm(tea.KeyPressMsg{Code: 'y', Text: "y"}))
	require.False(t, m.NavigationBlocked())
}

func TestDiscardEditReturnsToDetailsOnce(t *testing.T) {
	t.Run("key", func(t *testing.T) {
		m := NewVkeysViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))
		t.Cleanup(m.Cancel)
		m.mode = vkeysViewEditKey
		m.Dirty = true
		m.Draft.Discard = m.closeKeyForm
		m.Update(keyFormCloseMsg{owner: m})
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		require.NotNil(t, cmd)
		m.Update(cmd())
		require.Equal(t, vkeysViewDetail, m.mode)
	})
	t.Run("route", func(t *testing.T) {
		m := NewModelsViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))
		t.Cleanup(m.Cancel)
		loadModelRows(m, nil, []pkgClient.ModelGroupResponse{{Name: "target"}})
		m.activeRoute = "target"
		m.mode = modelsViewEditRoute
		m.Dirty = true
		m.Draft.Discard = m.discardRouteForm
		m.Update(routeCloseMsg{owner: m})
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		require.NotNil(t, cmd)
		m.Update(cmd())
		require.Equal(t, modelsViewRouteDetail, m.mode)
	})
}
