package views

import (
	"context"
	"errors"
	"testing"

	"github.com/charmbracelet/x/ansi"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/require"
)

func providerListFixture(t *testing.T) *ProvidersViewModel {
	t.Helper()
	m := NewProvidersViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))
	t.Cleanup(m.Cancel)
	m.Update(providersLoadedMsg{ctx: m.loadContext, providers: []shared.ProviderInfo{
		{Name: "ollama", Node: "studio", Kind: "local", Enabled: true},
		{Name: "vllm", Node: "studio", Kind: "local", Enabled: false},
		{Name: "groq", Node: "cloud", Kind: "cloud", Enabled: true},
	}})
	return m
}

func TestProviderFiltersAndRefreshFailure(t *testing.T) {
	m := providerListFixture(t)
	require.Len(t, m.providers, 2)
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	require.Len(t, m.providers, 3)
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Len(t, m.providers, 2)
	m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	m.Update(tea.PasteMsg{Content: "vllm"})
	require.Len(t, m.providers, 1)
	require.Equal(t, "vllm", m.providers[0].Name)
	m.Update(providersLoadedMsg{ctx: m.loadContext, err: errors.New("inventory unavailable")})
	require.Len(t, m.providers, 1)
	require.Contains(t, m.actionStatus, "Refresh failed")
	m.filterInput.SetValue("no-match")
	m.filterProviders()
	require.Contains(t, ansi.Strip(m.viewList(80, 20)), "no-match", "empty search retains the visible input")
}

func TestProviderFailureIsNotAnEmptyInventory(t *testing.T) {
	m := NewProvidersViewModel(nil, ui.NewStyles(ui.ClaudeCodeTheme()))
	t.Cleanup(m.Cancel)
	m.Update(providersLoadedMsg{ctx: m.loadContext, err: errors.New("inventory unavailable")})
	view := m.viewList(80, 20)
	require.Contains(t, view, "inventory unavailable")
	require.NotContains(t, view, "No providers")
}

func TestProviderUninstallConfirmsAndRetiresRead(t *testing.T) {
	m := providerListFixture(t)
	_ = m.fetchProvidersCmd(true)
	old := m.loadContext
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.Nil(t, cmd)
	require.NotNil(t, m.uninstallTarget)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, m.uninstallTarget, "Enter cancels a destructive confirmation")
	m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.NotNil(t, cmd)
	require.True(t, m.NavigationBlocked())
	require.ErrorIs(t, old.Err(), context.Canceled)
	_, reload := m.Update(providersActionMsg{ctx: m.uninstallContext, action: "uninstall", err: context.DeadlineExceeded})
	require.NotNil(t, reload)
	require.True(t, m.uninstallUncertain)
	m.Update(providersLoadedMsg{ctx: old})
	require.True(t, m.uninstallUncertain)
	require.Len(t, m.providers, 2)
}
