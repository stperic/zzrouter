package form

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/pkg/ui"
)

// RadioField is a horizontal single-select rendered as "(●) a  ( ) b".
// ←/→ cycles options while focused; Tab/↑/↓ defer to form navigation.
type RadioField struct {
	label    string
	options  []string // display labels
	values   []string // parallel values; empty entry uses option label
	selected int
	focused  bool
}

// NewRadio builds a radio with parallel options + values. If values is nil,
// the options slice is used as both labels and values.
func NewRadio(label string, options, values []string) *RadioField {
	if values == nil {
		values = append([]string(nil), options...)
	}
	return &RadioField{label: label, options: options, values: values}
}

// Label returns the field label.
func (f *RadioField) Label() string { return f.label }

// Selected returns the current selected index.
func (f *RadioField) Selected() int { return f.selected }

// SetSelected sets the selected index (clamped).
func (f *RadioField) SetSelected(i int) {
	if i < 0 {
		i = 0
	}
	if i >= len(f.options) {
		i = len(f.options) - 1
	}
	f.selected = i
}

// SetSelectedByValue sets the selected index to the option with matching
// value. No-op if not found.
func (f *RadioField) SetSelectedByValue(v string) {
	for i, val := range f.values {
		if val == v {
			f.selected = i
			return
		}
	}
}

// Value returns the value string of the selected option.
func (f *RadioField) Value() string {
	if f.selected < 0 || f.selected >= len(f.values) {
		return ""
	}
	return f.values[f.selected]
}

// Focus / Blur / IsFocused — focus state.
func (f *RadioField) Focus()          { f.focused = true }
func (f *RadioField) Blur()           { f.focused = false }
func (f *RadioField) IsFocused() bool { return f.focused }

// HandleKey consumes ←/→ for option movement. Other keys defer to form.
func (f *RadioField) HandleKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "left":
		if f.selected > 0 {
			f.selected--
		}
		return true, nil
	case "right":
		if f.selected < len(f.options)-1 {
			f.selected++
		}
		return true, nil
	}
	return false, nil
}

// View renders one line: "Label   (●) a  ( ) b  ( ) c".
func (f *RadioField) View(_ int, styles ui.Styles) string {
	label := padLabel(f.label, LabelWidth)
	if f.focused {
		label = styles.Accent.Render(label)
	} else {
		label = styles.Normal.Render(label)
	}

	parts := make([]string, 0, len(f.options))
	for i, opt := range f.options {
		if i == f.selected {
			parts = append(parts, styles.Selected.Render("(●) "+opt))
		} else {
			parts = append(parts, "( ) "+opt)
		}
	}
	return label + strings.Join(parts, "  ")
}
