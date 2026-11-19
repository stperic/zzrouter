package clientcli

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/version"
)

const (
	menuLogoFrames   = 8
	menuLogoInterval = 75 * time.Millisecond
)

type menuLogoTickMsg struct{}

func menuLogoTick() tea.Cmd {
	return tea.Tick(menuLogoInterval, func(time.Time) tea.Msg { return menuLogoTickMsg{} })
}

type menuItem struct {
	name        string
	description string
	view        tuiView
	isGroup     bool
	children    []menuItem
}

var mainMenuItems = []menuItem{
	{name: "Models", description: "Available models, running instances, and discovery", isGroup: true, children: []menuItem{
		{name: "Available", description: "Configured models, downloads, and routes", view: tuiViewModels},
		{name: "Running", description: "Currently running model and provider instances", view: tuiViewRunningModels},
		{name: "Discover", description: "Find models in registries and cloud catalogs", view: tuiViewSearch},
	}},
	{name: "Providers", description: "Local engines, cloud connections, and registry credentials", view: tuiViewProviders},
	{name: "Activity", description: "Installs, upgrades, downloads, and other jobs", view: tuiViewDeploys},
	{name: "Nodes", description: "Cluster health and resources", view: tuiViewNodes},
	{name: "Open web console", description: "Full cluster management in your browser", view: tuiViewConsole},
	{name: "Manage", description: "Advanced administration and diagnostics", isGroup: true, children: []menuItem{
		{name: "Keys", description: "Issue team keys and manage credentials", view: tuiViewVkeys},
		{name: "Teams", description: "Inspect policies, edit access and budgets in the console", view: tuiViewTeams},
		{name: "Logs", description: "Inference logs", view: tuiViewLogs},
		{name: "Pricing", description: "Token rates and unpriced models", view: tuiViewPricing},
		{name: "Software Update", description: "This node's version, install, and rollback", view: tuiViewUpdate},
		{name: "Guided Setup", description: "Set up hardware, providers, and models", view: tuiViewQuickstart},
	}},
}

// visibleMenuItem identifies a row independently of expanded groups before it.
type visibleMenuItem struct {
	item       menuItem
	topIndex   int
	childIndex int // -1 for a top-level item
}

func (i visibleMenuItem) Title() string       { return i.item.name }
func (i visibleMenuItem) Description() string { return i.item.description }
func (i visibleMenuItem) FilterValue() string { return i.item.name }

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

func (m *tuiModel) menuHeader() string {
	// The banner, six two-line menu items and footer fit a 24-line terminal.
	if m.menuWelcome && m.termHeight >= 24 {
		label := m.styles.Normal.Bold(true).Render("Router") + "  " +
			m.styles.HintsBar.Render("v"+version.Current.ShortString()) + "\n" +
			m.styles.HintsBar.Render("Local + cloud AI")
		welcome := lipgloss.JoinHorizontal(lipgloss.Center, m.menuMark(), "   ", label)
		if m.termWidth >= lipgloss.Width(welcome)+2 {
			return "\n" + lipgloss.NewStyle().PaddingLeft(1).Render(welcome) + "\n" +
				shared.Hrule(m.styles, m.termWidth) + "\n\n"
		}
	}
	return shared.RenderTitleBar(m.styles, m.termWidth) + "\n\n"
}

func (m *tuiModel) menuMark() string {
	mark := ui.BrandMark(m.styles)
	if m.menuLogoFrame == 0 || m.menuLogoFrame >= menuLogoFrames {
		return mark
	}
	width := lipgloss.Width(mark)
	columns := (width*m.menuLogoFrame + menuLogoFrames - 1) / menuLogoFrames
	rows := strings.Split(mark, "\n")
	for i, row := range rows {
		rows[i] = ansi.Truncate(row, columns, "")
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(rows, "\n"))
}

func (m *tuiModel) menuFooter() string {
	width := m.termWidth
	if width <= 0 {
		width = 80
	}
	hints := shared.TruncateText(tui.MenuKeys.HintsString(), width-1)
	return "\n" + shared.RenderViewFooter(m.styles, width, hints)
}

func (m *tuiModel) syncMenu() []visibleMenuItem {
	visible := visibleMenuItems(m.menuExpanded)
	items := make([]tui.Item, len(visible))
	for i, item := range visible {
		items[i] = item
	}
	m.menu.SetKeys(tui.DefaultNavigationKeys())
	m.menu.SetItems(items)
	height := m.termHeight
	if height <= 0 {
		height = 24
	}
	chrome := strings.Count(m.menuHeader(), "\n") + lipgloss.Height(m.menuFooter())
	m.menu.SetVisible(max((height-chrome)/2, 1))
	m.menu.SetCursor(m.menu.Cursor())
	return visible
}

func (m *tuiModel) toggleMenuGroup(topIndex int) {
	if m.menuExpanded == nil {
		m.menuExpanded = make(map[int]bool)
	}
	m.menuExpanded[topIndex] = !m.menuExpanded[topIndex]
	for i, item := range m.syncMenu() {
		if item.topIndex == topIndex && item.childIndex == -1 {
			m.menu.SetCursor(i)
			return
		}
	}
}

func (m *tuiModel) menuActivate(visible []visibleMenuItem, idx int) tea.Cmd {
	if idx < 0 || idx >= len(visible) {
		return nil
	}
	m.menuWelcome = false
	item := visible[idx]
	if item.item.isGroup {
		m.toggleMenuGroup(item.topIndex)
		return nil
	}
	return m.enterMenuItem(item.item.view)
}

func (m *tuiModel) updateMenu(msg tea.KeyPressMsg) tea.Cmd {
	visible := m.syncMenu()
	if m.menu.UpdateKey(msg) {
		return nil
	}
	switch {
	case key.Matches(msg, tui.MenuKeys.Quit):
		return tea.Quit
	case key.Matches(msg, tui.MenuKeys.Enter):
		return m.menuActivate(visible, m.menu.Cursor())
	}
	return nil
}

func (m *tuiModel) menuMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button != tea.MouseLeft {
		return nil
	}
	visible := m.syncMenu()
	start, end := m.menu.VisibleRange()
	row := msg.Y - strings.Count(m.menuHeader(), "\n")
	if row < 0 || row >= 2*(end-start) {
		return nil
	}
	idx := start + row/2
	if m.menu.Cursor() == idx {
		return m.menuActivate(visible, idx)
	}
	m.menu.SetCursor(idx)
	return nil
}

func (m *tuiModel) menuMouseWheel(msg tea.MouseWheelMsg) tea.Cmd { //nolint:unparam // Mouse dispatch returns a command even when only scrolling.
	m.syncMenu()
	m.menu.UpdateWheel(msg)
	return nil
}

func (m *tuiModel) renderMenu() tea.View {
	visible := m.syncMenu()
	var b strings.Builder
	b.WriteString(m.menuHeader())
	start, end := m.menu.VisibleRange()
	width := m.termWidth
	if width <= 0 {
		width = 80
	}
	for i := start; i < end; i++ {
		item := visible[i]
		selected := i == m.menu.Cursor()
		indent := ""
		if item.childIndex >= 0 {
			indent = "  "
		}
		marker := "  "
		if selected {
			marker = "> "
		}
		label := item.item.name
		if item.item.isGroup {
			indicator := "▶ "
			if m.menuExpanded[item.topIndex] {
				indicator = "▼ "
			}
			label = indicator + label
		}
		label = shared.TruncateText(indent+marker+label, width)
		style := m.styles.Normal
		if selected {
			style = m.styles.Selected
		}
		b.WriteString(style.Render(label) + "\n")
		description := shared.TruncateText(indent+"    "+item.item.description, width)
		b.WriteString(m.styles.Help.Render(description) + "\n")
	}
	b.WriteString(m.menuFooter())
	v := shared.NewAltScreenView(b.String(), m.styles.Theme)
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
