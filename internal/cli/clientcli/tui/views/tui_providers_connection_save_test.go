package views

import (
	"encoding/json"
	"net/http"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderConnectionSaveAndConnectRequestsActivation(t *testing.T) {
	for _, provider := range []string{"openai", "huggingface"} {
		for _, enabled := range []bool{false, true} {
			name := provider + "/disabled"
			if enabled {
				name = provider + "/enabled"
			}
			t.Run(name, func(t *testing.T) {
				type saveRequest struct {
					Activate bool `json:"activate"`
				}
				requests := make(chan saveRequest, 1)
				m := connectionWriteFixture(t, func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, http.MethodPut, r.Method)
					assert.Equal(t, "/zzrouter/v1/providers/"+provider+"/connection", r.URL.Path)
					var input saveRequest
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						http.Error(w, "bad request", http.StatusBadRequest)
						return
					}
					requests <- input
					active := enabled || input.Activate
					_ = json.NewEncoder(w).Encode(map[string]any{"data": client.ConnectionApplyResult{
						Connection: &client.ProviderConnection{Provider: provider, Revision: 8, Enabled: active, Configured: true, ManagedAvailable: true},
						Saved:      true, Durable: true, Active: active, Published: true, Outcome: "saved",
					}})
				})
				m.installProvider = provider
				m.connection.Provider = provider
				m.connection.Enabled = enabled
				for _, width := range []int{28, 36, 80} {
					rendered := m.viewEnterKey(width, 18)
					assert.Contains(t, rendered, "Save and connect")
					assert.LessOrEqual(t, lipgloss.Width(rendered), width)
				}
				_, command := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
				require.NotNil(t, command)
				message := command().(providerConnectionMsg)
				require.NoError(t, message.err)
				assert.True(t, (<-requests).Activate, "the existing Save and connect action requests onboarding")
				_, _ = m.Update(message)
				require.NotNil(t, m.connection)
				assert.True(t, m.connection.Enabled)
				assert.False(t, m.NavigationBlocked())
			})
		}
	}
}

func TestProviderConnectionSecretNavigationAndCleanup(t *testing.T) {
	const secret = "synthetic-hf-token"
	for _, outcome := range []string{"success", "unknown", "reload", "exit"} {
		t.Run(outcome, func(t *testing.T) {
			requests := make(chan map[string]string, 1)
			status := client.ProviderConnection{Provider: "huggingface", Revision: 8, ManagedAvailable: true,
				Fields: []config.ConnectionField{{ID: "token", Label: "Access token", Type: "text", Secret: true}},
			}
			m := connectionWriteFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": status})
					return
				}
				var input struct {
					Values map[string]string `json:"values"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				requests <- input.Values
				if outcome == "unknown" {
					http.Error(w, "response unavailable", http.StatusInternalServerError)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": client.ConnectionApplyResult{Connection: &status, Saved: true, Durable: true, Outcome: "saved"}})
			})
			m.installProvider, m.connection = "huggingface", &status
			m.connectionInput()
			m.Update(tea.PasteMsg{Content: secret})
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			require.Equal(t, 1, m.connectionField)
			require.Equal(t, secret, m.connectionValues["token"])
			m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
			assert.Equal(t, secret, m.keyInput.Value(), "returning to an optional secret field preserves the candidate")
			assert.Equal(t, textinput.EchoPassword, m.keyInput.EchoMode)
			for _, width := range []int{28, 80} {
				assert.NotContains(t, m.viewEnterKey(width, 18), secret)
			}
			switch outcome {
			case "success", "unknown":
				_, command := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
				require.NotNil(t, command)
				m.Update(command())
				assert.Equal(t, secret, (<-requests)["token"], "Ctrl+S sends the same complete candidate after field navigation")
				assert.False(t, m.NavigationBlocked())
				if outcome == "unknown" {
					assert.Nil(t, m.connection)
					assert.Contains(t, m.actionStatus, "outcome unknown")
				}
			case "reload":
				_, reload := m.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
				require.NotNil(t, reload)
				assert.Empty(t, m.keyInput.Value())
				m.Update(reload())
			case "exit":
				m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				assert.Equal(t, providersViewPickProvider, m.mode)
			}
			assert.Empty(t, m.keyInput.Value())
			assert.Empty(t, m.connectionValues["token"], "candidate secrets end with this form's write or lifetime")
		})
	}
}

func TestProviderConnectionBackPreservesUnsubmittedCandidate(t *testing.T) {
	for _, field := range []config.ConnectionField{
		{ID: "token", Label: "Token", Type: "text", Secret: true},
		{ID: "endpoint", Label: "Endpoint", Type: "endpoint", Required: true},
	} {
		t.Run(field.ID, func(t *testing.T) {
			m := connectionWriteFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("field navigation must not send HTTP") })
			m.connection.Fields = []config.ConnectionField{{ID: "account", Label: "Account", Type: "text", Required: true}, field}
			m.connectionInput()
			m.Update(tea.PasteMsg{Content: "account"})
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			require.Equal(t, 1, m.connectionField)
			m.Update(tea.PasteMsg{Content: "incomplete-candidate"})
			m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
			require.Zero(t, m.connectionField, "Back does not require the current candidate to pass forward validation")
			assert.Equal(t, "incomplete-candidate", m.connectionValues[field.ID])
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			require.Equal(t, 1, m.connectionField)
			assert.Equal(t, "incomplete-candidate", m.keyInput.Value(), "forward navigation restores the unsubmitted edit")
			if field.Secret {
				assert.Equal(t, textinput.EchoPassword, m.keyInput.EchoMode)
				assert.NotContains(t, m.viewEnterKey(80, 18), "incomplete-candidate")
			} else {
				assert.Equal(t, textinput.EchoNormal, m.keyInput.EchoMode)
				m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				assert.Equal(t, 1, m.connectionField, "Enter still validates the incomplete endpoint")
				assert.NotEmpty(t, m.actionStatus)
			}
		})
	}
}
