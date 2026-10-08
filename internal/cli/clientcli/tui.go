package clientcli

import (
	"fmt"
	"os"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views/chat"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views/search"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
)

// dispatchBreadcrumbClick converts a mouse click on the breadcrumb row
// (Y=2) into a tui.BreadcrumbClickMsg or root NavBack as appropriate.
// Returns the cmd to dispatch (nil if the click missed all ancestor
// segments).
func (m *tuiModel) dispatchBreadcrumbClick(x int) tea.Cmd {
	var segments []string
	switch m.pane {
	case paneSearch:
		segments = m.search.Breadcrumb()
	case paneRoot:
		if b, ok := m.root.Active().(tui.Breadcrumber); ok {
			segments = b.Breadcrumb()
		}
	}
	idx := shared.BreadcrumbClickIndex(segments, x)
	if idx < 0 || idx >= len(segments)-1 {
		return nil
	}
	if idx == 0 {
		// Root segment: leave the current pane back to menu.
		if m.pane == paneSearch {
			m.pane = paneMenu
			return nil
		}
		// paneRoot: pop everything off the stack.
		var cmds []tea.Cmd
		for m.root.Depth() > 0 {
			if cmd := m.popRoot(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return tea.Batch(cmds...)
	}
	// Middle segments: dispatch to the active view as a synthetic msg.
	return func() tea.Msg { return tui.BreadcrumbClickMsg{Level: idx} }
}

type tuiView = tui.View

const (
	tuiViewMenu       tuiView = tui.ViewMenu
	tuiViewSearch     tuiView = tui.ViewSearch
	tuiViewModels     tuiView = tui.ViewModels
	tuiViewNodes      tuiView = tui.ViewNodes
	tuiViewProviders  tuiView = tui.ViewProviders
	tuiViewChat       tuiView = tui.ViewChat
	tuiViewLogs       tuiView = tui.ViewLogs
	tuiViewRunLogs    tuiView = tui.ViewRunLogs
	tuiViewParams     tuiView = tui.ViewParams
	tuiViewVkeys      tuiView = tui.ViewVkeys
	tuiViewTeams      tuiView = tui.ViewTeams
	tuiViewQuickstart tuiView = tui.ViewQuickstart
	tuiViewDeploys    tuiView = tui.ViewDeploys
	tuiViewPricing    tuiView = tui.ViewPricing
	tuiViewUpdate     tuiView = tui.ViewUpdate
)

type menuItem struct {
	name        string
	description string
	view        tuiView
	// Group fields — only used when isGroup is true
	isGroup  bool
	children []menuItem
}

var mainMenuItems = []menuItem{
	{name: "Quick Start", description: "Guided setup: hardware, providers, models", view: tuiViewQuickstart},
	{name: "Model Hub", description: "Search & deploy models from model hub", view: tuiViewSearch},
	{name: "Activity", description: "Running jobs across the cluster: installs, upgrades, downloads", view: tuiViewDeploys},
	{name: "Model Deployments", description: "Deployed models, routes, and running providers", view: tuiViewModels},
	{name: "Providers", description: "Inference local and cloud providers", view: tuiViewProviders},
	{name: "Advanced", description: "Cluster management, keys, and diagnostics", isGroup: true, children: []menuItem{
		{name: "Keys", description: "Virtual API keys, budgets, and rate limits", view: tuiViewVkeys},
		{name: "Teams", description: "Team membership, shared budgets, and rate limits", view: tuiViewTeams},
		{name: "Nodes", description: "Cluster nodes and resources", view: tuiViewNodes},
		{name: "Logs", description: "Inference logs", view: tuiViewLogs},
		{name: "Pricing", description: "Token rates: operator overrides and un-priced models", view: tuiViewPricing},
		{name: "Software Update", description: "This node's zzRouter version: check, install, roll back", view: tuiViewUpdate},
	}},
}

// visibleMenuItems returns the flat list of menu items currently visible,
// accounting for expanded/collapsed groups.
func visibleMenuItems(expanded map[int]bool) []visibleMenuItem {
	var items []visibleMenuItem
	for i, item := range mainMenuItems {
		items = append(items, visibleMenuItem{item: item, topIndex: i, childIndex: -1})
		if item.isGroup && expanded[i] {
			for j, child := range item.children {
				items = append(items, visibleMenuItem{item: child, topIndex: i, childIndex: j})
			}
		}
	}
	return items
}

// visibleMenuItem tracks which top-level and child index a visible row maps to.
type visibleMenuItem struct {
	item       menuItem
	topIndex   int // index in mainMenuItems
	childIndex int // -1 for top-level items, >=0 for children
}

// tuiPane identifies which UI rail is currently active. The host renders
// and routes input differently for each pane.
type tuiPane int

const (
	paneMenu   tuiPane = iota // root menu
	paneSearch                // off-stack singleton (persistent + parent-mutated nodeCount)
	paneRoot                  // tui.Root stack-managed views
)

// tuiModel is the host model. Child views live on tui.Root's stack, except
// for search (sibling singleton — persistent state that the menu hand-off
// must preserve, plus parent-mutated nodeCount). The menu itself isn't a
// view at all; it's the shell the host renders when no other pane owns the
// screen.
type tuiModel struct {
	client       *pkgClient.Client
	styles       ui.Styles
	chatSettings pkgConfig.ChatSettings

	pane         tuiPane
	cursor       int          // menu cursor
	menuExpanded map[int]bool // group indices that are expanded

	root *tui.Root

	// search is a sibling singleton: persistent across nav and receives
	// nodeCount updates from tuiNodesMsg even when not the active pane.
	search            search.SearchTUIModel
	searchInitialized bool

	nodeCount  int
	nodeHealth map[string]string // node name → health status
	termWidth  int
	termHeight int
}

func NewTUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Interactive terminal UI",
		Long:  "Launch an interactive terminal UI for browsing models, nodes, and cluster status.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runTUI()
		},
	}
	return cmd
}

// tuiOption configures the TUI startup.
type tuiOption func(*tuiModel)

// withQuickstart starts the TUI in quickstart wizard mode.
func withQuickstart() tuiOption {
	return func(m *tuiModel) {
		m.pane = paneRoot
		updated, _ := m.root.Update(tui.NavToMsg{View: views.NewQuickstartViewModel(m.styles)})
		m.root, _ = updated.(*tui.Root)
	}
}

func runTUI(opts ...tuiOption) error {
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	// Load chat settings from client config
	var chatSettings pkgConfig.ChatSettings
	cm := pkgConfig.NewConfigManager("zzrouter")
	if cfg, err := cm.LoadClientConfig(); err == nil {
		chatSettings = cfg.Preferences.Chat
	}

	theme := shared.ResolveUITheme()
	styles := ui.NewStyles(theme)

	m := &tuiModel{
		client:       client,
		styles:       styles,
		chatSettings: chatSettings,
		pane:         paneMenu,
		menuExpanded: make(map[int]bool),
		search:       search.NewSearchTUIModel(styles, "", "", "", "", "", true),
		root:         tui.NewEmptyRoot(),
	}

	for _, opt := range opts {
		opt(m)
	}

	p := tea.NewProgram(m)

	// Program exit (clean quit or Ctrl-C) bypasses NavBack, so views
	// on the stack never see their Canceller fire. Tear them down
	// explicitly here so long-running subscriptions (jobstream SSE,
	// log tails) close their HTTP reads before the process exits.
	defer m.root.TeardownAll()

	defer func() {
		fmt.Print("\033[?25h")
		fmt.Print("\033[0m")
		fmt.Print("\033[?1049l")
		fmt.Print("\r\n")
		_ = os.Stdout.Sync()
	}()

	_, err = p.Run()
	return err
}

type tuiNodesMsg []shared.NodeInfo

// Init implements tea.Model.
func (m *tuiModel) Init() tea.Cmd {
	client := m.client
	fetchNodes := func() tea.Msg {
		nodes, err := shared.FetchNodes(client, "", false)
		if err != nil {
			return tuiNodesMsg(nil)
		}
		return tuiNodesMsg(nodes)
	}
	if m.pane == paneRoot && m.root.Active() != nil {
		// withQuickstart pre-pushed a view; run its Init.
		return tea.Batch(fetchNodes, m.root.Active().Init())
	}
	return fetchNodes
}

// Update implements tea.Model.
//
// Update has three responsibilities:
//  1. Cross-cutting messages that affect the whole host (node list, window
//     size, view-switching messages from children).
//  2. Pane-specific routing for input messages.
//  3. Detection of an empty root stack (last view popped) so the host can
//     fall back to the menu.
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// --- View-switching messages from children. These construct the
	// target view and push it onto root, regardless of current pane. ---
	switch msg := msg.(type) {
	// tui.NavToMsg from any source must reach root even if pane just
	// flipped to paneMenu (e.g. NavBack + NavTo in the same tea.Batch).
	// Forward verbatim so OnPop callbacks are preserved.
	case tui.NavToMsg:
		if msg.View == nil {
			return m, nil
		}
		m.pane = paneRoot
		updated, cmd := m.root.Update(msg)
		m.root, _ = updated.(*tui.Root)
		return m, cmd
	case tui.LaunchChatMsg:
		return m, m.enterChat(msg.Model, msg.Node)
	case tui.SwitchToModelsMsg:
		// Leaving search for models: reset search state so re-entry
		// from the menu shows a fresh list, not a stale detail view.
		m.search.State = search.StateList
		m.search.PullStatus = ""
		return m, m.pushOnRoot(m.newModelsView())
	case tui.OpenParamsEditorMsg:
		return m, m.pushOnRoot(views.NewParamsViewModel(m.client, m.styles, msg.Provider, msg.Model))
	case tui.LaunchRunLogsMsg:
		return m, m.pushOnRoot(views.NewRunlogsViewModel(m.client, m.styles, msg.Filter))

	// --- Quickstart hand-off. The wizard launches sibling views that
	// must report completion when popped. Use NavToWithPop. ---
	case tui.QsLaunchViewMsg:
		view := m.viewForKind(msg.View)
		if view == nil {
			return m, nil
		}
		step := msg.Step
		return m, tui.NavToWithPop(view, func() tea.Cmd {
			return func() tea.Msg { return tui.QsStepDoneMsg{Step: step} }
		})
	case tui.QsEnterMenuMsg:
		// Quickstart asked to exit to menu. Pop everything off root.
		var cmds []tea.Cmd
		for m.root.Depth() > 0 {
			if cmd := m.popRoot(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		m.pane = paneMenu
		return m, tea.Batch(cmds...)

	// --- Search-only signal. Search is off-stack, so it emits the
	// classic tuiBackMsg; host catches it and switches pane. ---
	case tui.TuiBackMsg:
		if m.pane == paneSearch {
			m.pane = paneMenu
		}
		return m, nil

	// --- Cluster nodes refresh. nodeCount/nodeHealth feed several
	// child views, plus the off-stack search singleton. ---
	case tuiNodesMsg:
		if len(msg) == 0 {
			m.nodeCount = 1
		} else {
			m.nodeCount = len(msg)
		}
		m.search.NodeCount = m.nodeCount
		m.nodeHealth = shared.BuildNodeHealthMap(msg)
		return m, nil

	// --- Window resize. Search is informed even when not active so it
	// can size its viewport for re-entry. ---
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height
		m.search.TermWidth = msg.Width
		m.search.TermHeight = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
		// Forward to both search and root regardless of active pane so
		// inactive-pane views have a known size cached for activation.
		// Without this, a view pushed from paneMenu (e.g. chat launched
		// from the menu) misses its first WindowSizeMsg and renders
		// stuck on its "not ready" placeholder until the terminal is
		// resized.
		updated, searchCmd := m.search.Update(msg)
		m.search = updated.(search.SearchTUIModel) //nolint:errcheck // bubbletea Update contract always returns same model type
		// Forward directly to root (not via forwardToRoot) so an empty
		// root stack doesn't flip the active pane back to paneMenu —
		// paneSearch uses the search singleton while root is empty.
		rootUpdated, rootCmd := m.root.Update(msg)
		m.root = rootUpdated.(*tui.Root) //nolint:errcheck // bubbletea Update contract always returns same model type
		return m, tea.Batch(searchCmd, rootCmd)

	// --- Mouse breadcrumb click. The host computes the level and
	// dispatches a synthetic BreadcrumbClickMsg into the active pane.
	// Chat (no breadcrumb) is excluded by the pane check. ---
	case tea.MouseClickMsg:
		if m.pane == paneMenu {
			return m, m.menuMouseClick(msg)
		}
		if msg.Button == tea.MouseLeft && msg.Y == 2 && !m.activeIsChat() {
			if cmd := m.dispatchBreadcrumbClick(msg.X); cmd != nil {
				return m, cmd
			}
		}
		return m, m.forwardToActive(msg)

	case tea.MouseWheelMsg:
		if m.pane == paneMenu {
			return m, m.menuMouseWheel(msg)
		}
		return m, m.forwardToActive(msg)

	case tea.KeyPressMsg:
		if m.pane == paneMenu {
			return m, m.updateMenu(msg)
		}
		return m, m.forwardToActive(msg)
	}

	// --- Default: forward to active pane (data load messages, etc). ---
	if m.pane == paneMenu {
		return m, nil
	}
	return m, m.forwardToActive(msg)
}

// forwardToActive routes a message to whichever pane is currently active
// (search singleton or root stack). Returns the cmd to dispatch.
func (m *tuiModel) forwardToActive(msg tea.Msg) tea.Cmd {
	switch m.pane {
	case paneSearch:
		updated, cmd := m.search.Update(msg)
		m.search = updated.(search.SearchTUIModel) //nolint:errcheck // bubbletea Update contract always returns same model type
		return cmd
	case paneRoot:
		return m.forwardToRoot(msg)
	}
	return nil
}

// forwardToRoot dispatches a message into root and detects when the stack
// becomes empty (last view popped) so the host can fall back to the menu.
func (m *tuiModel) forwardToRoot(msg tea.Msg) tea.Cmd {
	updated, cmd := m.root.Update(msg)
	m.root = updated.(*tui.Root) //nolint:errcheck // bubbletea Update contract always returns same model type
	if m.root.Depth() == 0 {
		m.pane = paneMenu
	}
	return cmd
}

// pushOnRoot pushes a view onto the root stack and switches the pane.
func (m *tuiModel) pushOnRoot(view tea.Model) tea.Cmd {
	if view == nil {
		return nil
	}
	m.pane = paneRoot
	updated, cmd := m.root.Update(tui.NavToMsg{View: view})
	m.root = updated.(*tui.Root) //nolint:errcheck // bubbletea Update contract always returns same model type
	return cmd
}

// popRoot pops the top frame off root and returns its cmd. Used by the
// breadcrumb-root-click and qsEnterMenuMsg handlers that need to drain
// the stack.
func (m *tuiModel) popRoot() tea.Cmd {
	updated, cmd := m.root.Update(tui.NavBackMsg{})
	m.root = updated.(*tui.Root) //nolint:errcheck // bubbletea Update contract always returns same model type
	if m.root.Depth() == 0 {
		m.pane = paneMenu
	}
	return cmd
}

// activeIsChat reports whether the active root view is the chat view (no
// breadcrumb chrome — host shouldn't try to handle breadcrumb clicks).
func (m *tuiModel) activeIsChat() bool {
	if m.pane != paneRoot {
		return false
	}
	_, ok := m.root.Active().(*chat.ChatModel)
	return ok
}

// enterChat constructs the chat view and pushes it onto root.
func (m *tuiModel) enterChat(modelName string, preferredNode ...string) tea.Cmd {
	cs := m.chatSettings
	chat := chat.NewChatModel(m.client, modelName, shared.NodeAddress(), cs.SystemPrompt, cs.Temperature, cs.TopP, cs.MaxTokens, cs.Thinking, m.styles)
	chat.SetShowStats(cs.Verbose)
	chat.Embedded = true
	chat.NodeCount = m.nodeCount
	if len(preferredNode) > 0 && preferredNode[0] != "" {
		chat.PreferredNode = preferredNode[0]
	}
	return m.pushOnRoot(chat)
}

// viewForKind constructs a view by tuiView identifier. Used by quickstart's
// step-launch flow (tui.QsLaunchViewMsg carries a tuiView).
func (m *tuiModel) viewForKind(view tuiView) tea.Model {
	switch view {
	case tuiViewSearch:
		// Search is off-stack — quickstart-launched search runs in paneSearch.
		// Returning nil here signals the caller to use the search rail directly.
		// Quickstart doesn't currently launch search, but kept for completeness.
		return nil
	case tuiViewModels:
		return m.newModelsView()
	case tuiViewNodes:
		return views.NewNodesViewModel(m.client, m.styles)
	case tuiViewProviders:
		return views.NewProvidersViewModel(m.client, m.styles)
	case tuiViewLogs:
		return views.NewLogsViewModel(m.client, m.styles)
	case tuiViewVkeys:
		return views.NewVkeysViewModel(m.client, m.styles)
	case tuiViewTeams:
		return views.NewTeamsViewModel(m.client, m.styles)
	case tuiViewQuickstart:
		return views.NewQuickstartViewModel(m.styles)
	case tuiViewDeploys:
		return views.NewDeploysViewModel(m.client, m.styles)
	case tuiViewPricing:
		return views.NewPricingViewModel(m.client, m.styles)
	case tuiViewUpdate:
		return views.NewUpdateViewModel(m.client, m.styles)
	}
	return nil
}

// newModelsView constructs a fresh models view with current nodeHealth.
func (m *tuiModel) newModelsView() *views.ModelsViewModel {
	v := views.NewModelsViewModel(m.client, m.styles)
	v.NodeHealth = m.nodeHealth
	return v
}

// toggleMenuGroup toggles a group's expanded state and clamps the cursor.
func (m *tuiModel) toggleMenuGroup(topIndex int) {
	m.menuExpanded[topIndex] = !m.menuExpanded[topIndex]
	if !m.menuExpanded[topIndex] {
		visible := visibleMenuItems(m.menuExpanded)
		if m.cursor >= len(visible) {
			m.cursor = len(visible) - 1
		}
	}
}

// menuActivate handles Enter/click on the item at the given visible index.
// Returns the cmd to dispatch (nil for group toggles or out-of-range).
func (m *tuiModel) menuActivate(visible []visibleMenuItem, idx int) tea.Cmd {
	if idx < 0 || idx >= len(visible) {
		return nil
	}
	vi := visible[idx]
	if vi.item.isGroup {
		m.toggleMenuGroup(vi.topIndex)
		return nil
	}
	return m.enterMenuItem(vi.item.view)
}

// enterMenuItem switches to the appropriate pane for a menu selection and
// pushes the view (or activates the singleton).
func (m *tuiModel) enterMenuItem(view tuiView) tea.Cmd {
	if view == tuiViewSearch {
		// Search is off-stack — switch pane and ensure it's initialized.
		m.pane = paneSearch
		sizeCmd := func() tea.Msg {
			return tea.WindowSizeMsg{Width: m.termWidth, Height: m.termHeight}
		}
		if !m.searchInitialized {
			m.searchInitialized = true
			return tea.Batch(m.search.Init(), sizeCmd)
		}
		// Reset to list view when re-entering from menu (don't show stale detail).
		m.search.State = search.StateList
		m.search.PullStatus = ""
		return sizeCmd
	}
	// All other views go on the root stack.
	target := m.viewForKind(view)
	if target == nil {
		return nil
	}
	return m.pushOnRoot(target)
}

func (m *tuiModel) updateMenu(msg tea.KeyPressMsg) tea.Cmd {
	visible := visibleMenuItems(m.menuExpanded)
	switch {
	case key.Matches(msg, tui.MenuKeys.Quit):
		return tea.Quit
	case key.Matches(msg, tui.MenuKeys.Up):
		if m.cursor > 0 {
			m.cursor--
		}
	case key.Matches(msg, tui.MenuKeys.Down):
		if m.cursor < len(visible)-1 {
			m.cursor++
		}
	case key.Matches(msg, tui.MenuKeys.Enter):
		return m.menuActivate(visible, m.cursor)
	}
	return nil
}

func (m *tuiModel) menuMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button != tea.MouseLeft {
		return nil
	}
	visible := visibleMenuItems(m.menuExpanded)
	// Menu items start at row 4 (title bar(2) + blank(1) + first item at row 3).
	// Each item takes 2 lines (name + description).
	row := msg.Y - 3
	if row < 0 {
		return nil
	}
	idx := row / 2
	if idx >= 0 && idx < len(visible) {
		if m.cursor == idx {
			return m.menuActivate(visible, idx)
		}
		m.cursor = idx
	}
	return nil
}

func (m *tuiModel) menuMouseWheel(msg tea.MouseWheelMsg) tea.Cmd { //nolint:unparam // Mouse dispatch returns tea.Cmd even when scrolling only updates the cursor.
	visible := visibleMenuItems(m.menuExpanded)
	switch msg.Button {
	case tea.MouseWheelUp:
		if m.cursor > 0 {
			m.cursor--
		}
	case tea.MouseWheelDown:
		if m.cursor < len(visible)-1 {
			m.cursor++
		}
	}
	return nil
}

// tuiMinWidth and tuiMinHeight are the minimum terminal dimensions for usable rendering.
const (
	tuiMinWidth  = 40
	tuiMinHeight = 10
)

// View implements tea.Model. The host renders the menu when paneMenu is
// active; otherwise it delegates to the active pane (search singleton or
// the root stack's top frame). Views render their own chrome — the host
// only adds the terminal-too-small fallback.
func (m *tuiModel) View() tea.View {
	if m.termWidth > 0 && m.termHeight > 0 && (m.termWidth < tuiMinWidth || m.termHeight < tuiMinHeight) {
		msg := fmt.Sprintf("Terminal too small (%dx%d). Minimum: %dx%d",
			m.termWidth, m.termHeight, tuiMinWidth, tuiMinHeight)
		return shared.NewAltScreenView(m.styles.Help.Render(msg), m.styles.Theme)
	}
	switch m.pane {
	case paneSearch:
		return shared.RenderChildView(m.styles, m.termWidth, m.search.Breadcrumb(), m.search.ViewContent())
	case paneRoot:
		if active := m.root.Active(); active != nil {
			return active.View()
		}
	}
	return m.renderMenu()
}

func (m *tuiModel) renderMenu() tea.View {
	var b strings.Builder

	b.WriteString(shared.RenderTitleBar(m.styles, m.termWidth))
	b.WriteString("\n\n")

	visible := visibleMenuItems(m.menuExpanded)
	for i, vi := range visible {
		isChild := vi.childIndex >= 0
		selected := i == m.cursor

		// Build the prefix: cursor marker + indent level
		var cursor string
		switch {
		case selected && isChild:
			cursor = "  > "
		case selected:
			cursor = "> "
		case isChild:
			cursor = "    "
		default:
			cursor = "  "
		}

		// Group header: show expand/collapse indicator
		label := vi.item.name
		if vi.item.isGroup {
			if m.menuExpanded[vi.topIndex] {
				label = "▼ " + label
			} else {
				label = "▶ " + label
			}
		}

		if selected {
			b.WriteString(m.styles.Selected.Render(cursor + label))
		} else {
			b.WriteString(m.styles.Normal.Render(cursor + label))
		}
		b.WriteString("\n")

		// Description line
		descIndent := "    "
		if isChild {
			descIndent = "      "
		}
		b.WriteString(descIndent)
		b.WriteString(m.styles.Help.Render(vi.item.description))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(shared.RenderViewFooter(m.styles, m.termWidth, tui.MenuKeys.HintsString()))
	b.WriteString("\n")

	v := shared.NewAltScreenView(b.String(), m.styles.Theme)
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
