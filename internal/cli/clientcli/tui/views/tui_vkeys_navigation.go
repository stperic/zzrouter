package views

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

func (m *VkeysViewModel) updateList(msg tea.KeyPressMsg) tea.Cmd {
	if m.list.UpdateKey(msg) {
		return nil
	}

	switch {
	case key.Matches(msg, tui.VkeysKeys.Enter):
		if k := m.selectedKey(); k != nil {
			m.mode = vkeysViewDetail
			m.detail.Reset()
			m.usage = nil
			m.usageErr = nil
			m.usageLoading = true
			client := m.client
			keyID := k.ID
			return func() tea.Msg {
				usage, err := client.GetKeyUsage(keyID)
				return vkeysUsageMsg{usage: usage, err: err}
			}
		}
		return nil
	case key.Matches(msg, tui.VkeysKeys.New):
		m.openNewKeyForm()
		return m.fetchKeyTeamsCmd()
	case key.Matches(msg, tui.VkeysKeys.Delete):
		if m.selectedKey() != nil {
			m.confirmPending = true
			m.confirmAction = "delete"
		}
		return nil
	case key.Matches(msg, tui.VkeysKeys.Refresh):
		m.loading = true
		return m.Init()
	case key.Matches(msg, tui.VkeysKeys.Back):
		return tui.NavBack()
	case key.Matches(msg, tui.VkeysKeys.Quit):
		return tea.Quit
	}

	return nil
}

func (m *VkeysViewModel) updateDetail(msg tea.KeyPressMsg) tea.Cmd {
	if m.detail.UpdateKey(msg) {
		return nil
	}
	switch {
	case key.Matches(msg, tui.VkeysKeys.Edit):
		if k := m.selectedKey(); k != nil {
			m.openEditKeyForm(k)
		}
		return nil
	case key.Matches(msg, tui.VkeysKeys.Rotate):
		if m.selectedKey() != nil {
			m.confirmPending = true
			m.confirmAction = "rotate"
		}
		return nil
	case key.Matches(msg, tui.VkeysKeys.Suspend):
		// Toggle: if currently suspended, prompt to unsuspend; else suspend.
		if k := m.selectedKey(); k != nil {
			m.confirmPending = true
			if k.Suspended {
				m.confirmAction = "unsuspend"
			} else {
				m.confirmAction = "suspend"
			}
		}
		return nil
	case key.Matches(msg, tui.VkeysKeys.JumpToTeam):
		if k := m.selectedKey(); k != nil && k.TeamID != "" {
			teamID := k.TeamID
			return tui.NavTo(NewTeamsViewModelFocused(m.client, m.styles, teamID))
		}
		return nil
	case key.Matches(msg, tui.VkeysKeys.ResetUsage):
		if m.selectedKey() != nil {
			m.confirmPending = true
			m.confirmAction = "reset-usage"
		}
		return nil
	case key.Matches(msg, tui.VkeysKeys.Delete):
		if m.selectedKey() != nil {
			m.confirmPending = true
			m.confirmAction = "delete"
		}
		return nil
	case key.Matches(msg, tui.VkeysKeys.Back, tui.VkeysKeys.Enter):
		m.mode = vkeysViewList
		return nil
	case key.Matches(msg, tui.VkeysKeys.Quit):
		return tea.Quit
	}
	return nil
}

func (m *VkeysViewModel) updateConfirm(msg tea.KeyPressMsg) tea.Cmd {
	switch m.confirm.UpdateKey(msg) {
	case tui.ConfirmYes:
		switch m.confirmAction {
		case "delete":
			if k := m.selectedKey(); k != nil {
				client := m.client
				keyID := k.ID
				m.confirmPending = false
				m.pending = true
				m.statusMsg = "Saving..."
				return func() tea.Msg {
					err := client.DeleteKey(keyID)
					return vkeysDeletedMsg{err: err}
				}
			}
		case "rotate":
			if k := m.selectedKey(); k != nil {
				client := m.client
				keyID := k.ID
				m.confirmPending = false
				m.pending = true
				m.statusMsg = "Saving..."
				return func() tea.Msg {
					resp, err := client.RotateKey(keyID)
					return vkeysRotatedMsg{resp: resp, err: err}
				}
			}
		case "reset-usage":
			if k := m.selectedKey(); k != nil {
				client := m.client
				keyID := k.ID
				m.confirmPending = false
				m.pending = true
				m.statusMsg = "Saving..."
				return func() tea.Msg {
					err := client.ResetKeyUsage(keyID)
					return vkeysUsageResetMsg{err: err}
				}
			}
		case "suspend", "unsuspend":
			if k := m.selectedKey(); k != nil {
				client := m.client
				keyID := k.ID
				suspend := m.confirmAction == "suspend"
				m.confirmPending = false
				m.pending = true
				m.statusMsg = "Saving..."
				return func() tea.Msg {
					req := &pkgClient.UpdateKeyRequest{Suspended: &suspend}
					resp, err := client.UpdateKey(keyID, req)
					return vkeysSuspendedMsg{resp: resp, err: err}
				}
			}
		}
		m.confirmPending = false
		return nil
	case tui.ConfirmNo, tui.ConfirmCancelled:
		m.confirmPending = false
		return nil
	}
	return nil
}

func (m *VkeysViewModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button == tea.MouseRight {
		if m.mode == vkeysViewDetail {
			m.mode = vkeysViewList
			m.detail.Reset()
		} else {
			return tui.NavBack()
		}
		return nil
	}
	if m.mode != vkeysViewList {
		return nil
	}
	enter, ok := m.list.Click(msg, shared.TuiTableClickY)
	if !ok || !enter {
		return nil
	}
	m.mode = vkeysViewDetail
	m.detail.Reset()
	m.usage = nil
	m.usageErr = nil
	m.usageLoading = true
	client := m.client
	keyID := m.keys[m.list.Cursor()].ID
	return func() tea.Msg {
		usage, err := client.GetKeyUsage(keyID)
		return vkeysUsageMsg{usage: usage, err: err}
	}
}

func (m *VkeysViewModel) handleMouseWheel(msg tea.MouseWheelMsg) {
	if m.mode == vkeysViewList {
		m.list.UpdateWheel(msg)
	}
}

// ============================================================================
// View
// ============================================================================

// Breadcrumb implements tui.Breadcrumber.
func (m *VkeysViewModel) Breadcrumb() []string {
	title := "Keys"
	if m.totalSpend > 0 {
		title = fmt.Sprintf("Keys: $%.2f", m.totalSpend)
	}
	switch m.mode {
	case vkeysViewDetail:
		if k := m.selectedKey(); k != nil {
			return []string{title, k.Name}
		}
		return []string{title, "Details"}
	case vkeysViewNewKey:
		return []string{title, "New Key"}
	case vkeysViewEditKey:
		if k := m.selectedKey(); k != nil {
			return []string{title, k.Name, "Edit"}
		}
		return []string{title, "Edit"}
	case vkeysViewShowRaw:
		return []string{title, "API Key"}
	default:
		return []string{title}
	}
}

// navigateToBreadcrumb navigates to the state matching breadcrumb level.
func (m *VkeysViewModel) navigateToBreadcrumb(level int) {
	if level == 1 {
		m.mode = vkeysViewList
		m.confirmPending = false
	}
}

// View implements tea.Model.
func (m *VkeysViewModel) View() tea.View {
	hints := tui.VkeysKeys.ListHintsString()
	if m.mode == vkeysViewDetail {
		hints = tui.VkeysKeys.DetailHintsString()
	}
	v := shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.Draft.View(m.styles, m.help.View(m.styles, m.viewContent(), m.termWidth, m.termHeight, hints), m.termWidth, m.termHeight))
	if m.mode == vkeysViewShowRaw {
		// Hand the mouse back to the terminal while the one-shot secret is
		// on screen. Under cell-motion capture the emulator routes drags to
		// us instead of selecting text, which removes the very fallback the
		// overlay tells the user to fall back to.
		v.MouseMode = tea.MouseModeNone
	}
	return v
}

func (m *VkeysViewModel) viewContent() string {
	width := m.termWidth
	height := m.termHeight
	switch m.mode {
	case vkeysViewDetail:
		return m.viewDetail(width, height)
	case vkeysViewNewKey:
		return m.viewKeyFormOverlay(width, height, "New Key")
	case vkeysViewEditKey:
		return m.viewKeyFormOverlay(width, height, "Edit Key")
	case vkeysViewShowRaw:
		return m.viewRawKeyOverlay(width, height)
	default:
		return m.viewList(width, height)
	}
}

func (m *VkeysViewModel) viewList(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.loading,
		LoadTick:  m.loadTick,
		Err:       m.err,
		Empty:     len(m.keys) == 0,
		EmptyText: "No keys configured. Press N to create one.",
		BackHint:  tui.Hint(tui.ListKeys.Back, "back"),
		EmptyHint: tui.JoinHints(tui.Hint(tui.VkeysKeys.New, "new key"), tui.Hint(tui.ListKeys.Back, "back")),
	}); done {
		return out
	}

	var b strings.Builder
	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))

	allRowStrs := make([][]string, len(m.keys))
	for i, k := range m.keys {
		rpm := shared.FormatRPM(k.RPMLimit)
		budget := shared.FormatBudget(k.SpendLimit, k.ResetPeriod)

		expires := shared.EmptyValue
		if k.ExpiresAt != nil {
			if len(*k.ExpiresAt) >= 10 {
				expires = (*k.ExpiresAt)[:10]
			} else {
				expires = *k.ExpiresAt
			}
		}
		if k.IsExpired {
			expires += " (expired)"
		}

		status := "active"
		if k.Suspended {
			status = "suspended"
		}

		allRowStrs[i] = []string{k.ID, k.Name, k.Role, rpm, budget, expires, status}
	}

	m.list.SetVisible(vis)
	start, end := m.list.VisibleRange()

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:        []string{"KEY ID", "NAME", "ROLE", "RPM", "BUDGET", "EXPIRES", "STATUS"},
		ColumnPriority: []int{100, 80, 40, 10, 30, 20, 90},
		ColWidths:      []int{16, 16, 8, 8, 12, 12, 10},
		Width:          width,
		Vis:            vis,
		Cursor:         m.list.Cursor(),
		Offset:         m.list.Offset(),
		Styles:         m.styles,
		StatusCol:      6,
		AllRows:        allRowStrs,
	})

	for _, row := range allRowStrs[start:end] {
		t.Row(row...)
	}

	b.WriteString(t.Render() + "\n")

	status := shared.TableStatus("Keys", len(m.keys), m.list.Offset(), vis)
	hints := tui.VkeysKeys.ListHintsString() + "  ? help"
	if m.confirmPending {
		if k := m.selectedKey(); k != nil {
			status = "Delete key " + k.ID + "?"
			hints = "Y delete  Enter/Esc cancel"
		}
	}
	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, status, hints))

	return b.String()
}
