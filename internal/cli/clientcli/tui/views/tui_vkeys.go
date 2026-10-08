package views

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
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
	resp *pkgClient.CreateKeyResponse
	err  error
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

// vkeysGroupsMsg carries available model group names for the allowed models picker.
type vkeysGroupsMsg struct {
	groups []string
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
	vkeysFieldRPM
	vkeysFieldTPM
	vkeysFieldMaxParallel
	vkeysFieldBudgetMax
	vkeysFieldBudgetPeriod
	vkeysFieldExpires
)

var vkeysBudgetPeriods = []string{"", "daily", "weekly", "monthly"}

// vkeyItem wraps KeyResponse to satisfy tui.Item.
type vkeyItem struct{ *pkgClient.KeyResponse }

func (k vkeyItem) Title() string       { return k.Name }
func (k vkeyItem) Description() string { return k.ID }
func (k vkeyItem) FilterValue() string { return k.Name + " " + k.ID }

type VkeysViewModel struct {
	tui.ViewContext

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
	formID           textinput.Model
	formName         textinput.Model
	formRole         int // 0=user, 1=admin
	formRPM          textinput.Model
	formTPM          textinput.Model
	formMaxParallel  textinput.Model
	formBudgetMax    textinput.Model
	formBudgetPeriod int // index into vkeysBudgetPeriods
	formExpires      textinput.Model
	formFocus        vkeysFormField

	// Allowed models multi-select
	availableGroups []string        // model group names from API
	selectedGroups  map[string]bool // toggled model groups
	groupsCursor    int             // cursor within the groups list
	groupsLoaded    bool

	// Metadata key-value pairs
	formMetadata  map[string]string
	formMetaKey   textinput.Model
	formMetaValue textinput.Model

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
		client:          client,
		styles:          styles,
		loading:         true,
		list:            list,
		detail:          detail,
		confirm:         tui.NewConfirm(),
		formID:          newFormInput("key-id", 64),
		formName:        newFormInput("human-readable name", 128),
		formRPM:         newFormInput("blank = unlimited", 16),
		formTPM:         newFormInput("blank = unlimited", 16),
		formMaxParallel: newFormInput("blank = unlimited", 8),
		formBudgetMax:   newFormInput("blank = unlimited", 16),
		formExpires:     newFormInput("YYYY-MM-DD, blank = never", 32),
		selectedGroups:  make(map[string]bool),
		formMetadata:    make(map[string]string),
		formMetaKey:     newFormInput("key", 64),
		formMetaValue:   newFormInput("value", 128),
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

// Refresh implements tui.Refresher: reload keys when a child view (team
// detail) pops, since membership changes there alter what keys report.
// Groups and spend are left alone — they load once and the child can't
// change them.
func (m *VkeysViewModel) Refresh() tea.Cmd { return m.fetchKeysCmd() }

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
	if !m.groupsLoaded {
		cmds = append(cmds, func() tea.Msg {
			groups, _, err := client.ListModelGroups()
			if err != nil {
				return vkeysGroupsMsg{groups: nil}
			}
			names := make([]string, len(groups))
			for i, g := range groups {
				names[i] = g.Name
			}
			return vkeysGroupsMsg{groups: names}
		})
	}
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
		}
		return m, nil

	case shared.SpinnerTickMsg:
		if m.loading {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case vkeysGroupsMsg:
		m.availableGroups = msg.groups
		m.groupsLoaded = true
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
		if m.mode == vkeysViewEditKey {
			m.mode = vkeysViewDetail
		}
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
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			return m, nil
		}
		// Show the raw key
		m.rawKeyValue = msg.resp.Data.RawKey
		m.rawKeyFlash = ""
		m.mode = vkeysViewShowRaw
		m.statusMsg = ""
		return m, nil

	case vkeysRotatedMsg:
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
		m.confirmPending = false
		m.confirmAction = ""
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.loading = true
		return m, m.Init()

	case vkeysSuspendedMsg:
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
		return nil
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
	v := shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
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

	start, end := m.list.VisibleRange()

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"KEY ID", "NAME", "ROLE", "RPM", "BUDGET", "EXPIRES", "STATUS"},
		Width:     width,
		Vis:       vis,
		Cursor:    m.list.Cursor(),
		Offset:    m.list.Offset(),
		Styles:    m.styles,
		StatusCol: 6,
		AllRows:   allRowStrs,
	})

	for _, row := range allRowStrs[start:end] {
		t.Row(row...)
	}

	b.WriteString(t.Render() + "\n")

	if m.confirmPending {
		k := m.selectedKey()
		if k != nil {
			b.WriteString(m.styles.Error.Render(fmt.Sprintf("  Delete key %q? (y/n)", k.ID)) + "\n")
		}
	}

	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, shared.TableStatus("Keys", len(m.keys), m.list.Offset(), vis), tui.VkeysKeys.ListHintsString()))

	return b.String()
}
