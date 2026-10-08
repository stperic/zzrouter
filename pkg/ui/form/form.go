// Package form is a minimal, reusable TUI form abstraction for Bubbletea
// views. Fields own their own state and key handling; the Form drives
// Tab / ↑ / ↓ navigation and composes the stacked view.
//
// A Field handles any key first via HandleKey. If it returns handled=false,
// the Form tries to interpret the key as navigation. Fields with internal
// cursors (checklists, long pickers) return handled=true while the cursor
// is in-bounds and handled=false at the boundary so ↑/↓ promotes to field
// navigation naturally.
package form

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/pkg/ui"
)

// formNavKeys are the form-owned navigation bindings. Fields see the key
// first via HandleKey; the form only consults these when the field returns
// handled=false.
var (
	keyNext = key.NewBinding(key.WithKeys("tab", "down"))
	keyPrev = key.NewBinding(key.WithKeys("shift+tab", "up"))
)

// Field is the contract every form row implements.
type Field interface {
	// Label is the human-readable field label rendered above the input.
	Label() string

	// Focus / Blur / IsFocused manage focus state on the field.
	Focus()
	Blur()
	IsFocused() bool

	// HandleKey processes a key press targeted at this field. It returns
	// handled=true when the key was consumed and the form should NOT treat
	// it as navigation. Non-key messages should be routed through
	// tea.Cmd returns; forms only dispatch key presses today.
	HandleKey(msg tea.KeyPressMsg) (handled bool, cmd tea.Cmd)

	// View renders the field (label + input block) at the given width.
	View(width int, styles ui.Styles) string
}

// Form is an ordered collection of Fields with a single focus cursor.
type Form struct {
	fields []Field
	focus  int
}

// New builds a Form with the given fields. The first field receives focus.
func New(fields ...Field) *Form {
	f := &Form{fields: fields}
	if len(fields) > 0 {
		fields[0].Focus()
	}
	return f
}

// Fields returns the underlying fields slice.
func (f *Form) Fields() []Field { return f.fields }

// Focused returns the currently focused field, or nil if the form is empty.
func (f *Form) Focused() Field {
	if f.focus < 0 || f.focus >= len(f.fields) {
		return nil
	}
	return f.fields[f.focus]
}

// FocusIndex returns the current focus index.
func (f *Form) FocusIndex() int { return f.focus }

// FocusAt moves focus to the field at index i (wraps).
func (f *Form) FocusAt(i int) {
	if len(f.fields) == 0 {
		return
	}
	n := len(f.fields)
	i = ((i % n) + n) % n
	if cur := f.Focused(); cur != nil {
		cur.Blur()
	}
	f.focus = i
	f.fields[i].Focus()
}

// Next moves focus to the next field (wraps).
func (f *Form) Next() { f.FocusAt(f.focus + 1) }

// Prev moves focus to the previous field (wraps).
func (f *Form) Prev() { f.FocusAt(f.focus - 1) }

// Update routes a message through the focused field, then falls back to
// navigation keys (Tab / Shift+Tab / ↑ / ↓) if the field did not consume
// the key. Non-key messages are ignored; views should keep their own
// handlers for tea.WindowSizeMsg, etc.
func (f *Form) Update(msg tea.Msg) tea.Cmd {
	km, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	if focused := f.Focused(); focused != nil {
		if handled, cmd := focused.HandleKey(km); handled {
			return cmd
		}
	}
	switch {
	case key.Matches(km, keyNext):
		f.Next()
	case key.Matches(km, keyPrev):
		f.Prev()
	}
	return nil
}

// View stacks field views with a single blank line between them.
func (f *Form) View(width int, styles ui.Styles) string {
	var out string
	for i, field := range f.fields {
		if i > 0 {
			out += "\n"
		}
		out += field.View(width, styles) + "\n"
	}
	return out
}
