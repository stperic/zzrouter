package views

import (
	"fmt"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// vkeysViewMode tracks what the virtual keys view is showing.
type vkeysViewMode int

const (
	vkeysViewList    vkeysViewMode = iota // Table list
	vkeysViewDetail                       // Detail panel for selected key
	vkeysViewNewKey                       // Overlay: create new key
	vkeysViewEditKey                      // Overlay: edit key settings
	vkeysViewShowRaw                      // Overlay: display raw key after create/rotate
)

// vkeysLoadedMsg carries the result of fetching virtual keys.
type vkeysLoadedMsg struct {
	keys []*pkgClient.KeyResponse
	err  error
}

// vkeysUsageMsg carries the result of fetching key usage.
type vkeysUsageMsg struct {
	usage *pkgClient.KeyUsageResponse
	err   error
}

// vkeysDeletedMsg carries the result of deleting a key.
type vkeysDeletedMsg struct {
	err error
}

// vkeysCreatedMsg carries the result of creating a key.
type vkeysCreatedMsg struct {
	owner *VkeysViewModel
	resp  *pkgClient.CreateKeyResponse
	err   error
}

// vkeysRotatedMsg carries the result of rotating a key.
type vkeysRotatedMsg struct {
	resp *pkgClient.CreateKeyResponse
	err  error
}

// vkeysUsageResetMsg carries the result of resetting key usage.
type vkeysUsageResetMsg struct {
	err error
}

// vkeysSuspendedMsg carries the result of toggling a key's suspension state.
// resp carries the updated KeyResponse so the detail view can refresh
// immediately without a full list reload.
type vkeysSuspendedMsg struct {
	resp *pkgClient.KeyResponse
	err  error
}

// vkeysCopiedMsg carries the result of writing the raw key to the local
// OS clipboard. err non-nil means no local clipboard tool answered, and
// the terminal escape path is tried next.
type vkeysCopiedMsg struct{ err error }

// vkeysSavedKeyMsg carries the result of writing the raw key to a file,
// the last resort when neither clipboard path works.
type vkeysSavedKeyMsg struct {
	path string
	err  error
}

// vkeysTeamsMsg carries the server-owned policies for explicit team selection.
type vkeysTeamsMsg struct {
	owner *VkeysViewModel
	teams []*pkgClient.TeamResponse
	err   error
}

type vkeysUpdatedMsg struct {
	owner *VkeysViewModel
	key   *pkgClient.KeyResponse
	err   error
}

// vkeysSpendMsg carries the total spend from the spend report.
type vkeysSpendMsg struct {
	totalSpend float64
}

// vkeysFormField identifies which text field is focused in the key form.
type vkeysFormField int

const (
	vkeysFieldID vkeysFormField = iota
	vkeysFieldName
	vkeysFieldRole
	vkeysFieldTeam
	vkeysFieldExpires
)

// vkeyItem wraps KeyResponse to satisfy tui.Item.
type vkeyItem struct{ *pkgClient.KeyResponse }

func (k vkeyItem) Title() string       { return k.Name }
func (k vkeyItem) Description() string { return k.ID }
func (k vkeyItem) FilterValue() string { return k.Name + " " + k.ID }

type VkeysViewModel struct {
	tui.ViewContext
	help tui.Help
	tui.Draft

	client     *pkgClient.Client
	styles     ui.Styles
	keys       []*pkgClient.KeyResponse
	list       tui.List
	detail     tui.Detail
	confirm    tui.Confirm
	termWidth  int
	termHeight int
	loading    bool
	err        error
	mode       vkeysViewMode
	loadTick   int

	// Detail view
	usage        *pkgClient.KeyUsageResponse
	usageLoading bool
	usageErr     error

	// Spend report
	totalSpend  float64
	spendLoaded bool

	// Confirmation dialog
	confirmPending bool
	confirmAction  string // "delete", "rotate", "reset-usage", "suspend", "unsuspend"

	// Overlay form: new/edit key
	formID         textinput.Model
	formName       textinput.Model
	formRole       int // 0=user, 1=admin
	formExpires    textinput.Model
	formFocus      vkeysFormField
	formDetail     tui.Detail
	formTeamID     string
	formHasExpiry  bool
	teams          []*pkgClient.TeamResponse
	teamsLoaded    bool
	teamsErr       error
	pending        bool
	writeUncertain bool

	// Raw key display
	rawKeyValue string
	rawKeyFlash string // transient feedback under the secret ("key copied…")

	// focusID is set when the view is pushed via NewVkeysViewModelFocused so
	// the load handler can drop the cursor on a specific key and open it
	// in detail mode as soon as the keys list comes back. Cleared after
	// the first successful focus to avoid re-focusing on subsequent refreshes.
	focusID string

	// Status message
	statusMsg string
}

func NewVkeysViewModel(client *pkgClient.Client, styles ui.Styles) *VkeysViewModel {
	list := tui.NewList()
	list.SetKeys(tui.NavigationKeys{
		Up: tui.VkeysKeys.Up, Down: tui.VkeysKeys.Down,
		PageUp: tui.VkeysKeys.PageUp, PageDown: tui.VkeysKeys.PageDown,
		Home: tui.VkeysKeys.Home, End: tui.VkeysKeys.End,
	})
	detail := tui.NewDetail()
	detail.SetKeys(tui.NavigationKeys{
		Up: tui.VkeysKeys.Up, Down: tui.VkeysKeys.Down,
		PageUp: tui.VkeysKeys.PageUp, PageDown: tui.VkeysKeys.PageDown,
		Home: tui.VkeysKeys.Home, End: tui.VkeysKeys.End,
	})
	return &VkeysViewModel{
		client:      client,
		styles:      styles,
		loading:     true,
		list:        list,
		detail:      detail,
		confirm:     tui.NewConfirm(),
		formID:      newFormInput("key-id", 64),
		formName:    newFormInput("human-readable name", 128),
		formExpires: newFormInput("YYYY-MM-DD, blank = never", 64),
		formDetail:  tui.NewDetail(),
	}
}

// NewVkeysViewModelFocused constructs a vkeys view model that auto-opens
// the detail view for the key with the given ID once data loads.
// Used by cross-view navigation (e.g. teams → "V" on a team with members).
func NewVkeysViewModelFocused(client *pkgClient.Client, styles ui.Styles, focusID string) *VkeysViewModel {
	m := NewVkeysViewModel(client, styles)
	m.focusID = focusID
	return m
}

// Refresh reloads keys and inherited policies after a console or team handoff.
func (m *VkeysViewModel) Refresh() tea.Cmd { return tea.Batch(m.fetchKeysCmd(), m.fetchKeyTeamsCmd()) }

func (m *VkeysViewModel) fetchKeysCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		keys, err := client.ListKeys()
		return vkeysLoadedMsg{keys: keys, err: err}
	}
}

// Init implements tea.Model.
func (m *VkeysViewModel) Init() tea.Cmd {
	client := m.client
	cmds := []tea.Cmd{
		m.fetchKeysCmd(),
		shared.SpinnerTickCmd(),
	}
	cmds = append(cmds, m.fetchKeyTeamsCmd())
	if !m.spendLoaded {
		cmds = append(cmds, func() tea.Msg {
			report, err := client.GetSpendReport()
			if err != nil || report == nil {
				return vkeysSpendMsg{totalSpend: 0}
			}
			return vkeysSpendMsg{totalSpend: report.TotalSpend}
		})
	}
	return tea.Batch(cmds...)
}

func (m *VkeysViewModel) selectedKey() *pkgClient.KeyResponse {
	c := m.list.Cursor()
	if c >= 0 && c < len(m.keys) {
		return m.keys[c]
	}
	return nil
}

// Update implements tea.Model.
func (m *VkeysViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	if m.help.Update(msg, m.mode == vkeysViewList || m.mode == vkeysViewDetail) {
		return m, nil
	}

	if handled, cmd := m.Draft.Update(msg); handled {
		return m, cmd
	}

	if m.Context().Err() != nil {
		return m, nil
	}
	if m.pending {
		switch msg.(type) {
		case tea.KeyPressMsg, tea.MouseClickMsg, tea.MouseWheelMsg, tui.BreadcrumbClickMsg:
			return m, nil
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
		vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
		m.list.SetVisible(vis)
		m.detail.SetVisible(vis)
		return m, nil

	case tui.BreadcrumbClickMsg:
		if msg.Level == 0 {
			return m, tui.NavBack()
		}
		m.navigateToBreadcrumb(msg.Level)
		return m, nil

	case tea.MouseClickMsg:
		if m.mode == vkeysViewList || m.mode == vkeysViewDetail {
			return m, m.handleMouseClick(msg)
		}
		return m, nil

	case tea.MouseWheelMsg:
		if m.mode == vkeysViewList {
			m.handleMouseWheel(msg)
		} else if m.mode == vkeysViewDetail {
			m.detail.UpdateWheel(msg)
		}
		return m, nil

	case shared.SpinnerTickMsg:
		if m.loading {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case vkeysTeamsMsg:
		if msg.owner != m || m.Context().Err() != nil {
			return m, nil
		}
		m.teams, m.teamsErr, m.teamsLoaded = msg.teams, msg.err, true
		return m, nil

	case vkeysUpdatedMsg:
		if msg.owner == m && msg.err == nil {
			m.Dirty = false
		}
		if msg.owner != m || m.Context().Err() != nil {
			return m, nil
		}
		m.pending = false
		if msg.err != nil {
			m.keyWriteFailed(msg.err)
			return m, nil
		}
		if msg.key == nil || msg.key.ID != m.formID.Value() {
			m.keyWriteFailed(fmt.Errorf("key response missing identity"))
			return m, nil
		}
		m.mode = vkeysViewDetail
		m.statusMsg = "Key updated. Team policy and additional key limits were preserved."
		for i, k := range m.keys {
			if k.ID == msg.key.ID {
				m.keys[i] = msg.key
				break
			}
		}
		return m, nil

	case vkeysSpendMsg:
		m.totalSpend = msg.totalSpend
		m.spendLoaded = true
		return m, nil

	case vkeysLoadedMsg:
		m.loading = false
		// A failed background reload must not swap populated content for an
		// error page: keep what is on screen and report in the status line.
		if msg.err != nil && len(m.keys) > 0 {
			m.statusMsg = fmt.Sprintf("Refresh failed: %v", msg.err)
			return m, nil
		}
		m.keys = msg.keys
		m.err = msg.err
		items := make([]tui.Item, len(msg.keys))
		for i, k := range msg.keys {
			items[i] = vkeyItem{k}
		}
		m.list.SetItems(items)
		if m.focusID != "" && msg.err == nil {
			for i, k := range m.keys {
				if k.ID == m.focusID {
					m.list.SetCursor(i)
					m.mode = vkeysViewDetail
					m.detail.Reset()
					m.usageLoading = true
					m.usage = nil
					m.usageErr = nil
					client := m.client
					keyID := k.ID
					m.focusID = ""
					return m, func() tea.Msg {
						usage, err := client.GetKeyUsage(keyID)
						return vkeysUsageMsg{usage: usage, err: err}
					}
				}
			}
			// Target not found — don't retry next refresh.
			m.statusMsg = fmt.Sprintf("key %q not found", m.focusID)
			m.focusID = ""
		}
		return m, nil

	case vkeysUsageMsg:
		m.usageLoading = false
		m.usage = msg.usage
		m.usageErr = msg.err
		return m, nil

	case vkeysCreatedMsg:
		if msg.owner == m && msg.err == nil {
			m.Dirty = false
		}
		if msg.owner != nil && msg.owner != m {
			return m, nil
		}
		m.pending = false
		if msg.err != nil {
			m.keyWriteFailed(msg.err)
			return m, nil
		}
		if msg.resp == nil || msg.resp.Data.RawKey == "" {
			m.keyWriteFailed(fmt.Errorf("key response missing secret"))
			return m, nil
		}
		// Show the raw key
		m.rawKeyValue = msg.resp.Data.RawKey
		m.rawKeyFlash = ""
		m.mode = vkeysViewShowRaw
		m.statusMsg = ""
		return m, nil

	case vkeysRotatedMsg:
		m.pending = false
		m.confirmPending = false
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			return m, nil
		}
		m.rawKeyValue = msg.resp.Data.RawKey
		m.rawKeyFlash = ""
		m.mode = vkeysViewShowRaw
		m.statusMsg = ""
		return m, nil

	case vkeysDeletedMsg:
		m.pending = false
		m.confirmPending = false
		m.confirmAction = ""
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.loading = true
		return m, m.Init()

	case vkeysSuspendedMsg:
		m.pending = false
		m.confirmPending = false
		m.confirmAction = ""
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			return m, nil
		}
		// Update the in-memory list entry so the detail/list views reflect
		// the new state without a full refetch.
		if msg.resp != nil {
			for i, k := range m.keys {
				if k.ID == msg.resp.ID {
					m.keys[i] = msg.resp
					break
				}
			}
		}
		m.statusMsg = ""
		return m, nil

	case vkeysUsageResetMsg:
		m.pending = false
		m.confirmPending = false
		m.confirmAction = ""
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			return m, nil
		}
		// Refresh usage
		if k := m.selectedKey(); k != nil {
			m.usageLoading = true
			client := m.client
			keyID := k.ID
			return m, func() tea.Msg {
				usage, err := client.GetKeyUsage(keyID)
				return vkeysUsageMsg{usage: usage, err: err}
			}
		}
		return m, nil

	case vkeysCopiedMsg:
		if msg.err != nil {
			// No pbcopy/xclip/wl-copy reachable — an SSH session into a
			// headless box. Ask the terminal emulator to do it instead;
			// OSC 52 gives no acknowledgement, hence the hedged wording.
			m.rawKeyFlash = "No local clipboard. Asked the terminal (OSC 52): if nothing pastes, press S."
			return m, tea.SetClipboard(m.rawKeyValue)
		}
		m.rawKeyFlash = "Copied to clipboard."
		return m, nil

	case vkeysSavedKeyMsg:
		if msg.err != nil {
			m.rawKeyFlash = "Could not save: " + msg.err.Error()
			return m, nil
		}
		m.rawKeyFlash = "Saved to " + msg.path
		return m, nil

	case keyFormCloseMsg:
		if msg.owner != m || m.pending || m.ConfirmNavigation(msg) {
			return m, nil
		}
		m.closeKeyForm()
		return m, nil
	case tea.KeyPressMsg:
		if m.confirmPending {
			return m, m.updateConfirm(msg)
		}
		switch m.mode {
		case vkeysViewDetail:
			return m, m.updateDetail(msg)
		case vkeysViewNewKey, vkeysViewEditKey:
			return m, m.updateKeyForm(msg)
		case vkeysViewShowRaw:
			return m, m.updateRawKey(msg)
		default:
			return m, m.updateList(msg)
		}
	}

	return m, nil
}
