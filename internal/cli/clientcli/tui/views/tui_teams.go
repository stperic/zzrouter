package views

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// teamsViewMode tracks what the teams view is showing.
type teamsViewMode int

const (
	teamsViewList   teamsViewMode = iota // Table list
	teamsViewDetail                      // Detail panel for selected team
)

// teamsLoadedMsg carries the result of fetching teams.
type teamsLoadedMsg struct {
	teams []*pkgClient.TeamResponse
	err   error
}

// teamsUsageMsg carries the result of fetching team usage.
type teamsUsageMsg struct {
	usage *pkgClient.TeamUsageResponse
	err   error
}

// teamsDeletedMsg carries the result of deleting a team.
type teamsDeletedMsg struct {
	err error
}

// teamsSuspendedMsg carries the result of toggling a team's suspension
// state. team carries the updated TeamResponse so the detail view can
// refresh in place without a full list reload.
type teamsSuspendedMsg struct {
	team *pkgClient.TeamResponse
	err  error
}

// teamsUsageResetMsg carries the result of resetting team usage.
type teamsUsageResetMsg struct {
	err error
}

// teamsKeysMsg carries the available keys for the team roster.
type teamsKeysMsg struct {
	keys []*pkgClient.KeyResponse
}

// teamItem wraps TeamResponse to satisfy tui.Item.
type teamItem struct{ *pkgClient.TeamResponse }

func (t teamItem) Title() string       { return t.Name }
func (t teamItem) Description() string { return t.ID }
func (t teamItem) FilterValue() string { return t.Name + " " + t.ID }

type TeamsViewModel struct {
	tui.ViewContext

	client     *pkgClient.Client
	styles     ui.Styles
	teams      []*pkgClient.TeamResponse
	list       tui.List
	detail     tui.Detail
	confirm    tui.Confirm
	termWidth  int
	termHeight int
	loading    bool
	err        error
	mode       teamsViewMode
	loadTick   int

	// Detail view
	usage        *pkgClient.TeamUsageResponse
	usageLoading bool
	usageErr     error

	// Confirmation dialog
	confirmPending bool
	confirmAction  string // "delete", "reset-usage", "suspend", "unsuspend"

	pending bool

	// Key roster — loaded once via ListKeys, used to render the "Keys in team"
	// section of the detail view (filtered by TeamID client-side) and the
	// list-view member count column.
	availableKeys []*pkgClient.KeyResponse
	keysLoaded    bool

	// focusID is set when the view is pushed via NewTeamsViewModelFocused so
	// the load handler can focus the cursor on a specific team and open it
	// in detail mode. Cleared after the first successful focus.
	focusID string

	// Status message
	statusMsg string
}

func NewTeamsViewModel(client *pkgClient.Client, styles ui.Styles) *TeamsViewModel {
	list := tui.NewList()
	list.SetKeys(tui.NavigationKeys{
		Up: tui.TeamsKeys.Up, Down: tui.TeamsKeys.Down,
		PageUp: tui.TeamsKeys.PageUp, PageDown: tui.TeamsKeys.PageDown,
		Home: tui.TeamsKeys.Home, End: tui.TeamsKeys.End,
	})
	detail := tui.NewDetail()
	detail.SetKeys(tui.NavigationKeys{
		Up: tui.TeamsKeys.Up, Down: tui.TeamsKeys.Down,
		PageUp: tui.TeamsKeys.PageUp, PageDown: tui.TeamsKeys.PageDown,
		Home: tui.TeamsKeys.Home, End: tui.TeamsKeys.End,
	})
	m := &TeamsViewModel{
		client:  client,
		styles:  styles,
		loading: true,
		list:    list,
		detail:  detail,
		confirm: tui.NewConfirm(),
	}

	return m
}

// Refresh reloads policy and membership after a console or key handoff.
func (m *TeamsViewModel) Refresh() tea.Cmd {
	m.keysLoaded = false
	return m.Init()
}

func (m *TeamsViewModel) fetchTeamsCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		teams, err := client.ListTeams()
		return teamsLoadedMsg{teams: teams, err: err}
	}
}

// Init implements tea.Model.
func (m *TeamsViewModel) Init() tea.Cmd {
	client := m.client
	cmds := []tea.Cmd{
		m.fetchTeamsCmd(),
		shared.SpinnerTickCmd(),
	}
	if !m.keysLoaded {
		cmds = append(cmds, func() tea.Msg {
			keys, err := client.ListKeys()
			if err != nil {
				return teamsKeysMsg{keys: nil}
			}
			return teamsKeysMsg{keys: keys}
		})
	}
	return tea.Batch(cmds...)
}

// NewTeamsViewModelFocused constructs a teams view model that auto-opens
// the detail view for the team with the given ID once data loads.
// Used by cross-view navigation (e.g. vkeys → "T" on a key with a team).
func NewTeamsViewModelFocused(client *pkgClient.Client, styles ui.Styles, focusID string) *TeamsViewModel {
	m := NewTeamsViewModel(client, styles)
	m.focusID = focusID
	return m
}

func (m *TeamsViewModel) selectedTeam() *pkgClient.TeamResponse {
	c := m.list.Cursor()
	if c >= 0 && c < len(m.teams) {
		return m.teams[c]
	}
	return nil
}

// Update implements tea.Model.
func (m *TeamsViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
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
		if m.mode == teamsViewList || m.mode == teamsViewDetail {
			return m, m.handleMouseClick(msg)
		}
		return m, nil

	case tea.MouseWheelMsg:
		if m.mode == teamsViewList {
			m.handleMouseWheel(msg)
		} else if m.mode == teamsViewDetail {
			m.detail.UpdateWheel(msg)
		}
		return m, nil

	case shared.SpinnerTickMsg:
		if m.loading {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case teamsKeysMsg:
		m.availableKeys = msg.keys
		m.keysLoaded = true
		return m, nil

	case teamsLoadedMsg:
		m.loading = false
		// A failed background reload must not swap populated content for an
		// error page: keep what is on screen and report in the status line.
		if msg.err != nil && len(m.teams) > 0 {
			m.statusMsg = fmt.Sprintf("Refresh failed: %v", msg.err)
			return m, nil
		}
		m.teams = msg.teams
		m.err = msg.err
		items := make([]tui.Item, len(msg.teams))
		for i, t := range msg.teams {
			items[i] = teamItem{t}
		}
		m.list.SetItems(items)
		if m.focusID != "" && msg.err == nil {
			for i, t := range m.teams {
				if t.ID == m.focusID {
					m.list.SetCursor(i)
					m.mode = teamsViewDetail
					m.detail.Reset()
					m.usageLoading = true
					m.usage = nil
					m.usageErr = nil
					client := m.client
					teamID := t.ID
					m.focusID = ""
					return m, func() tea.Msg {
						usage, err := client.GetTeamUsage(teamID)
						return teamsUsageMsg{usage: usage, err: err}
					}
				}
			}
			m.statusMsg = fmt.Sprintf("team %q not found", m.focusID)
			m.focusID = ""
		}
		return m, nil

	case teamsUsageMsg:
		m.usageLoading = false
		m.usage = msg.usage
		m.usageErr = msg.err
		return m, nil

	case teamsDeletedMsg:
		m.pending = false
		m.confirmPending = false
		m.confirmAction = ""
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.loading = true
		return m, m.Init()

	case teamsSuspendedMsg:
		m.pending = false
		m.confirmPending = false
		m.confirmAction = ""
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			return m, nil
		}
		if msg.team != nil {
			for i, t := range m.teams {
				if t.ID == msg.team.ID {
					m.teams[i] = msg.team
					break
				}
			}
		}
		m.statusMsg = ""
		return m, nil

	case teamsUsageResetMsg:
		m.pending = false
		m.confirmPending = false
		m.confirmAction = ""
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			return m, nil
		}
		if t := m.selectedTeam(); t != nil {
			m.usageLoading = true
			client := m.client
			teamID := t.ID
			return m, func() tea.Msg {
				usage, err := client.GetTeamUsage(teamID)
				return teamsUsageMsg{usage: usage, err: err}
			}
		}
		return m, nil

	case tea.KeyPressMsg:
		if m.confirmPending {
			return m, m.updateConfirm(msg)
		}
		switch m.mode {
		case teamsViewDetail:
			return m, m.updateDetail(msg)
		default:
			return m, m.updateList(msg)
		}
	}

	return m, nil
}

func (m *TeamsViewModel) updateList(msg tea.KeyPressMsg) tea.Cmd {
	// Any keystroke clears the transient status line; handlers below may
	// re-set it (e.g. the delete-blocked-on-members message).
	m.statusMsg = ""

	if m.list.UpdateKey(msg) {
		return nil
	}

	switch {
	case key.Matches(msg, tui.TeamsKeys.Enter):
		if t := m.selectedTeam(); t != nil {
			m.mode = teamsViewDetail
			m.detail.Reset()
			m.usage = nil
			m.usageErr = nil
			m.usageLoading = true
			client := m.client
			teamID := t.ID
			return func() tea.Msg {
				usage, err := client.GetTeamUsage(teamID)
				return teamsUsageMsg{usage: usage, err: err}
			}
		}
		return nil
	case key.Matches(msg, tui.TeamsKeys.New):
		return tui.NavTo(NewConsoleViewModel(m.client, m.styles, pkgClient.ConsoleTarget{Page: "access", Item: "teams", Mode: "create"}))
	case key.Matches(msg, tui.TeamsKeys.Delete):
		if t := m.selectedTeam(); t != nil {
			if n := countTeamKeys(m.availableKeys, t.ID); n > 0 {
				// Server rejects delete on non-empty teams with 409. Short-circuit
				// here so the user sees an actionable message instead of a
				// confirm prompt followed by a cryptic API error.
				m.statusMsg = fmt.Sprintf("Team %q still has %d key(s). Delete their keys first. Membership cannot be changed.", t.ID, n)
				return nil
			}
			m.statusMsg = ""
			m.confirmPending = true
			m.confirmAction = "delete"
		}
		return nil
	case key.Matches(msg, tui.TeamsKeys.Refresh):
		m.loading = true
		return m.Init()
	case key.Matches(msg, tui.TeamsKeys.Back):
		return tui.NavBack()
	case key.Matches(msg, tui.TeamsKeys.Quit):
		return tea.Quit
	}

	return nil
}

func (m *TeamsViewModel) updateDetail(msg tea.KeyPressMsg) tea.Cmd {
	if m.detail.UpdateKey(msg) {
		return nil
	}
	switch {
	case key.Matches(msg, tui.TeamsKeys.Edit):
		if t := m.selectedTeam(); t != nil {
			return tui.NavTo(NewConsoleViewModel(m.client, m.styles, pkgClient.ConsoleTarget{Page: "access", Item: "teams/" + t.ID}))
		}
		return nil
	case key.Matches(msg, tui.TeamsKeys.Suspend):
		if t := m.selectedTeam(); t != nil {
			if t.Kind == "personal" {
				m.statusMsg = "personal teams cannot be suspended directly: suspend their owner key instead"
				return nil
			}
			m.statusMsg = ""
			m.confirmPending = true
			if t.Suspended {
				m.confirmAction = "unsuspend"
			} else {
				m.confirmAction = "suspend"
			}
		}
		return nil
	case key.Matches(msg, tui.TeamsKeys.JumpToKey):
		if t := m.selectedTeam(); t != nil {
			members := keysInTeam(m.availableKeys, t.ID)
			if len(members) == 0 {
				m.statusMsg = fmt.Sprintf("team %q has no keys yet", t.ID)
				return nil
			}
			keyID := members[0].ID
			return tui.NavTo(NewVkeysViewModelFocused(m.client, m.styles, keyID))
		}
		return nil
	case key.Matches(msg, tui.TeamsKeys.ResetUsage):
		if m.selectedTeam() != nil {
			m.confirmPending = true
			m.confirmAction = "reset-usage"
		}
		return nil
	case key.Matches(msg, tui.TeamsKeys.Delete):
		if t := m.selectedTeam(); t != nil {
			if n := countTeamKeys(m.availableKeys, t.ID); n > 0 {
				m.statusMsg = fmt.Sprintf("Team %q still has %d key(s). Delete their keys first. Membership cannot be changed.", t.ID, n)
				return nil
			}
			m.statusMsg = ""
			m.confirmPending = true
			m.confirmAction = "delete"
		}
		return nil
	case key.Matches(msg, tui.TeamsKeys.Back, tui.TeamsKeys.Enter):
		m.mode = teamsViewList
		return nil
	case key.Matches(msg, tui.TeamsKeys.Quit):
		return tea.Quit
	}
	return nil
}

func (m *TeamsViewModel) updateConfirm(msg tea.KeyPressMsg) tea.Cmd {
	switch m.confirm.UpdateKey(msg) {
	case tui.ConfirmYes:
		switch m.confirmAction {
		case "delete":
			if t := m.selectedTeam(); t != nil {
				client := m.client
				teamID := t.ID
				m.confirmPending = false
				m.pending = true
				m.statusMsg = "Saving..."
				return func() tea.Msg {
					err := client.DeleteTeam(teamID)
					return teamsDeletedMsg{err: err}
				}
			}
		case "reset-usage":
			if t := m.selectedTeam(); t != nil {
				client := m.client
				teamID := t.ID
				m.confirmPending = false
				m.pending = true
				m.statusMsg = "Saving..."
				return func() tea.Msg {
					err := client.ResetTeamUsage(teamID)
					return teamsUsageResetMsg{err: err}
				}
			}
		case "suspend", "unsuspend":
			if t := m.selectedTeam(); t != nil {
				client := m.client
				teamID := t.ID
				suspend := m.confirmAction == "suspend"
				m.confirmPending = false
				m.pending = true
				m.statusMsg = "Saving..."
				return func() tea.Msg {
					req := &pkgClient.UpdateTeamRequest{Suspended: &suspend}
					team, err := client.UpdateTeam(teamID, req)
					return teamsSuspendedMsg{team: team, err: err}
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

func (m *TeamsViewModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button == tea.MouseRight {
		if m.mode == teamsViewDetail {
			m.mode = teamsViewList
			m.detail.Reset()
		} else {
			return tui.NavBack()
		}
		return nil
	}
	if m.mode != teamsViewList {
		return nil
	}
	enter, ok := m.list.Click(msg, shared.TuiTableClickY)
	if !ok || !enter {
		return nil
	}
	m.mode = teamsViewDetail
	m.detail.Reset()
	m.usage = nil
	m.usageErr = nil
	m.usageLoading = true
	client := m.client
	teamID := m.teams[m.list.Cursor()].ID
	return func() tea.Msg {
		usage, err := client.GetTeamUsage(teamID)
		return teamsUsageMsg{usage: usage, err: err}
	}
}

func (m *TeamsViewModel) handleMouseWheel(msg tea.MouseWheelMsg) {
	if m.mode == teamsViewList {
		m.list.UpdateWheel(msg)
	}
}

// ============================================================================
// View
// ============================================================================

// Breadcrumb implements tui.Breadcrumber.
func (m *TeamsViewModel) Breadcrumb() []string {
	title := "Teams"
	switch m.mode {
	case teamsViewDetail:
		if t := m.selectedTeam(); t != nil {
			return []string{title, t.Name}
		}
		return []string{title, "Details"}
	default:
		return []string{title}
	}
}

func (m *TeamsViewModel) navigateToBreadcrumb(level int) {
	if level == 1 {
		m.mode = teamsViewList
		m.confirmPending = false
	}
}

// View implements tea.Model.
func (m *TeamsViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
}

func (m *TeamsViewModel) viewContent() string {
	switch m.mode {
	case teamsViewDetail:
		return m.viewDetail(m.termWidth, m.termHeight)
	default:
		return m.viewList(m.termWidth, m.termHeight)
	}
}

func (m *TeamsViewModel) viewList(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.loading,
		LoadTick:  m.loadTick,
		Err:       m.err,
		Empty:     len(m.teams) == 0,
		EmptyText: "No teams configured. Press N to create one in the web console.",
		BackHint:  tui.Hint(tui.ListKeys.Back, "back"),
		EmptyHint: tui.JoinHints(tui.Hint(tui.TeamsKeys.New, "new team"), tui.Hint(tui.ListKeys.Back, "back")),
	}); done {
		return out
	}

	var b strings.Builder
	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))

	allRowStrs := make([][]string, len(m.teams))
	for i, t := range m.teams {
		rpm := shared.FormatRPM(t.RPMLimit)
		budget := shared.FormatBudget(t.SpendLimit, t.ResetPeriod)
		memberCount := fmt.Sprintf("%d", countTeamKeys(m.availableKeys, t.ID))

		status := "active"
		if t.Suspended {
			status = "suspended"
		}

		allRowStrs[i] = []string{t.ID, t.Name, memberCount, rpm, budget, status}
	}

	start, end := m.list.VisibleRange()

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"TEAM ID", "NAME", "MEMBERS", "RPM", "BUDGET", "STATUS"},
		Width:     width,
		Vis:       vis,
		Cursor:    m.list.Cursor(),
		Offset:    m.list.Offset(),
		Styles:    m.styles,
		StatusCol: -1,
		AllRows:   allRowStrs,
	})

	for _, row := range allRowStrs[start:end] {
		t.Row(row...)
	}

	b.WriteString(t.Render() + "\n")

	if m.confirmPending {
		sel := m.selectedTeam()
		if sel != nil {
			b.WriteString(m.styles.Error.Render(fmt.Sprintf("  Delete team %q? (y/n)", sel.ID)) + "\n")
		}
	} else if m.statusMsg != "" {
		b.WriteString(m.styles.Error.Render("  "+m.statusMsg) + "\n")
	}

	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, shared.TableStatus("Teams", len(m.teams), m.list.Offset(), vis), tui.TeamsKeys.ListHintsString()))

	return b.String()
}

// NavigationBlocked keeps the selected team stable until its write completes.
func (m *TeamsViewModel) NavigationBlocked() bool { return m.pending }
