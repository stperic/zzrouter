package ui

import (
	"image/color"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
)

// PopupSeverity picks the default border color and textual tag for a Popup.
// Callers can override the color by setting Popup.BorderColor.
type PopupSeverity int

const (
	PopupInfo PopupSeverity = iota
	PopupSuccess
	PopupWarning
	PopupError
)

// Popup describes a centered modal dialog. It is purely presentational —
// callers manage visibility and key dispatch themselves, matching the
// existing confirmKind pattern used by tui_models/tui_routes/tui_teams.
//
// Do not stack popups. If you need stacking, promote this to a tea.Model
// sub-component; a pure render helper can't coordinate focus.
type Popup struct {
	Title    string
	Message  string
	Details  []string
	Severity PopupSeverity
	// Hints is a pre-rendered footer line (typically built with joinHints /
	// hint in client views). Leave empty to omit the footer.
	Hints string
	// BorderColor overrides the severity-derived border color when non-nil.
	BorderColor color.Color
}

// RenderPopup formats the popup as a bordered box. It wraps Message to fit
// the computed inner width, prefixes the title with a textual severity tag,
// and renders Details as a bullet list. Returns the raw box — use
// OverlayPopup to place it on top of a background view.
func RenderPopup(styles Styles, width int, p Popup) string {
	boxW := popupBoxWidth(width)
	innerW := boxW - 4 // 2-cell padding each side
	if innerW < 10 {
		innerW = 10
	}

	border := p.BorderColor
	if border == nil {
		border = severityBorder(styles.Theme, p.Severity)
	}

	var b strings.Builder

	// Title with textual severity tag (survives NO_COLOR / colorblind).
	tag := severityTag(p.Severity)
	titleText := p.Title
	if tag != "" {
		if titleText == "" {
			titleText = tag
		} else {
			titleText = tag + ": " + titleText
		}
	}
	if titleText != "" {
		b.WriteString(styles.Title.Render(titleText))
		b.WriteString("\n\n")
	}

	if p.Message != "" {
		wrapped := lipgloss.NewStyle().
			Foreground(styles.Theme.Text).
			Width(innerW).
			Render(p.Message)
		b.WriteString(wrapped)
		b.WriteString("\n")
	}

	if len(p.Details) > 0 {
		b.WriteString("\n")
		bulletStyle := lipgloss.NewStyle().Foreground(styles.Theme.Subtext).Width(innerW)
		for _, d := range p.Details {
			b.WriteString(bulletStyle.Render("• " + d))
			b.WriteString("\n")
		}
	}

	if p.Hints != "" {
		b.WriteString("\n")
		b.WriteString(styles.Help.Render(p.Hints))
		b.WriteString("\n")
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		BorderBackground(styles.Theme.Mantle).
		Background(styles.Theme.Mantle).
		Padding(1, 2).
		Width(boxW).
		Render(strings.TrimRight(b.String(), "\n"))

	return box
}

// OverlayPopup composes a pre-rendered box on top of a background view. The
// background is right-padded to exactly w×h so the base terminal color does
// not bleed through on resize (a known source of flicker in the original
// private overlays).
func OverlayPopup(styles Styles, bg, box string, w, h int) string {
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

	bgPadded := padToSize(bg, w, h, styles.Theme.Base)

	bgLayer := lipgloss.NewLayer(bgPadded)
	fgLayer := lipgloss.NewLayer(box).X(startCol).Y(startRow).Z(1)
	comp := lipgloss.NewCompositor(bgLayer, fgLayer)
	canvas := lipgloss.NewCanvas(w, h)
	return canvas.Compose(comp).Render()
}

// RenderAndOverlayPopup is the one-shot convenience combining RenderPopup
// and OverlayPopup for callers that don't need to inspect the box.
func RenderAndOverlayPopup(styles Styles, bg string, w, h int, p Popup) string {
	box := RenderPopup(styles, w, p)
	return OverlayPopup(styles, bg, box, w, h)
}

// popupBoxWidth picks a box width that fits narrow terminals.
// Target is 75% of terminal up to 80 cells; floor at 20 so wrapping still
// produces readable lines, and always leave a 2-cell margin.
func popupBoxWidth(termWidth int) int {
	if termWidth <= 0 {
		termWidth = 80
	}
	w := termWidth * 3 / 4
	if w > 80 {
		w = 80
	}
	if w < 44 {
		w = 44
	}
	if w > termWidth-2 {
		w = termWidth - 2
	}
	if w < 20 {
		w = 20
	}
	return w
}

// padToSize ensures s occupies exactly w×h with the given background.
// Shorter lines get right-padded; missing rows get filled with blank lines.
func padToSize(s string, w, h int, bg color.Color) string {
	filler := lipgloss.NewStyle().Background(bg).Width(w).Render("")

	lines := strings.Split(s, "\n")
	// Clip to h rows; pad shorter lines to width w.
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, line := range lines {
		lineW := lipgloss.Width(line)
		if lineW < w {
			pad := lipgloss.NewStyle().Background(bg).Width(w - lineW).Render("")
			lines[i] = line + pad
		}
	}
	for len(lines) < h {
		lines = append(lines, filler)
	}
	return strings.Join(lines, "\n")
}

func severityBorder(t Theme, s PopupSeverity) color.Color {
	switch s {
	case PopupSuccess:
		return t.Success
	case PopupWarning:
		return t.Warning
	case PopupError:
		return t.Error
	default:
		return t.Primary
	}
}

func severityTag(s PopupSeverity) string {
	switch s {
	case PopupSuccess:
		return "Success"
	case PopupWarning:
		return "Warning"
	case PopupError:
		return "Error"
	default:
		return ""
	}
}
