package views

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stretchr/testify/require"
)

func TestProviderConnectionOwnershipShortcutsRequireConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, meaning string
		key                         rune
	}{
		{"remove", http.MethodDelete, "/connection", "disable external credential fallback", 'd'},
		{"external", http.MethodPost, "/connection/use-external", "declared environment or credential-cache values", 'e'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			m := connectionWriteFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Equal(t, tc.method, r.Method)
				require.Equal(t, "/zzrouter/v1/providers/fixture"+tc.path, r.URL.Path)
				var body struct {
					ExpectedRevision uint64 `json:"expected_revision"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, uint64(7), body.ExpectedRevision)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": client.ConnectionApplyResult{
					Connection: &client.ProviderConnection{Provider: "fixture", Revision: 8}, Outcome: "saved", Saved: true, Durable: true,
				}})
			})
			m.keyInput.SetValue("synthetic-candidate")
			shortcut := tea.KeyPressMsg{Code: tc.key, Mod: tea.ModCtrl}
			for _, dismiss := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: 'n'}, {Code: tea.KeyEscape}} {
				_, command := m.Update(shortcut)
				require.Nil(t, command, "the shortcut opens a prompt without scheduling HTTP")
				require.False(t, m.NavigationBlocked())
				require.Contains(t, strings.Join(strings.Fields(ansi.Strip(m.viewEnterKey(100, 20))), " "), tc.meaning)
				for _, size := range []struct{ width, height int }{{100, 20}, {42, 14}, {28, 10}} {
					rendered := m.viewEnterKey(size.width, size.height)
					require.LessOrEqual(t, lipgloss.Width(rendered), size.width)
					require.LessOrEqual(t, lipgloss.Height(rendered), size.height)
					require.Contains(t, rendered, "Y confirm")
					require.NotContains(t, rendered, "synthetic-candidate")
				}
				for _, message := range []tea.Msg{
					tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl},
					tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl}, tea.PasteMsg{Content: "unexpected-paste"},
				} {
					_, command = m.Update(message)
					require.Nil(t, command, "confirmation consumes unrelated commands and candidate input")
					require.Equal(t, "synthetic-candidate", m.keyInput.Value())
				}
				_, command = m.Update(dismiss)
				require.Nil(t, command)
				require.Equal(t, providersViewEnterKey, m.mode, "dismiss returns to the same connection form")
				require.Equal(t, "synthetic-candidate", m.keyInput.Value())
				require.Zero(t, calls.Load())
			}
			_, command := m.Update(shortcut)
			require.Nil(t, command)
			_, command = m.Update(tea.KeyPressMsg{Code: 'y'})
			require.NotNil(t, command, "only explicit confirmation schedules the existing write")
			require.True(t, m.NavigationBlocked())
			_, duplicate := m.Update(shortcut)
			require.Nil(t, duplicate)
			m.Update(command())
			require.EqualValues(t, 1, calls.Load())
			require.False(t, m.NavigationBlocked())
			require.Equal(t, uint64(8), m.connection.Revision)
		})
	}
}
