package chat

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/stperic/zzrouter/pkg/ui"
)

// slashCommand defines a chat slash command for autocomplete
type slashCommand struct {
	Name    string
	Desc    string
	HasArgs bool // true if command needs arguments (insert text instead of executing)
}

// chatSlashCommands is the ordered list of available slash commands
var chatSlashCommands = []slashCommand{
	{"/verbose", "Toggle verbose mode (show LLM stats)", false},
	{"/thinking", "Toggle thinking/reasoning display", false},
	{"/temperature", "Set temperature (0.0-2.0, -1 = model default)", true},
	{"/top_p", "Set top-p sampling (0.0-1.0, -1 = model default)", true},
	{"/max_tokens", "Set max response tokens (0 = unlimited)", true},
	{"/copy", "Copy last response to clipboard", false},
	{"/clear", "Clear session context", false},
	{"/system", "Set system prompt", true},
	{"/bye", "Exit", false},
}

// autocompleteState tracks the slash command suggestion popup
type autocompleteState struct {
	active      bool
	suggestions []slashCommand
	selected    int
}

// update recomputes suggestions based on current textarea input
func (ac *autocompleteState) update(input string) {
	input = strings.TrimSpace(input)

	// Only trigger for single-line input starting with /
	if input == "" || !strings.HasPrefix(input, "/") || strings.Contains(input, "\n") {
		ac.reset()
		return
	}

	// If input has args (space after command), don't show
	parts := strings.Fields(input)
	if len(parts) > 1 {
		ac.reset()
		return
	}

	prefix := strings.ToLower(parts[0])

	var matches []slashCommand
	for _, cmd := range chatSlashCommands {
		if strings.HasPrefix(cmd.Name, prefix) {
			matches = append(matches, cmd)
		}
	}

	// Don't show if the only match is exact
	if len(matches) == 1 && matches[0].Name == prefix {
		ac.reset()
		return
	}

	if len(matches) == 0 {
		ac.reset()
		return
	}

	ac.active = true
	ac.suggestions = matches
	if ac.selected >= len(matches) {
		ac.selected = len(matches) - 1
	}
}

func (ac *autocompleteState) reset() {
	ac.active = false
	ac.suggestions = nil
	ac.selected = 0
}

func (ac *autocompleteState) moveUp() {
	if ac.selected > 0 {
		ac.selected--
	}
}

func (ac *autocompleteState) moveDown() {
	if ac.selected < len(ac.suggestions)-1 {
		ac.selected++
	}
}

func (ac *autocompleteState) selectedCommand() string {
	if ac.selected < len(ac.suggestions) {
		return ac.suggestions[ac.selected].Name
	}
	return ""
}

func (ac *autocompleteState) selectedHasArgs() bool {
	if ac.selected < len(ac.suggestions) {
		return ac.suggestions[ac.selected].HasArgs
	}
	return false
}

// height returns the number of terminal lines the suggestion popup occupies (0 if inactive)
func (ac *autocompleteState) height() int {
	if !ac.active {
		return 0
	}
	return len(ac.suggestions)
}

// renderSuggestions renders the suggestion list below the textarea
func (ac *autocompleteState) renderSuggestions(styles ui.Styles, width int) string {
	if !ac.active || len(ac.suggestions) == 0 {
		return ""
	}

	// Compute column width for command names
	maxName := 0
	for _, cmd := range ac.suggestions {
		if len(cmd.Name) > maxName {
			maxName = len(cmd.Name)
		}
	}

	highlightName := lipgloss.NewStyle().Bold(true).Foreground(styles.Theme.Text)
	highlightDesc := lipgloss.NewStyle().Bold(true).Foreground(styles.Theme.Subtext)
	dimName := lipgloss.NewStyle().Foreground(styles.Theme.Overlay)
	dimDesc := lipgloss.NewStyle().Foreground(styles.Theme.Overlay)

	var lines []string
	for i, cmd := range ac.suggestions {
		padded := fmt.Sprintf("  %-*s", maxName+4, cmd.Name)
		if i == ac.selected {
			line := highlightName.Render(padded) + highlightDesc.Render(cmd.Desc)
			lines = append(lines, line)
		} else {
			line := dimName.Render(padded) + dimDesc.Render(cmd.Desc)
			lines = append(lines, line)
		}
	}

	_ = width // available for future truncation
	return strings.Join(lines, "\n")
}
