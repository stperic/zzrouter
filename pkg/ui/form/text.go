package form

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/stperic/zzrouter/pkg/ui"
)

// LabelWidth is the column width reserved for the field label. Labels
// longer than this are truncated with an ellipsis. Shorter labels are
// right-padded so inputs line up across a form.
const LabelWidth = 16

// TextField renders a single-line input as `Label   <filled-bg-input>`.
// No border, no second line — the input is signaled by a subtle Surface
// background and (when focused) an Accent-colored label.
type TextField struct {
	label   string
	hint    string // optional inline caption rendered to the right of the input
	input   textinput.Model
	focused bool
}

// NewText builds a text field with the given label and placeholder.
// charLimit caps the input length and drives the on-screen input width
// (capped at 40, floored at 8).
func NewText(label, placeholder string, charLimit int) *TextField {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = placeholder
	ti.CharLimit = charLimit
	w := charLimit
	if w > 40 {
		w = 40
	}
	if w < 8 {
		w = 8
	}
	ti.SetWidth(w)
	return &TextField{label: label, input: ti}
}

// Label returns the field label.
func (f *TextField) Label() string { return f.label }

// SetValue sets the input value.
func (f *TextField) SetValue(s string) { f.input.SetValue(s) }

// Value returns the current input value.
func (f *TextField) Value() string { return f.input.Value() }

// SetHint sets an optional caption rendered to the right of the input.
func (f *TextField) SetHint(s string) { f.hint = s }

// Focus / Blur / IsFocused — focus state.
func (f *TextField) Focus() {
	f.focused = true
	f.input.Focus()
}

func (f *TextField) Blur() {
	f.focused = false
	f.input.Blur()
}

func (f *TextField) IsFocused() bool { return f.focused }

// HandleKey forwards keys to the underlying textinput, except Tab /
// Shift-Tab / ↑ / ↓, which are reserved for form navigation.
func (f *TextField) HandleKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "tab", "shift+tab", "up", "down":
		return false, nil
	}
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(msg)
	return true, cmd
}

// View renders one line: "Label   <filled-bg input>   <optional hint>".
// Width is currently unused; inputs size themselves from their charLimit.
func (f *TextField) View(_ int, styles ui.Styles) string {
	label := padLabel(f.label, LabelWidth)
	if f.focused {
		label = styles.Accent.Render(label)
	} else {
		label = styles.Normal.Render(label)
	}

	// Filled-background input for visibility; a brighter bg + bold when
	// focused.
	bg := styles.Theme.Surface
	inputStyle := lipgloss.NewStyle().Background(bg).Padding(0, 1)
	if f.focused {
		inputStyle = inputStyle.Bold(true)
	}
	input := inputStyle.Render(f.input.View())

	var b strings.Builder
	b.WriteString(label)
	b.WriteString(input)
	if f.hint != "" {
		b.WriteString("  " + styles.Help.Render(f.hint))
	}
	return b.String()
}

// padLabel right-pads or truncates a label to exactly width columns.
func padLabel(s string, width int) string {
	if len(s) >= width {
		return s[:width-1] + "…"
	}
	return fmt.Sprintf("%-*s", width, s)
}
