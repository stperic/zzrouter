package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// ConfirmKeys is the yes/no/cancel binding set for Confirm dialogs.
type ConfirmKeys struct {
	Yes    key.Binding
	No     key.Binding
	Cancel key.Binding
}

// DefaultConfirmKeys returns the destructive-action preset:
//
//	y        → Yes
//	n, Enter → No  (Enter defaults to safe/no for destructive actions)
//	Esc      → Cancel
func DefaultConfirmKeys() ConfirmKeys {
	return ConfirmKeys{
		Yes:    key.NewBinding(key.WithKeys("y", "Y")),
		No:     key.NewBinding(key.WithKeys("n", "N", "enter")),
		Cancel: key.NewBinding(key.WithKeys("esc")),
	}
}

// ConfirmResult is what UpdateKey reports.
type ConfirmResult int

const (
	ConfirmNone      ConfirmResult = iota // key did not match
	ConfirmYes                            // user confirmed
	ConfirmNo                             // user declined
	ConfirmCancelled                      // user cancelled (Esc)
)

// Confirm is a minimal yes/no dialog. Views own the prompt text and result
// wiring; Confirm only handles the key routing.
type Confirm struct {
	keys ConfirmKeys
}

// NewConfirm returns a Confirm using default (destructive) keys.
func NewConfirm() Confirm { return Confirm{keys: DefaultConfirmKeys()} }

// SetKeys overrides the keymap in place.
func (c *Confirm) SetKeys(k ConfirmKeys) { c.keys = k }

// UpdateKey classifies a key press. Non-matching keys return ConfirmNone
// so callers can fall through to other handlers.
func (c Confirm) UpdateKey(msg tea.KeyPressMsg) ConfirmResult {
	switch {
	case key.Matches(msg, c.keys.Yes):
		return ConfirmYes
	case key.Matches(msg, c.keys.No):
		return ConfirmNo
	case key.Matches(msg, c.keys.Cancel):
		return ConfirmCancelled
	}
	return ConfirmNone
}
