package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// StatusMessage is the interface for custom messages that carry a success/failure status.
// Implement this on any tea.Msg that should update the TUI status bar.
type StatusMessage interface {
	StatusText() string
	IsSuccess() bool
}

// TUIRenderer implements InteractiveRenderer using Bubble Tea v2
type TUIRenderer struct {
	theme       *Theme
	pageSize    int
	model       *tuiModel
	keyBindings []KeyBinding
}

// tuiModel is the bubbletea model for interactive table display
type tuiModel struct {
	data             TableData
	theme            *Theme
	keyBindings      map[string]KeyBinding
	statusMsg        string
	err              error
	quitting         bool
	confirmationMode bool
	confirmationMsg  string
	confirmationKey  string // The key that triggered confirmation
	cursor           int    // Current cursor position
	page             int    // Current page
	pageSize         int    // Items per page
}

// Custom message types
type updateDataMsg struct {
	data TableData
}

type statusMsg struct {
	message string
	isError bool
}

// NewTUIRenderer creates a new TUI renderer with Bubble Tea v2
func NewTUIRenderer(theme *Theme, pageSize int) *TUIRenderer {
	if theme == nil {
		t := CatppuccinMocha()
		theme = &t
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	return &TUIRenderer{
		theme:       theme,
		pageSize:    pageSize,
		keyBindings: []KeyBinding{},
	}
}

// RenderTable displays data in an interactive table
func (r *TUIRenderer) RenderTable(data TableData) error {
	// Create the model
	r.model = &tuiModel{
		data:        data,
		theme:       r.theme,
		keyBindings: make(map[string]KeyBinding),
		cursor:      0,
		page:        0,
		pageSize:    10,
	}

	// Register key bindings
	for _, binding := range r.keyBindings {
		r.model.keyBindings[binding.Key] = binding
	}

	// Run the program inline (no alt screen, like search command)
	p := tea.NewProgram(r.model)
	_, err := p.Run()
	return err
}

// ShowMessage displays a message (stores for display in View)
func (r *TUIRenderer) ShowMessage(msg string, level MessageLevel) error {
	if r.model != nil {
		r.model.statusMsg = msg
		r.model.err = nil
		if level == LevelError {
			r.model.err = fmt.Errorf("%s", msg)
		}
	}
	return nil
}

// ShowLoading displays a loading indicator
func (r *TUIRenderer) ShowLoading(msg string) error {
	return r.ShowMessage(msg, LevelInfo)
}

// Close cleans up resources
func (r *TUIRenderer) Close() error {
	return nil
}

// Run starts the interactive event loop with custom key bindings
func (r *TUIRenderer) Run() error {
	if r.model == nil {
		return fmt.Errorf("no data to display, call RenderTable first")
	}
	// Already running in RenderTable
	return nil
}

// Update updates the display with new data
func (r *TUIRenderer) Update(data any) error {
	if r.model == nil {
		return fmt.Errorf("renderer not initialized")
	}

	tableData, ok := data.(TableData)
	if !ok {
		return fmt.Errorf("data must be TableData type")
	}

	r.model.data = tableData
	return nil
}

// SetKeyHandler sets custom key bindings
func (r *TUIRenderer) SetKeyHandler(handler KeyHandler) error {
	// This interface method is not ideal for multiple handlers
	// Use AddKeyBinding instead
	return fmt.Errorf("use AddKeyBinding to add custom key handlers")
}

// AddKeyBinding adds a custom key binding
func (r *TUIRenderer) AddKeyBinding(binding KeyBinding) {
	r.keyBindings = append(r.keyBindings, binding)
}

// Bubbletea Model Implementation

// Init initializes the model
func (m *tuiModel) Init() tea.Cmd {
	return nil
}

// Update handles messages and updates the model
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		// Handle confirmation mode
		if m.confirmationMode {
			switch msg.String() {
			case "y", "Y":
				// User confirmed - execute the original handler
				if binding, ok := m.keyBindings[m.confirmationKey]; ok {
					handlerCmd := binding.Handler.HandleKey(m.cursor, m.data)

					m.confirmationMode = false
					m.confirmationMsg = ""
					m.confirmationKey = ""

					if teaCmd, ok := handlerCmd.(tea.Cmd); ok {
						return m, teaCmd
					}
				}
				m.confirmationMode = false
				return m, nil

			case "n", "N", "escape":
				// User cancelled
				m.confirmationMode = false
				m.confirmationMsg = ""
				m.confirmationKey = ""
				m.statusMsg = "Cancelled"
				return m, nil
			}
			// Ignore other keys in confirmation mode
			return m, nil
		}

		// Normal mode key handling
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				if m.cursor < m.page*m.pageSize {
					m.page--
				}
			}
			return m, nil

		case "down", "j":
			if m.cursor < len(m.data.Rows)-1 {
				m.cursor++
				if m.cursor >= (m.page+1)*m.pageSize {
					m.page++
				}
			}
			return m, nil

		case "pgup":
			if m.page > 0 {
				m.page--
				m.cursor = m.page * m.pageSize
			}
			return m, nil

		case "pgdown":
			maxPage := (len(m.data.Rows) - 1) / m.pageSize
			if m.page < maxPage {
				m.page++
				m.cursor = m.page * m.pageSize
			}
			return m, nil

		default:
			// Check for custom key bindings
			if binding, ok := m.keyBindings[msg.String()]; ok {
				// Check if this action needs confirmation
				if needsConfirmation(msg.String()) {
					// Enter confirmation mode
					m.confirmationMode = true
					m.confirmationKey = msg.String()

					if m.cursor >= 0 && m.cursor < len(m.data.Rows) {
						modelName := m.data.Rows[m.cursor][0]
						m.confirmationMsg = fmt.Sprintf("Delete '%s'? (y/n)", modelName)
					} else {
						m.confirmationMsg = fmt.Sprintf("%s? (y/n)", binding.Description)
					}
					return m, nil
				}

				// No confirmation needed - execute immediately
				handlerCmd := binding.Handler.HandleKey(m.cursor, m.data)

				if teaCmd, ok := handlerCmd.(tea.Cmd); ok {
					return m, teaCmd
				}

				return m, nil
			}
		}

	case updateDataMsg:
		// Update table with new data
		m.data = msg.data
		// Rebuild table (would need access to renderer)
		return m, nil

	case statusMsg:
		m.statusMsg = msg.message
		if msg.isError {
			m.err = fmt.Errorf("%s", msg.message)
		} else {
			m.err = nil
		}
		return m, nil

	default:
		// Handle custom status messages via interface
		if csm, ok := msg.(StatusMessage); ok {
			m.statusMsg = csm.StatusText()
			if !csm.IsSuccess() {
				m.err = fmt.Errorf("%s", csm.StatusText())
			} else {
				m.err = nil
			}
			return m, nil
		}
	}

	return m, nil
}

// View renders the UI
func (m *tuiModel) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}

	var b strings.Builder

	// Styles derived from theme
	t := m.theme
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Accent)
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Primary)
	helpStyle := lipgloss.NewStyle().Foreground(t.Overlay)
	selectedStyle := lipgloss.NewStyle().Foreground(t.Primary).Bold(true)
	normalStyle := lipgloss.NewStyle().Foreground(t.Text)
	errorStyle := lipgloss.NewStyle().Foreground(t.Error).Bold(true)
	successStyle := lipgloss.NewStyle().Foreground(t.Success)

	// Title (use from TableData if provided)
	titleText := m.data.Title
	if titleText == "" {
		titleText = "📊 Interactive Table View"
	}
	b.WriteString(titleStyle.Render(titleText) + "\n\n")

	// Error state
	if m.err != nil {
		b.WriteString(errorStyle.Render("✗ "+m.err.Error()) + "\n")
		b.WriteString(helpStyle.Render("\nPress q to quit") + "\n")
		return tea.NewView(b.String())
	}

	// Column headers
	if len(m.data.Columns) > 0 {
		headerParts := []string{"#"}
		separatorParts := []string{"─"}
		for _, col := range m.data.Columns {
			headerParts = append(headerParts, col.Header)
			separatorParts = append(separatorParts, strings.Repeat("─", len(col.Header)))
		}
		b.WriteString(headerStyle.Render(fmt.Sprintf("  %s", strings.Join(headerParts, "  "))) + "\n")
		b.WriteString(helpStyle.Render(fmt.Sprintf("  %s", strings.Join(separatorParts, "  "))) + "\n")
	}

	// Calculate page bounds
	start := m.page * m.pageSize
	end := min(start+m.pageSize, len(m.data.Rows))

	// Render rows as numbered list
	for i := start; i < end; i++ {
		row := m.data.Rows[i]

		// Format row number and cells
		line := fmt.Sprintf("%2d. %s", i+1, strings.Join(row, "  "))

		// Apply style based on selection
		if i == m.cursor {
			b.WriteString(selectedStyle.Render(line) + "\n")
		} else {
			b.WriteString(normalStyle.Render(line) + "\n")
		}
	}

	// Pagination info
	totalPages := (len(m.data.Rows) + m.pageSize - 1) / m.pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	b.WriteString("\n" + helpStyle.Render(fmt.Sprintf("Page %d/%d | Showing %d-%d of %d items",
		m.page+1, totalPages, start+1, end, len(m.data.Rows))) + "\n")

	// Status message or confirmation prompt
	if m.confirmationMode {
		confirmStyle := lipgloss.NewStyle().Foreground(t.Warning).Bold(true)
		b.WriteString(confirmStyle.Render(m.confirmationMsg) + "\n")
	} else if m.statusMsg != "" {
		if m.err != nil {
			b.WriteString(errorStyle.Render(m.statusMsg) + "\n")
		} else {
			b.WriteString(successStyle.Render(m.statusMsg) + "\n")
		}
	}

	// Help footer - inline format like search
	help := m.buildHelp()
	b.WriteString(helpStyle.Render(help) + "\n")

	return tea.NewView(b.String())
}

// buildHelp builds the help text
func (m *tuiModel) buildHelp() string {
	if m.confirmationMode {
		return "y: Confirm • n/esc: Cancel"
	}

	helps := []string{
		"↑/↓: Navigate",
		"PgUp/PgDn: Page",
		"Home/End: Jump",
	}

	// Add custom key bindings
	for key, binding := range m.keyBindings {
		helps = append(helps, fmt.Sprintf("%s: %s", key, binding.Description))
	}

	helps = append(helps, "q: Quit")

	return strings.Join(helps, " • ")
}

// needsConfirmation checks if a key action requires confirmation
func needsConfirmation(key string) bool {
	// Keys that need confirmation before executing
	confirmKeys := map[string]bool{
		"d": true, // Delete
		"D": true, // Delete
		"x": true, // Remove
		"X": true, // Remove
	}
	return confirmKeys[key]
}
