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
	"github.com/stperic/zzrouter/pkg/ui/form"
)

// teamsViewMode tracks what the teams view is showing.
type teamsViewMode int

const (
	teamsViewList     teamsViewMode = iota // Table list
	teamsViewDetail                        // Detail panel for selected team
	teamsViewNewTeam                       // Overlay: create new team
	teamsViewEditTeam                      // Overlay: edit team settings
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

// teamsSavedMsg carries the result of create/update.
type teamsSavedMsg struct {
	team *pkgClient.TeamResponse
	err  error
}

// teamsUsageResetMsg carries the result of resetting team usage.
type teamsUsageResetMsg struct {
	err error
}

// teamsGroupsMsg carries available model group names for the allowed models picker.
type teamsGroupsMsg struct {
	groups []string
}

// teamsKeysMsg carries the available virtual keys for the member picker.
type teamsKeysMsg struct {
	keys []*pkgClient.KeyResponse
}

// slugify converts a human name into a URL-safe team ID.
// "My Team 01" → "my-team-01". Collapses runs of non-alphanumerics to
// a single dash and trims leading/trailing dashes.
func slugify(name string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
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

	// Overlay form — built on pkg/ui/form. teamForm owns navigation; the
	// named handles below give saveTeamForm direct access to field values.
	// teamID holds the existing team id in edit mode (not user-editable).
	teamForm      *form.Form
	fName         *form.TextField
	fRPM          *form.TextField
	fTPM          *form.TextField
	fMaxParallel  *form.TextField
	fBudgetMax    *form.TextField
	fBudgetPeriod *form.RadioField
	fModels       *form.ChecklistField
	teamID        string

	groupsLoaded bool

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

	m.fName = form.NewText("Name", "human-readable name", 128)
	m.fRPM = form.NewText("RPM Limit", "blank = unlimited", 16)
	m.fTPM = form.NewText("TPM Limit", "blank = unlimited", 16)
	m.fMaxParallel = form.NewText("Max Parallel", "blank = unlimited", 8)
	m.fBudgetMax = form.NewText("Budget (USD)", "blank = unlimited", 16)
	m.fBudgetPeriod = form.NewRadio(
		"Budget Period",
		[]string{"none", "daily", "weekly", "monthly"},
		[]string{"", "daily", "weekly", "monthly"},
	)
	m.fModels = form.NewChecklist(
		"Allowed Models",
		nil,
		"(no routes configured: all allowed)",
		"all (use Space to restrict)",
	)

	m.teamForm = form.New(
		m.fName,
		m.fRPM,
		m.fTPM,
		m.fMaxParallel,
		m.fBudgetMax,
		m.fBudgetPeriod,
		m.fModels,
	)

	return m
}

// Refresh implements tui.Refresher: reload teams when a child view (key
// detail) pops, since a key's team assignment can change there. Groups and
// keys are left alone — they load once and the child can't change them.
func (m *TeamsViewModel) Refresh() tea.Cmd { return m.fetchTeamsCmd() }

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
	if !m.groupsLoaded {
		cmds = append(cmds, func() tea.Msg {
			groups, _, err := client.ListModelGroups()
			if err != nil {
				return teamsGroupsMsg{groups: nil}
			}
			names := make([]string, len(groups))
			for i, g := range groups {
				names[i] = g.Name
			}
			return teamsGroupsMsg{groups: names}
		})
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
		}
		return m, nil

	case shared.SpinnerTickMsg:
		if m.loading {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case teamsGroupsMsg:
		m.fModels.SetOptions(msg.groups)
		m.groupsLoaded = true
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
		if m.mode == teamsViewEditTeam {
			m.mode = teamsViewDetail
		}
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

	case teamsSavedMsg:
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			return m, nil
		}
		m.statusMsg = ""
		m.mode = teamsViewList
		m.loading = true
		return m, m.Init()

	case teamsDeletedMsg:
		m.confirmPending = false
		m.confirmAction = ""
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.loading = true
		return m, m.Init()

	case teamsSuspendedMsg:
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
		case teamsViewNewTeam, teamsViewEditTeam:
			return m, m.updateTeamForm(msg)
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
		m.openNewTeamForm()
		return nil
	case key.Matches(msg, tui.TeamsKeys.Delete):
		if t := m.selectedTeam(); t != nil {
			if n := countTeamKeys(m.availableKeys, t.ID); n > 0 {
				// Server rejects delete on non-empty teams with 409. Short-circuit
				// here so the user sees an actionable message instead of a
				// confirm prompt followed by a cryptic API error.
				m.statusMsg = fmt.Sprintf("Team %q still has %d key(s). Delete or re-home them first.", t.ID, n)
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
			m.openEditTeamForm(t)
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
				m.statusMsg = fmt.Sprintf("Team %q still has %d key(s). Delete or re-home them first.", t.ID, n)
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
	case teamsViewNewTeam:
		return []string{title, "New Team"}
	case teamsViewEditTeam:
		if t := m.selectedTeam(); t != nil {
			return []string{title, t.Name, "Edit"}
		}
		return []string{title, "Edit"}
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
	case teamsViewNewTeam:
		return m.viewTeamFormOverlay(m.termWidth, m.termHeight, "New Team")
	case teamsViewEditTeam:
		return m.viewTeamFormOverlay(m.termWidth, m.termHeight, "Edit Team")
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
		EmptyText: "No teams configured. Press N to create one.",
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
