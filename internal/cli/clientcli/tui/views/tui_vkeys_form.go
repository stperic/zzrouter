package views

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

func (m *VkeysViewModel) openNewKeyForm() {
	m.mode = vkeysViewNewKey
	m.formID.SetValue("")
	m.formName.SetValue("")
	m.formRole = 0
	m.formTeamID = ""
	m.formHasExpiry = false
	m.formExpires.SetValue("")
	m.formFocus = vkeysFieldID
	m.statusMsg = ""
	m.writeUncertain = false
	m.formDetail.Reset()
	m.setKeyFormFocus()
}

func (m *VkeysViewModel) openEditKeyForm(k *pkgClient.KeyResponse) {
	m.mode = vkeysViewEditKey
	m.formID.SetValue(k.ID)
	m.formName.SetValue(k.Name)
	m.formRole = 0
	if k.Role == "admin" {
		m.formRole = 1
	}
	m.formTeamID = k.TeamID
	m.formHasExpiry = k.ExpiresAt != nil
	m.formExpires.SetValue("")
	if k.ExpiresAt != nil {
		m.formExpires.SetValue(*k.ExpiresAt)
	}
	m.formFocus = vkeysFieldName
	m.statusMsg = ""
	m.writeUncertain = false
	m.formDetail.Reset()
	m.setKeyFormFocus()
}

func (m *VkeysViewModel) setKeyFormFocus() {
	m.formID.Blur()
	m.formName.Blur()
	m.formExpires.Blur()
	switch m.formFocus {
	case vkeysFieldID:
		m.formID.Focus()
	case vkeysFieldName:
		m.formName.Focus()
	case vkeysFieldExpires:
		m.formExpires.Focus()
	}
}

func (m *VkeysViewModel) updateKeyForm(msg tea.KeyPressMsg) tea.Cmd {
	if m.pending {
		return nil
	}
	switch msg.String() {
	case "pgup", "pgdown", "home", "end":
		m.formDetail.UpdateKey(msg)
		return nil
	case "tab", "shift+tab":
		m.formDetail.Reset()
		fields := []vkeysFormField{vkeysFieldName, vkeysFieldRole, vkeysFieldExpires}
		if m.mode == vkeysViewNewKey {
			fields = []vkeysFormField{vkeysFieldID, vkeysFieldName, vkeysFieldTeam, vkeysFieldRole, vkeysFieldExpires}
		}
		for i, field := range fields {
			if m.formFocus == field {
				step := 1
				if msg.String() == "shift+tab" {
					step = -1
				}
				m.formFocus = fields[(i+step+len(fields))%len(fields)]
				break
			}
		}
		m.setKeyFormFocus()
		return nil
	case "left", "right":
		if m.formFocus == vkeysFieldRole {
			m.formRole = 1 - m.formRole
			return nil
		}
		if m.formFocus == vkeysFieldTeam {
			teams := m.sharedTeams()
			if len(teams) == 0 {
				return nil
			}
			index := -1
			for i, team := range teams {
				if team.ID == m.formTeamID {
					index = i
					break
				}
			}
			step := 1
			if msg.String() == "left" {
				step = -1
			}
			if index < 0 {
				index = 0
			} else {
				index = (index + step + len(teams)) % len(teams)
			}
			m.formTeamID = teams[index].ID
			return nil
		}
	case "ctrl+s":
		return m.saveKeyForm()
	case "ctrl+o":
		return tui.NavTo(NewConsoleViewModel(m.client, m.styles, pkgClient.ConsoleTarget{Page: "access", Item: "teams"}))
	case "esc":
		if m.mode == vkeysViewEditKey {
			m.mode = vkeysViewDetail
		} else {
			m.mode = vkeysViewList
		}
		m.statusMsg = ""
		return nil
	}
	var cmd tea.Cmd
	switch m.formFocus {
	case vkeysFieldID:
		m.formID, cmd = m.formID.Update(msg)
	case vkeysFieldName:
		m.formName, cmd = m.formName.Update(msg)
	case vkeysFieldExpires:
		m.formExpires, cmd = m.formExpires.Update(msg)
	}
	return cmd
}

func (m *VkeysViewModel) sharedTeams() []*pkgClient.TeamResponse {
	var result []*pkgClient.TeamResponse
	if m.teamsErr != nil || !m.teamsLoaded {
		return result
	}
	for _, team := range m.teams {
		if team.Kind == "shared" && !team.Suspended {
			result = append(result, team)
		}
	}
	return result
}

func (m *VkeysViewModel) keyTeam() *pkgClient.TeamResponse {
	for _, team := range m.teams {
		if team.ID == m.formTeamID {
			return team
		}
	}
	return nil
}

// keyExpiry accepts the displayed RFC3339 value unchanged, or a date ending at
// 23:59:59 UTC. The API currently has no operation for clearing an expiry.
func keyExpiry(value string) (*string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if len(value) == len("2006-01-02") {
		value += "T23:59:59Z"
	}
	_, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("expiry must be YYYY-MM-DD or an RFC3339 timestamp")
	}
	return &value, nil
}

func (m *VkeysViewModel) saveKeyForm() tea.Cmd {
	if m.pending || m.writeUncertain {
		return nil
	}
	m.formDetail.Reset()
	id, name := strings.TrimSpace(m.formID.Value()), strings.TrimSpace(m.formName.Value())
	if id == "" || name == "" {
		m.statusMsg = "Key ID and name are required"
		return nil
	}
	role := "user"
	if m.formRole == 1 {
		role = "admin"
	}
	expires, err := keyExpiry(m.formExpires.Value())
	if err != nil {
		m.statusMsg = err.Error()
		return nil
	}
	client, ctx := m.client, m.Context()
	if m.mode == vkeysViewNewKey {
		var chosen *pkgClient.TeamResponse
		for _, team := range m.sharedTeams() {
			if team.ID == m.formTeamID {
				chosen = team
				break
			}
		}
		if chosen == nil {
			m.statusMsg = "Select an active shared team. Manage teams with Ctrl+O."
			return nil
		}
		req := &pkgClient.CreateKeyRequest{ID: id, Name: name, Role: role, TeamID: chosen.ID, TeamRole: "member", ExpiresAt: expires}
		m.pending, m.statusMsg = true, "Creating key..."
		return func() tea.Msg {
			resp, err := client.CreateKeyContext(ctx, req)
			if ctx.Err() != nil {
				return nil
			}
			return vkeysCreatedMsg{owner: m, resp: resp, err: err}
		}
	}
	// Credentials and expiry only. Team membership is immutable; all quota and
	// metadata fields are omitted so existing additional limits are preserved.
	if m.formHasExpiry && expires == nil {
		m.statusMsg = "The API cannot remove an expiry. Keep the existing timestamp or enter a new date."
		return nil
	}
	req := &pkgClient.UpdateKeyRequest{Name: &name, Role: &role, ExpiresAt: expires}
	m.pending, m.statusMsg = true, "Saving key..."
	return func() tea.Msg {
		updated, err := client.UpdateKeyContext(ctx, id, req)
		if ctx.Err() != nil {
			return nil
		}
		return vkeysUpdatedMsg{owner: m, key: updated, err: err}
	}
}

func (m *VkeysViewModel) keyWriteFailed(err error) {
	var apiErr *pkgClient.APIError
	// Transport/server failures can arrive after commit. Do not offer a second
	// submission of an unrecoverable key; inspect or rotate it first.
	if !errors.As(err, &apiErr) || apiErr.StatusCode >= 500 {
		m.writeUncertain = true
		m.statusMsg = "The write outcome is unknown. Close this form and refresh keys before trying again."
		return
	}
	m.statusMsg = err.Error()
}

func (m *VkeysViewModel) fetchKeyTeamsCmd() tea.Cmd {
	ctx, client := m.Context(), m.client
	m.teamsLoaded, m.teamsErr = false, nil
	return func() tea.Msg {
		teams, err := client.ListTeamsContext(ctx)
		if ctx.Err() != nil {
			return nil
		}
		return vkeysTeamsMsg{owner: m, teams: teams, err: err}
	}
}

// NavigationBlocked prevents losing a pending credential write.
func (m *VkeysViewModel) NavigationBlocked() bool { return m.pending }

// Cancel releases view work and clears any one-time secret from the model.
func (m *VkeysViewModel) Cancel() {
	m.rawKeyValue, m.rawKeyFlash = "", ""
	m.ViewContext.Cancel()
}

func (m *VkeysViewModel) viewKeyFormOverlay(width, height int, title string) string {
	var popup strings.Builder
	popup.WriteString(m.styles.Title.Render(title) + "\n\n")
	if m.statusMsg != "" {
		popup.WriteString(m.styles.Error.Render(m.statusMsg) + "\n\n")
	}
	field := func(f vkeysFormField, label, value string) {
		prefix := "  "
		if m.formFocus == f {
			prefix = "▸ "
		}
		popup.WriteString(m.styles.Normal.Render(prefix+fmt.Sprintf("%-12s", label+":")) + value + "\n")
	}
	if m.mode == vkeysViewNewKey {
		field(vkeysFieldID, "ID", m.formID.View())
	} else {
		field(-1, "ID", m.formID.Value())
	}
	field(vkeysFieldName, "Name", m.formName.View())
	teamLabel := "Select a team with ←/→"
	switch {
	case !m.teamsLoaded:
		teamLabel = "Loading teams..."
	case m.teamsErr != nil:
		teamLabel = "Could not load teams. Ctrl+O opens the console."
	case m.keyTeam() != nil:
		teamLabel = m.keyTeam().Name + " (" + m.keyTeam().ID + ")"
	case len(m.sharedTeams()) == 0:
		teamLabel = "No active shared teams. Create one with Ctrl+O."
	}
	if m.mode == vkeysViewNewKey {
		field(vkeysFieldTeam, "Team", teamLabel)
	} else {
		field(-1, "Team", m.formTeamID+" (fixed)")
	}
	role := "[user]  admin"
	if m.formRole == 1 {
		role = "user  [admin]"
	}
	field(vkeysFieldRole, "Key role", role)
	if m.formRole == 1 {
		popup.WriteString(m.styles.Help.Render("  Administrators can change cluster policies. Use user for clients.") + "\n")
	}
	field(vkeysFieldExpires, "Expires", m.formExpires.View())
	if team := m.keyTeam(); team != nil {
		popup.WriteString("\n" + m.styles.Help.Render("Inherited team policy") + "\n")
		for _, line := range teamPolicyLines(team) {
			popup.WriteString("  " + line + "\n")
		}
	}
	if m.mode == vkeysViewEditKey {
		popup.WriteString("\n" + m.styles.Help.Render("Additional key limits are read-only and preserved. Issue a new key to change teams.") + "\n")
	}
	innerWidth := max(formOverlayWidth(width)-6, 1)
	content := lipgloss.NewStyle().Width(innerWidth).Render(strings.TrimSuffix(popup.String(), "\n"))
	hints := "Tab next  ←/→ choose  Ctrl+S save\nPgUp/PgDn scroll  Ctrl+O teams  Esc cancel"
	if m.pending {
		hints = "Saving. Please wait..."
	}
	if m.writeUncertain {
		hints = "Esc close, then refresh keys to inspect the result."
	}
	footer := lipgloss.NewStyle().Width(innerWidth).Render(m.styles.Help.Render(hints))
	content = m.formDetail.Render(content, max(height-5-lipgloss.Height(footer), 1)) + "\n" + footer
	return renderFormOverlay(content, m.viewList(width, height), width, height, m.styles)
}

// updateRawKey drives the one-shot secret overlay. Only Copy, Save and an
// explicit Dismiss are honored: the raw key is Argon2id-hashed server
// side and can never be re-read, so a stray keystroke must not close
// the only display of it.
func (m *VkeysViewModel) updateRawKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, tui.VkeysKeys.Copy):
		return m.copyRawKeyCmd()

	case key.Matches(msg, tui.VkeysKeys.SaveKey):
		return m.saveRawKeyCmd()

	case key.Matches(msg, tui.VkeysKeys.Dismiss):
		m.mode = vkeysViewList
		m.rawKeyValue = ""
		m.rawKeyFlash = ""
		m.loading = true
		return m.Init()
	}
	return nil
}

// clipboardWrite is the native OS clipboard write (pbcopy on macOS,
// xclip/xsel/wl-copy on Linux). Indirected for tests, and tried before
// the terminal escape because terminals refuse OSC 52 clipboard writes
// by default — the escape silently does nothing on a stock iTerm2,
// Ghostty or Terminal.app.
var clipboardWrite = clipboard.WriteAll

// copyRawKeyCmd writes the secret to the local OS clipboard. The result
// decides whether the terminal-escape fallback is worth trying, so the
// write happens in a command rather than inline: it forks a helper
// process and must not block the update loop.
func (m *VkeysViewModel) copyRawKeyCmd() tea.Cmd {
	secret := m.rawKeyValue
	if secret == "" {
		return nil
	}
	return func() tea.Msg {
		return vkeysCopiedMsg{err: clipboardWrite(secret)}
	}
}

// rawKeyFileName is the file a saved key lands in. One fixed name, not
// one per key: the file is a hand-off buffer the operator empties, and
// accumulating secrets in a cache directory is the opposite of the goal.
const rawKeyFileName = "last-created-key.txt"

// saveRawKeyCmd writes the secret to a 0600 file. The escape hatch when
// the terminal blocks OSC 52 and mouse selection is unavailable — losing
// an unrecoverable key to a clipboard policy is the worse outcome.
func (m *VkeysViewModel) saveRawKeyCmd() tea.Cmd {
	secret := m.rawKeyValue
	if secret == "" {
		return nil
	}
	return func() tea.Msg {
		base, err := os.UserCacheDir()
		if err != nil {
			return vkeysSavedKeyMsg{err: err}
		}
		dir := filepath.Join(base, "zzrouter")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return vkeysSavedKeyMsg{err: err}
		}
		path := filepath.Join(dir, rawKeyFileName)
		if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
			return vkeysSavedKeyMsg{err: err}
		}
		return vkeysSavedKeyMsg{path: path}
	}
}

// rawKeyBoxWidth is the inner secret panel's target width. Keys are 47
// characters; the panel is sized to hold one on a single line so a mouse
// drag selects the key and nothing else.
const rawKeyBoxWidth = 52

// rawKeyBoxChrome is what the popup adds around that panel: 2 cells of
// padding and 1 border cell on each side.
const rawKeyBoxChrome = 6

func (m *VkeysViewModel) viewRawKeyOverlay(width, height int) string {
	var popup strings.Builder

	popup.WriteString(m.styles.Title.Render("Key Created Successfully") + "\n\n")

	// The secret sits alone in its own frame: it is the payload, and a
	// row containing nothing else is a row the mouse can select cleanly.
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(m.styles.Theme.Overlay).
		Foreground(m.styles.Theme.Accent).
		Bold(true).
		Padding(0, 1).
		Width(min(rawKeyBoxWidth, max(width-12, 20)))
	popup.WriteString(box.Render(m.rawKeyValue) + "\n\n")

	// One status line, never two: the standing warning is replaced by the
	// outcome once the operator acts, so the box does not grow a stack of
	// stale messages.
	if m.rawKeyFlash != "" {
		popup.WriteString(m.styles.Success.Render(m.rawKeyFlash) + "\n")
	} else {
		popup.WriteString(m.styles.Error.Render("This key cannot be shown again.") + "\n")
	}

	popup.WriteString("\n" + m.styles.Help.Render(tui.VkeysKeys.RawKeyHintsString()))

	bg := m.viewList(width, height)
	// Sized to the secret panel plus the popup's own padding and borders,
	// so the dialog frames the key instead of a screenful of empty space.
	boxWidth := min(rawKeyBoxWidth+rawKeyBoxChrome, max(width-4, 24))
	return renderOverlayAtWidth(popup.String(), bg, width, height, boxWidth, m.styles)
}
