package views

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/require"
)

func consoleFixture(t *testing.T) *ConsoleViewModel {
	t.Helper()
	m := NewConsoleViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()), pkgClient.ConsoleTarget{Page: "access", Item: "teams/team-one"})
	m.width = 100
	m.handoff = func(context.Context, pkgClient.ConsoleTarget) (*pkgClient.ConsoleGrant, error) {
		return &pkgClient.ConsoleGrant{Token: "temporary-code", URL: "https://router.example/console/"}, nil
	}
	m.discover = func(_ context.Context, target pkgClient.ConsoleTarget) (string, error) {
		require.Equal(t, "teams/team-one", target.Item)
		return "https://router.example/console/#access?item=teams%2Fteam-one", nil
	}
	t.Cleanup(m.Cancel)
	return m
}

func TestConsoleViewDiscoveryAndEffectsUseCommands(t *testing.T) {
	m := consoleFixture(t)
	discover := m.Init()
	require.Empty(t, m.url)
	m.Update(discover())
	require.False(t, m.loading)
	require.Contains(t, m.View().Content, "https://router.example")
	var opened, copied string
	m.open = func(value string) error { opened = value; return nil }
	m.copy = func(value string) error { copied = value; return nil }
	m.browserAvailable = func() bool { return true }
	_, open := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, open)
	require.Empty(t, opened, "opener must run outside the update loop")
	_, duplicate := m.Update(keyPress("o"))
	require.Nil(t, duplicate)
	m.Update(open())
	require.Equal(t, m.url, opened)
	_, copyURL := m.Update(keyPress("c"))
	require.Empty(t, copied)
	m.Update(copyURL())
	require.Equal(t, m.url, copied)
	_, back := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.IsType(t, tui.NavBackMsg{}, back())
}

func TestConsoleViewRemoteAndFailedOpenKeepUsableURL(t *testing.T) {
	m := consoleFixture(t)
	m.Update(m.Init()())
	m.browserAvailable = func() bool { return false }
	_, cmd := m.Update(keyPress("o"))
	require.NotNil(t, cmd)
	m.Update(cmd())
	require.Equal(t, "temporary-code", m.code)
	require.NotEmpty(t, m.url)
	m.browserAvailable = func() bool { return true }
	m.open = func(string) error { return errors.New("no browser") }
	_, cmd = m.Update(keyPress("o"))
	m.Update(cmd())
	require.Contains(t, m.status, "Use the URL above")
	require.NotEmpty(t, m.url)
}

func TestConsoleViewCancellationAndForeignReplies(t *testing.T) {
	m, other := consoleFixture(t), consoleFixture(t)
	m.Update(consoleResultMsg{owner: other, url: "https://wrong.example"})
	require.Empty(t, m.url)
	cmd := m.Init()
	m.Cancel()
	require.Nil(t, cmd())
	m.Update(consoleResultMsg{owner: m, url: "https://late.example"})
	require.Empty(t, m.url)
}

func TestConsoleViewFitsNarrowTerminal(t *testing.T) {
	m := consoleFixture(t)
	m.Update(m.Init()())
	m.Update(tea.WindowSizeMsg{Width: 42, Height: 18})
	frame := m.View().Content
	require.LessOrEqual(t, lipgloss.Width(frame), 42)
	require.LessOrEqual(t, lipgloss.Height(frame), 18)
	require.Contains(t, frame, "copy URL")
}
