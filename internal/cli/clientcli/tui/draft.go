package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/pkg/ui"
)

// Draft preserves a leave intention until the user saves or discards edits.
// Views mark changes in Update and mark the draft clean after a successful write.
type Draft struct {
	Dirty   bool
	Discard func()
	leave   tea.Msg
}

// ConfirmNavigation defers navigation while this draft has unsaved changes.
func (d *Draft) ConfirmNavigation(msg tea.Msg) bool {
	if !d.Dirty {
		return false
	}
	if d.leave == nil {
		d.leave = msg
	}
	return true
}

// Update handles the discard prompt without dispatching input to the form below.
func (d *Draft) Update(msg tea.Msg) (bool, tea.Cmd) {
	if d.leave == nil {
		return false, nil
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch NewConfirm().UpdateKey(msg) {
		case ConfirmYes:
			leave := d.leave
			d.Dirty, d.leave = false, nil
			if d.Discard != nil {
				d.Discard()
			}
			return true, func() tea.Msg { return leave }
		case ConfirmNo, ConfirmCancelled:
			d.leave = nil
		}
		return true, nil
	case tea.MouseClickMsg, tea.MouseWheelMsg, tea.PasteMsg:
		return true, nil
	}
	return false, nil
}

// View overlays the common discard prompt when a leave intention is pending.
func (d *Draft) View(styles ui.Styles, body string, width, height int) string {
	if d.leave == nil {
		return body
	}
	return ui.RenderAndOverlayPopup(styles, body, width, height, ui.Popup{
		Title: "Unsaved changes", Message: "Discard your changes?",
		Hints: "Y discard  Esc keep editing",
	})
}
