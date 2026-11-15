package form

import (
	"charm.land/lipgloss/v2"

	"github.com/stperic/zzrouter/pkg/ui"
)

// RenderOverlay centers a popup content string over a background view.
// The popup is drawn as a rounded primary-bordered box with no fill, so
// the user's terminal background reads naturally through unused space.
// Section headers, field labels, and field rendering are the caller's
// responsibility; this only handles framing + centering.
func RenderOverlay(content, bg string, width, height int, styles ui.Styles) string {
	popupWidth := width * 3 / 4
	if popupWidth > 80 {
		popupWidth = 80
	}
	if popupWidth < 44 {
		popupWidth = 44
	}
	if popupWidth > width-4 && width > 10 {
		popupWidth = width - 4
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(styles.Theme.Primary).
		Padding(1, 2).
		Width(popupWidth).
		Render(content)

	w, h := width, height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}

	boxH := lipgloss.Height(box)
	boxW := lipgloss.Width(box)
	startCol := (w - boxW) / 2
	startRow := (h - boxH) / 2
	if startCol < 0 {
		startCol = 0
	}
	if startRow < 0 {
		startRow = 0
	}

	bgLayer := lipgloss.NewLayer(bg)
	fgLayer := lipgloss.NewLayer(box).X(startCol).Y(startRow).Z(1)
	comp := lipgloss.NewCompositor(bgLayer, fgLayer)
	canvas := lipgloss.NewCanvas(w, h)
	return canvas.Compose(comp).Render()
}
