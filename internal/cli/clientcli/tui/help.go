package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/ui/form"
)

// Help holds the viewport for the current view's complete shortcut list.
type Help struct {
	open   bool
	detail Detail
}

// Update handles help only outside editable fields; normal data replies continue.
func (h *Help) Update(msg tea.Msg, available bool) bool {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		if !h.open {
			if !available || key.String() != "?" {
				return false
			}
			h.open = true
			h.detail = NewDetail()
			return true
		}
		if key.String() == "esc" || key.String() == "?" || key.String() == "q" {
			h.open = false
		} else {
			h.detail.UpdateKey(key)
		}
		return true
	}
	if h.open {
		switch msg := msg.(type) {
		case tea.MouseWheelMsg:
			h.detail.UpdateWheel(msg)
			return true
		case tea.MouseClickMsg, tea.PasteMsg:
			return true
		}
	}
	return false
}

// View renders all shortcuts with scrolling, keeping the close hint visible.
func (h *Help) View(styles ui.Styles, bg string, width, height int, hints string) string {
	if !h.open {
		return bg
	}
	lines := []string{}
	for _, part := range strings.Split(hints, "  ") {
		if part != "" && part != "? help" {
			lines = append(lines, part)
		}
	}
	content := h.detail.Render(strings.Join(lines, "\n"), max(height-5, 1))
	return form.RenderOverlay(styles.Title.Render("Shortcuts")+"\n"+content+"\n"+styles.Help.Render("↑/↓ scroll  Esc close"), bg, width, height, styles)
}
