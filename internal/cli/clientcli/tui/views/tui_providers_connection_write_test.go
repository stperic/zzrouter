package views

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/require"
)

func connectionWriteFixture(t *testing.T, handler http.HandlerFunc) *ProvidersViewModel {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	m := NewProvidersViewModel(client.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	m.mode, m.installProvider = providersViewEnterKey, "fixture"
	m.connection = &client.ProviderConnection{Provider: "fixture", Revision: 7, ManagedAvailable: true}
	m.connectionValues = map[string]string{}
	return m
}

func TestProviderConnectionWriteSurvivesAllNavigationUntilResponse(t *testing.T) {
	for _, action := range []string{"save", "remove", "external"} {
		t.Run(action, func(t *testing.T) {
			started, release := make(chan context.Context, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var calls atomic.Int32
			m := connectionWriteFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				started <- r.Context()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": client.ConnectionApplyResult{Connection: &client.ProviderConnection{Provider: "fixture", Revision: 8}, Saved: true, Durable: true, Outcome: "saved"}})
			})
			root := tui.NewRoot(m)
			command := m.connectionCommand(action)
			require.NotNil(t, command)
			require.True(t, m.NavigationBlocked())
			_, bounded := m.connectionContext.Deadline()
			require.True(t, bounded)
			done := make(chan tea.Msg, 1)
			go func() { done <- command() }()
			requestContext := <-started
			for _, message := range []tea.Msg{
				tea.KeyPressMsg{Code: tea.KeyEscape}, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl},
				tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}, tea.MouseClickMsg{Button: tea.MouseRight}, tui.BreadcrumbClickMsg{Level: 1},
				tui.NavToMsg{View: NewProvidersViewModel(nil, m.styles)}, tui.NavBackMsg{}, tui.NavReplaceMsg{View: NewProvidersViewModel(nil, m.styles)}, tui.NavQuitMsg{},
			} {
				_, next := root.Update(message)
				require.Nil(t, next)
				require.Same(t, m, root.Active())
				require.NoError(t, requestContext.Err())
				require.Equal(t, providersViewEnterKey, m.mode)
			}
			require.Nil(t, m.connectionCommand(action), "a pending command cannot be submitted twice")
			unblock()
			_, _ = root.Update(<-done)
			require.False(t, m.NavigationBlocked())
			require.Equal(t, uint64(8), m.connection.Revision)
			require.Contains(t, m.actionStatus, "saved")
			require.EqualValues(t, 1, calls.Load())
			_, _ = root.Update(tui.NavBackMsg{})
			require.Zero(t, root.Depth())
		})
	}
}

func TestProviderConnectionDiagnosticStillCancels(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	m := connectionWriteFixture(t, func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	done := make(chan tea.Msg, 1)
	command := m.connectionCommand("test")
	require.False(t, m.NavigationBlocked())
	go func() { done <- command() }()
	<-started
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	<-cancelled
	_, _ = m.Update(<-done)
	require.Equal(t, providersViewPickProvider, m.mode)
	require.False(t, m.connectionBusy)
}

func TestProviderConnectionUnknownOutcomeRequiresReadOnlyReload(t *testing.T) {
	for _, response := range []string{"unreadable", "missing", "server-error", "refused"} {
		t.Run(response, func(t *testing.T) {
			var writes atomic.Int32
			m := connectionWriteFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": client.ProviderConnection{Provider: "fixture", Revision: 8, ManagedAvailable: true}})
					return
				}
				writes.Add(1)
				switch response {
				case "unreadable":
					_, _ = io.WriteString(w, "{")
				case "missing":
					_, _ = io.WriteString(w, `{}`)
				case "server-error":
					http.Error(w, "response unavailable", http.StatusInternalServerError)
				case "refused":
					http.Error(w, "revision changed", http.StatusConflict)
				}
			})
			_, _ = m.Update(m.connectionCommand("save")())
			require.False(t, m.NavigationBlocked())
			require.False(t, m.connectionBusy)
			if response == "refused" {
				require.Contains(t, m.actionStatus, "request failed")
				require.NotNil(t, m.connection)
				return
			}
			require.Contains(t, m.actionStatus, "outcome unknown")
			require.Contains(t, m.viewEnterKey(80, 24), "Ctrl+R reload")
			require.Nil(t, m.connectionCommand("save"))
			_, reload := m.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
			require.NotNil(t, reload)
			_, _ = m.Update(reload())
			require.Equal(t, uint64(8), m.connection.Revision)
			require.EqualValues(t, 1, writes.Load(), "recovery reads current state without retrying a write")
		})
	}
}

func TestProviderConnectionCanceledRequestReleasesNavigation(t *testing.T) {
	m := connectionWriteFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("cancelled request must not be sent") })
	command := m.connectionCommand("save")
	m.connectionCancel()
	_, _ = m.Update(command())
	require.False(t, m.NavigationBlocked())
	require.False(t, m.connectionBusy)
	require.Contains(t, m.actionStatus, "outcome unknown")
}
