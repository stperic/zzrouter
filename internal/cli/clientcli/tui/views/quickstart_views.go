package views

import (
	"fmt"
	"strings"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/pkg/ui"
)

// ============================================================================
// Quickstart Wizard — orchestrator view (tutorial + checklist + completion)
// ============================================================================

type quickstartPhase int

const (
	qsPhaseTutorial quickstartPhase = iota // tutorial pages
	qsPhaseWizard                          // guided checklist
	qsPhaseComplete                        // navigation guide
)

// wizardStep identifies which TUI view to launch for each checklist item.
type wizardStep int

const (
	qsStepHardware wizardStep = iota
	qsStepProvider
	qsStepPull
	qsStepChat
	qsStepCount // sentinel
)

var wizardStepLabels = [qsStepCount]string{
	"Detect hardware",
	"Choose a provider",
	"Deploy your first model",
	"Send a test request",
}

var wizardStepViews = [qsStepCount]tui.View{
	tui.ViewNodes,
	tui.ViewProviders,
	tui.ViewSearch,
	tui.ViewChat,
}

type QuickstartViewModel struct {
	styles     ui.Styles
	termWidth  int
	termHeight int

	phase       quickstartPhase
	tutPage     int // 0 .. totalTutorialPages-1
	stepCursor  int // 0 .. qsStepCount-1
	stepDone    [qsStepCount]bool
	stepSummary [qsStepCount]string // brief summary after completion

	shimmerFrame int
	pages        []tutorialPage
}

func NewQuickstartViewModel(styles ui.Styles) *QuickstartViewModel {
	return &QuickstartViewModel{
		styles: styles,
		pages:  tutorialPageList(),
	}
}

// shimmerTickMsg for quickstart shimmer animation.
type qsShimmerTickMsg struct{}

func qsShimmerTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(_ time.Time) tea.Msg {
		return qsShimmerTickMsg{}
	})
}

// Init implements tea.Model. Starts the shimmer animation.
func (m *QuickstartViewModel) Init() tea.Cmd {
	return qsShimmerTickCmd()
}

// Key maps

type qsTutorialKeyMap struct {
	Enter key.Binding
	Back  key.Binding
	Quit  key.Binding
}

var qsTutorialKeys = qsTutorialKeyMap{
	Enter: key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "continue")),
	Back:  key.NewBinding(key.WithKeys("backspace", "left"), key.WithHelp("←", "back")),
	Quit:  key.NewBinding(key.WithKeys("q", "Q", "ctrl+c"), key.WithHelp("Q", "quit")),
}

type qsWizardKeyMap struct {
	Up    key.Binding
	Down  key.Binding
	Enter key.Binding
	Quit  key.Binding
}

var qsWizardKeys = qsWizardKeyMap{
	Up:    key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:  key.NewBinding(key.WithKeys("down", "j")),
	Enter: key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "open")),
	Quit:  key.NewBinding(key.WithKeys("q", "Q", "ctrl+c"), key.WithHelp("Q", "quit")),
}

// ============================================================================
// Update
// ============================================================================

// Update implements tea.Model.
func (m *QuickstartViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case qsShimmerTickMsg:
		m.shimmerFrame++
		return m, qsShimmerTickCmd()

	case tui.QsStepDoneMsg:
		if msg.Step >= 0 && msg.Step < int(qsStepCount) {
			m.stepDone[msg.Step] = true
			if msg.Summary != "" {
				m.stepSummary[msg.Step] = msg.Summary
			}
			// Auto-advance cursor to next incomplete step
			for i := msg.Step + 1; i < int(qsStepCount); i++ {
				if !m.stepDone[i] {
					m.stepCursor = i
					break
				}
			}
			// Check if all steps done → complete
			allDone := true
			for _, d := range m.stepDone {
				if !d {
					allDone = false
					break
				}
			}
			if allDone {
				m.phase = qsPhaseComplete
			}
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
		return m, nil

	case tea.KeyPressMsg:
		switch m.phase {
		case qsPhaseTutorial:
			return m, m.updateTutorial(msg)
		case qsPhaseWizard:
			return m, m.updateWizard(msg)
		case qsPhaseComplete:
			return m, m.updateComplete(msg)
		}
	}

	return m, nil
}

func (m *QuickstartViewModel) updateTutorial(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, qsTutorialKeys.Quit):
		return tea.Quit
	case key.Matches(msg, qsTutorialKeys.Enter):
		if m.tutPage < totalTutorialPages-1 {
			m.tutPage++
		} else {
			// Transition to wizard phase
			m.phase = qsPhaseWizard
		}
		return nil
	case key.Matches(msg, qsTutorialKeys.Back):
		if m.tutPage > 0 {
			m.tutPage--
		}
		return nil
	}
	return nil
}

func (m *QuickstartViewModel) updateWizard(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, qsWizardKeys.Quit):
		return tea.Quit
	case key.Matches(msg, qsWizardKeys.Up):
		if m.stepCursor > 0 {
			m.stepCursor--
		}
		return nil
	case key.Matches(msg, qsWizardKeys.Down):
		if m.stepCursor < int(qsStepCount)-1 {
			m.stepCursor++
		}
		return nil
	case key.Matches(msg, qsWizardKeys.Enter):
		step := wizardStep(m.stepCursor)
		return func() tea.Msg {
			return tui.QsLaunchViewMsg{View: wizardStepViews[step], Step: int(step)}
		}
	}
	return nil
}

func (m *QuickstartViewModel) updateComplete(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, qsTutorialKeys.Quit):
		return tea.Quit
	case key.Matches(msg, qsTutorialKeys.Enter):
		return func() tea.Msg { return tui.QsEnterMenuMsg{} }
	case key.Matches(msg, qsTutorialKeys.Back):
		m.phase = qsPhaseWizard
		return nil
	}
	return nil
}

// ============================================================================
// View
// ============================================================================

// Breadcrumb implements tui.Breadcrumber.
func (m *QuickstartViewModel) Breadcrumb() []string {
	return []string{"Quick Start"}
}

// View implements tea.Model.
func (m *QuickstartViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
}

func (m *QuickstartViewModel) viewContent() string {
	switch m.phase {
	case qsPhaseTutorial:
		return m.viewTutorial(m.termWidth, m.termHeight)
	case qsPhaseWizard:
		return m.viewWizard(m.termWidth, m.termHeight)
	case qsPhaseComplete:
		return m.viewComplete(m.termWidth, m.termHeight)
	}
	return ""
}

func (m *QuickstartViewModel) viewTutorial(width, height int) string {
	if m.tutPage >= len(m.pages) {
		return ""
	}
	page := m.pages[m.tutPage]

	// Render body
	bodyHeight := height - 2 // reserve for footer
	body := page.renderBody(width, bodyHeight, m.styles, m.shimmerFrame)

	// Footer
	pageIndicator := m.styles.Help.Render(fmt.Sprintf("Page %d of %d", m.tutPage+1, totalTutorialPages))

	hintText := "Enter to continue"
	if page.hint != "" {
		hintText = page.hint
	}
	if m.tutPage > 0 {
		hintText = "← Back  ·  " + hintText
	}
	hintText += "  ·  q quit"
	hints := m.styles.Help.Render(hintText)

	// Build footer line — hints left, page indicator right
	hintsW := lipgloss.Width(hints)
	indW := lipgloss.Width(pageIndicator)
	gap := max(width-hintsW-indW-2, 1)
	footer := " " + hints + strings.Repeat(" ", gap) + pageIndicator

	if page.centered {
		// Center content vertically
		content := body + "\n\n" + footer
		placed := lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
		return placed
	}

	// Left-aligned with footer at bottom
	bodyLines := strings.Count(body, "\n") + 1
	padding := max(height-bodyLines-2, 0)
	return body + strings.Repeat("\n", padding) + footer
}

func (m *QuickstartViewModel) viewWizard(width, height int) string {
	var b strings.Builder

	// Step progress track
	doneCount := 0
	for _, d := range m.stepDone {
		if d {
			doneCount++
		}
	}
	progressTitle := fmt.Sprintf("Step %d of %d", doneCount+1, qsStepCount)
	if doneCount >= int(qsStepCount) {
		progressTitle = "All steps complete"
	}
	b.WriteString(m.styles.Title.Render(progressTitle) + "\n")

	// Progress bar
	trackWidth := max(width-4, 20)
	if trackWidth > 60 {
		trackWidth = 60
	}
	filled := (trackWidth * doneCount) / int(qsStepCount)
	empty := trackWidth - filled
	bar := m.styles.Success.Render(strings.Repeat("━", filled)) +
		m.styles.Help.Render(strings.Repeat("░", empty))
	b.WriteString("  " + bar + "\n\n")

	// Checklist
	for i := range int(qsStepCount) {
		selected := i == m.stepCursor

		// Status icon
		var icon string
		switch {
		case m.stepDone[i]:
			icon = m.styles.Success.Render("✔")
		case selected:
			icon = m.styles.Title.Render("▶")
		default:
			icon = m.styles.Help.Render("○")
		}

		// Label
		label := wizardStepLabels[i]
		var styledLabel string
		if selected {
			styledLabel = m.styles.Selected.Render(label)
		} else if m.stepDone[i] {
			styledLabel = m.styles.Normal.Render(label)
		} else {
			styledLabel = m.styles.Help.Render(label)
		}

		// Summary or action hint
		var detail string
		if m.stepDone[i] && m.stepSummary[i] != "" {
			detail = m.styles.Help.Render(m.stepSummary[i])
		} else if selected && !m.stepDone[i] {
			detail = m.styles.Help.Render("Press Enter")
		}

		line := fmt.Sprintf("  %s  %-30s  %s", icon, styledLabel, detail)
		b.WriteString(line + "\n")
	}

	// Footer
	hints := tui.JoinHints(
		tui.Hint(qsWizardKeys.Up, "navigate"),
		tui.Hint(qsWizardKeys.Enter, "open"),
		tui.Hint(qsWizardKeys.Quit, "quit"),
	)
	padding := max(
		// title + bar + blank + steps + footer
		height-int(qsStepCount)-5, 1)
	b.WriteString(strings.Repeat("\n", padding))
	b.WriteString(shared.RenderViewFooter(m.styles, width, hints))

	return b.String()
}

func (m *QuickstartViewModel) viewComplete(width, height int) string {
	body := renderCompletionScreen(width, height, m.styles)

	// Footer
	hints := m.styles.Help.Render("Enter to open TUI  ·  ← Back  ·  q quit")
	indicator := m.styles.Success.Render("Complete")
	hintsW := lipgloss.Width(hints)
	indW := lipgloss.Width(indicator)
	gap := max(width-hintsW-indW-2, 1)
	footer := " " + hints + strings.Repeat(" ", gap) + indicator

	bodyLines := strings.Count(body, "\n") + 1
	padding := max(height-bodyLines-2, 0)
	return body + strings.Repeat("\n", padding) + footer
}
