package chat

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// View implements tea.Model — renders the full chat TUI
func (m *ChatModel) View() tea.View {
	titleBar := shared.RenderTitleBar(m.styles, m.termWidth)

	if !m.ready {
		return shared.NewAltScreenView(titleBar+"\n"+"Initializing chat...", m.styles.Theme)
	}

	rule := shared.Hrule(m.styles, m.termWidth)

	// Build the content sections
	sections := []string{
		titleBar,
		m.viewport.View(),
		rule,
		m.textarea.View(),
	}

	// Render autocomplete suggestions below textarea (Claude Code style)
	if suggestions := m.state.autocomplete.renderSuggestions(m.styles, m.termWidth); suggestions != "" {
		sections = append(sections, suggestions)
	}

	sections = append(sections, rule)
	sections = append(sections, m.renderFooter())
	sections = append(sections, rule)
	sections = append(sections, " "+m.styles.HintsBar.Render(tui.ChatKeys.HintsString()))

	content := lipgloss.JoinVertical(lipgloss.Left, sections...)
	v := shared.NewAltScreenView(content, m.styles.Theme)
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// renderFooter builds the combined status + stats footer below the input
func (m *ChatModel) renderFooter() string {
	st := m.state
	width := max(m.termWidth, 20)

	// Left: model name — displayName is enriched with "(provider on node)" once streaming starts.
	// Before the first response, show the preferred node if the user explicitly routed to one.
	label := m.displayName
	if m.PreferredNode != "" && !strings.Contains(m.displayName, " on ") && !strings.Contains(m.displayName, "(") {
		label += " on " + m.PreferredNode
	}
	left := m.styles.ChatStatusAccent.Render(" ● ") + m.styles.ChatStatusAccent.Render(label)

	// Active settings indicators (only show non-default values)
	var indicators []string
	if st.showThinking {
		indicators = append(indicators, "thinking")
	}
	if st.showStats {
		indicators = append(indicators, "verbose")
	}
	if m.temperature >= 0 {
		indicators = append(indicators, fmt.Sprintf("temp:%.1f", m.temperature))
	}
	if m.topP >= 0 {
		indicators = append(indicators, fmt.Sprintf("top_p:%.2f", m.topP))
	}
	if m.maxTokens > 0 {
		indicators = append(indicators, fmt.Sprintf("max:%d", m.maxTokens))
	}
	if len(indicators) > 0 {
		left += m.styles.ChatDim.Render("  [" + strings.Join(indicators, ", ") + "]")
	}

	// Right: stats then status (rightmost)
	var rightParts []string
	if st.totalMsgTokens > 0 {
		rightParts = append(rightParts, fmt.Sprintf("%d tokens", st.totalMsgTokens))
	}
	msgCount := 0
	for _, msg := range st.messages {
		if msg.Role == roleUser || msg.Role == roleAssistant {
			msgCount++
		}
	}
	if msgCount > 0 {
		rightParts = append(rightParts, fmt.Sprintf("%d msgs", msgCount))
	}
	switch {
	case st.statusMessage != "":
		rightParts = append(rightParts, st.statusMessage)
	case st.active:
		rightParts = append(rightParts, "generating...")
	}
	right := m.styles.ChatDim.Render(strings.Join(rightParts, " │ ") + " ")

	// Layout: left ... right, or two lines if too narrow
	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)
	gap := width - leftWidth - rightWidth

	if gap >= 1 {
		m.state.footerLines = 1
		return left + strings.Repeat(" ", gap) + right
	}

	// Wrap: left on first line, right on second line (right-aligned)
	m.state.footerLines = 2
	pad := max(width-rightWidth, 0)
	return left + "\n" + strings.Repeat(" ", pad) + right
}

// resizeTextarea grows or shrinks the textarea to fit its content.
func (m *ChatModel) resizeTextarea() {
	lines := max(m.textarea.LineCount(), chatInputMinLines)
	maxLines := max(m.termHeight/3, chatInputMinLines)
	if lines > maxLines {
		lines = maxLines
	}
	if lines == m.textarea.Height() {
		return
	}
	// Save cursor position before resize
	row, col := m.textarea.Line(), m.textarea.Column()
	m.textarea.SetHeight(lines)
	// SetHeight's repositionView only ensures cursor visibility — it won't
	// scroll up when the viewport grew to fit all content.  Force YOffset=0
	// by moving to the beginning, then restore the cursor.
	m.textarea.MoveToBegin()
	for range row {
		m.textarea.CursorDown()
	}
	m.textarea.SetCursorColumn(col)
	// Adjust chat viewport to accommodate new textarea size
	m.viewport.SetHeight(m.viewportHeight())
	if m.state.followMode {
		m.viewport.GotoBottom()
	}
}

// chatRuleLines is the number of fixed chrome lines: rule above textarea + rule below textarea + rule below footer + hints bar.
const chatRuleLines = 4

// viewportHeight calculates the available height for the message viewport
func (m *ChatModel) viewportHeight() int {
	inputHeight := m.textarea.Height() + 1
	acHeight := m.state.autocomplete.height()
	footerH := max(m.state.footerLines, 1)
	h := max(m.termHeight-inputHeight-acHeight-footerH-shared.TuiTitleBarLines-chatRuleLines, 3)
	return h
}
