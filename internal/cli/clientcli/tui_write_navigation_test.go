package clientcli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views/search"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

type hostWriteView struct {
	pending, cancelled bool
}

func TestHostPendingDeploymentKeepsAcknowledgementAcrossBreadcrumbAttempts(t *testing.T) {
	started, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			started <- r.Context()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": client.Deployment{ID: "deployment-accepted"}})
			return
		}
		_, _ = io.WriteString(w, `{"data":{"discovery":{"provider":"ollama","api_id":"qwen3:cloud","display_name":"qwen3:cloud","is_cloud":true}}}`)
	}))
	t.Cleanup(server.Close)
	m := menuFixture(t)
	m.client = client.NewClient(config.ClientNodeConfig{Address: server.URL})
	m.search = search.NewSearchTUIModel(m.client, m.styles, "", "ollama", "", "", "", true)
	t.Cleanup(m.search.Deactivate)
	m.enterMenuItem(tuiViewSearch)
	m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	for _, ch := range "qwen3:cloud" {
		m.Update(tea.KeyPressMsg{Code: ch})
	}
	_, validate := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, validate)
	_, deploy := m.Update(validate())
	require.NotNil(t, deploy)
	require.True(t, m.search.NavigationBlocked())
	done := make(chan tea.Msg, 1)
	go func() { done <- deploy() }()
	ctx := <-started
	for _, message := range []tea.Msg{
		tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 2}, tui.TuiBackMsg{}, tui.SwitchToModelsMsg{},
		tui.NavToMsg{View: &hostWriteView{}}, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl},
	} {
		_, next := m.Update(message)
		require.Nil(t, next)
		require.Equal(t, paneSearch, m.pane)
		require.NoError(t, ctx.Err())
	}
	require.Nil(t, m.enterMenuItem(tuiViewModels))
	unblock()
	m.Update(<-done)
	require.False(t, m.search.NavigationBlocked())
	require.Equal(t, "deployment-accepted", m.search.LastDeployment.ID)
	require.Contains(t, m.search.PullStatus, "See Activity")
	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 2})
	require.Equal(t, paneMenu, m.pane)
}

func (*hostWriteView) Init() tea.Cmd                         { return nil }
func (v *hostWriteView) Update(tea.Msg) (tea.Model, tea.Cmd) { return v, nil }
func (*hostWriteView) View() tea.View                        { return tea.NewView("pending") }
func (*hostWriteView) Breadcrumb() []string                  { return []string{"Providers", "Connection"} }
func (v *hostWriteView) NavigationBlocked() bool             { return v.pending }
func (v *hostWriteView) Cancel()                             { v.cancelled = true }

func TestHostPendingWriteBlocksCrossPaneNavigation(t *testing.T) {
	m := menuFixture(t)
	view := &hostWriteView{pending: true}
	m.root, m.pane = tui.NewRoot(view), paneRoot
	for _, message := range []tea.Msg{
		tui.NavToMsg{View: &hostWriteView{}}, tui.NavBackMsg{}, tui.NavReplaceMsg{View: &hostWriteView{}}, tui.NavQuitMsg{},
		tui.BreadcrumbClickMsg{Level: 0}, tui.LaunchChatMsg{}, tui.SwitchToModelsMsg{}, tui.OpenParamsEditorMsg{},
		tui.LaunchRunLogsMsg{}, tui.QsLaunchViewMsg{}, tui.QsEnterMenuMsg{}, tui.TuiBackMsg{},
		tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 2},
	} {
		_, command := m.Update(message)
		require.Nil(t, command)
		require.Equal(t, paneRoot, m.pane)
		require.Same(t, view, m.root.Active())
		require.False(t, view.cancelled)
	}
	require.Nil(t, m.dispatchBreadcrumbClick(2))
	require.Nil(t, m.enterMenuItem(tuiViewSearch))
	require.Nil(t, m.pushOnRoot(&hostWriteView{}))
	require.Nil(t, m.popRoot())
	require.Equal(t, paneRoot, m.pane)
	view.pending = false
	_, _ = m.Update(tui.NavBackMsg{})
	require.Equal(t, paneMenu, m.pane)
	require.True(t, view.cancelled)
}
