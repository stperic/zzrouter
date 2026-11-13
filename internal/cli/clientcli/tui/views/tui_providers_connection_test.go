package views

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/lipgloss/v2"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/require"
)

func TestProviderConnectionFormUsesSchemaAndImmutableRemoteCandidate(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": client.ProviderConnection{Provider: "fixture", Scope: "coordinator", Mode: "managed", Revision: 7, ManagedAvailable: true, Fields: []config.ConnectionField{{ID: "account_id", Label: "Account", Type: "identifier", Env: "ACCOUNT_ID", Required: true}, {ID: "credential", Label: "Credential", Type: "text", Env: "UNUSUAL_NAME", Secret: true, Required: true}}, Values: map[string]string{"account_id": "account"}}})
			return
		}
		require.Equal(t, "/zzrouter/v1/providers/fixture/connection/test", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		received <- body
		_ = json.NewEncoder(w).Encode(map[string]any{"data": client.ConnectionTestResult{Status: "ok", Code: "accepted"}})
	}))
	defer server.Close()
	m := NewProvidersViewModel(client.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.CatppuccinMocha()))
	defer m.Cancel()
	m.installProvider = "fixture"
	cmd := m.beginConnection("fixture")
	require.NotNil(t, cmd)
	require.Nil(t, m.connection, "Update schedules HTTP; it does not perform it synchronously")
	m.handleConnectionResult(cmd().(providerConnectionMsg))
	require.Equal(t, "account", m.keyInput.Value())
	require.Equal(t, textinput.EchoNormal, m.keyInput.EchoMode)
	m.updateEnterKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, textinput.EchoPassword, m.keyInput.EchoMode, "masking is declared, not inferred from key names")
	m.keyInput.SetValue("candidate-secret")
	cmd = m.connectionCommand("test")
	m.connectionValues["credential"] = "later-value"
	result := cmd().(providerConnectionMsg)
	require.NoError(t, result.err)
	body := <-received
	require.Equal(t, "candidate-secret", body["values"].(map[string]any)["credential"], "commands capture values before returning")
	m.handleConnectionResult(result)
	require.Contains(t, m.actionStatus, "No changes were saved")
	for _, width := range []int{28, 80, 140} {
		require.NotContains(t, m.viewEnterKey(width, 24), "candidate-secret")
	}
}

func TestProviderConnectionLateReplyCannotReplaceReenteredForm(t *testing.T) {
	m := NewProvidersViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	defer m.Cancel()
	m.mode = providersViewEnterKey
	m.installProvider = "fixture"
	_ = m.beginConnection("fixture")
	old := providerConnectionMsg{ctx: m.connectionContext, provider: "fixture", status: &client.ProviderConnection{Revision: 1}}
	m.cancelConnection()
	_ = m.beginConnection("fixture")
	m.connection = &client.ProviderConnection{Revision: 9}
	require.Nil(t, m.handleConnectionResult(old))
	require.Equal(t, uint64(9), m.connection.Revision)
	require.True(t, m.connectionBusy, "a retired reply cannot clear the replacement's loading state")
}

func TestProviderConnectionReplyCannotCrossViewLifetimes(t *testing.T) {
	for _, pop := range []bool{true, false} {
		name := "popped"
		if !pop {
			name = "inactive"
		}
		t.Run(name, func(t *testing.T) {
			var revision atomic.Uint64
			revision.Store(1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": client.ProviderConnection{
					Provider: "fixture", Revision: revision.Load(),
					Fields: []config.ConnectionField{{ID: "account", Label: "Account", Type: "text"}},
					Values: map[string]string{"account": "from-server"},
				}})
			}))
			defer server.Close()
			c := client.NewClient(config.ClientNodeConfig{Address: server.URL})
			styles := ui.NewStyles(ui.CatppuccinMocha())
			first := NewProvidersViewModel(c, styles)
			first.installProvider = "fixture"
			root := tui.NewRoot(first)
			defer root.TeardownAll()
			oldCtx := first.Context()
			queued := first.beginConnection("fixture")()
			if pop {
				root.Update(tui.NavBackMsg{})
				require.Error(t, oldCtx.Err(), "Root pop cancels the first view")
			}
			revision.Store(9)
			second := NewProvidersViewModel(c, styles)
			second.installProvider = "fixture"
			root.Update(tui.NavToMsg{View: second})
			root.Update(second.beginConnection("fixture")())
			require.Equal(t, uint64(9), second.connection.Revision, "fresh-view control before replay")
			second.keyInput.SetValue("new-candidate")
			root.Update(queued)
			require.Equal(t, uint64(9), second.connection.Revision, "a different view's queued response must not match")
			require.Equal(t, "new-candidate", second.keyInput.Value())
		})
	}
}

func TestProviderConnectionInitialFailureCanReload(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/zzrouter/v1/providers/fixture/connection", r.URL.Path)
		if attempts.Add(1) == 1 {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": client.ProviderConnection{
			Provider: "fixture", Scope: "coordinator", Mode: "managed", Revision: 3,
			Fields: []config.ConnectionField{{ID: "account", Label: "Account", Type: "identifier"}},
			Values: map[string]string{"account": "reloaded-account"},
		}})
	}))
	defer server.Close()
	m := NewProvidersViewModel(client.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.CatppuccinMocha()))
	defer m.Cancel()
	m.installProvider = "fixture"
	initial := m.beginConnection("fixture")()
	m.Update(initial)
	require.Nil(t, m.connection)
	require.False(t, m.connectionBusy)
	require.Contains(t, m.actionStatus, "Connection request failed")
	require.Contains(t, m.viewEnterKey(80, 24), "Ctrl+R reload")
	requestCtx := m.connectionContext
	_, reload := m.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	require.NotNil(t, reload, "reload must remain available before any connection status was loaded")
	require.EqualValues(t, 1, attempts.Load(), "Update schedules the retry without performing I/O")
	require.True(t, m.connectionBusy)
	require.ErrorIs(t, requestCtx.Err(), context.Canceled)
	require.NotEqual(t, requestCtx.Done(), m.connectionContext.Done())
	m.Update(reload())
	require.EqualValues(t, 2, attempts.Load())
	require.NotNil(t, m.connection)
	require.Equal(t, uint64(3), m.connection.Revision)
	require.Equal(t, "reloaded-account", m.keyInput.Value())
	require.False(t, m.connectionBusy)
	m.Update(initial)
	require.NotContains(t, m.actionStatus, "failed", "a retired request's error must not replace the reloaded form")
}

func TestRegistryConnectionsAreReachableFromProviderPicker(t *testing.T) {
	m := NewProvidersViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	defer m.Cancel()
	m.buildTypePicker()
	require.Contains(t, m.pickerKeys, constants.AppModeRegistry)
	m.installType = constants.AppModeRegistry
	m.catalog = []client.ProviderCatalogEntry{{Key: "huggingface", Mode: constants.AppModeRegistry}}
	m.buildProviderPicker()
	require.Equal(t, []string{"huggingface"}, m.pickerKeys)
	require.NotNil(t, m.updatePicker(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Equal(t, providersViewEnterKey, m.mode)
	require.Equal(t, "huggingface", m.installProvider)
}

func TestProviderConnectionLongInputFitsResizedTerminal(t *testing.T) {
	m := NewProvidersViewModel(client.NewClient(config.ClientNodeConfig{Address: "https://coordinator.example"}), ui.NewStyles(ui.CatppuccinMocha()))
	defer m.Cancel()
	m.installProvider = "huggingface"
	m.mode = providersViewEnterKey
	m.connection = &client.ProviderConnection{Provider: "huggingface", Mode: "managed", ManagedAvailable: true, Configured: true, Fields: []config.ConnectionField{{ID: "token", Label: "Access token", Secret: true}}}
	m.connectionValues = map[string]string{}
	m.actionStatus = "connection changed; reload its revision before retrying"
	for _, size := range []struct{ width, height int }{{100, 24}, {42, 18}, {28, 14}} {
		m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		m.connectionInput()
		m.keyInput.SetValue(strings.Repeat("s", 300))
		rendered := m.viewEnterKey(size.width, m.termHeight)
		require.LessOrEqual(t, lipgloss.Width(rendered), size.width)
		require.LessOrEqual(t, lipgloss.Height(rendered), m.termHeight)
		require.NotContains(t, rendered, strings.Repeat("s", 5))
		require.Contains(t, rendered, "Secret: absent", "presence is independent of optional public connectivity")
	}
}
