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
	return renderPopup(styles, width, 24, p)
}

func renderPopup(styles Styles, width, height int, p Popup) string {
	boxW := PopupWidth(width)
	inner := max(boxW-6, 1)
	title := p.Title
	if tag := severityTag(p.Severity); tag != "" {
		if title != "" {
			title = tag + ": " + title
		} else {
			title = tag
		}
	}
	title = lipgloss.NewStyle().Width(inner).MaxHeight(1).Render(styles.Title.Render(title))
	message := p.Message
	for _, detail := range p.Details {
		message += "\n• " + detail
	}
	hints := lipgloss.NewStyle().Width(inner).Render(styles.Help.Render(p.Hints))
	available := max(height-4-lipgloss.Height(title)-lipgloss.Height(hints), 1)
	body := lipgloss.NewStyle().Width(inner).MaxHeight(available).Render(message)
	content := strings.Join([]string{title, body, hints}, "\n")
	border := p.BorderColor
	if border == nil {
		border = severityBorder(styles.Theme, p.Severity)
	}
	return overlayBox(styles, content, boxW, border)
}

// RenderOverlayAtWidth shares the frame and placement used by forms and popups.
// boxWidth includes the border; content must fit the caller's viewport budget.
func RenderOverlayAtWidth(styles Styles, bg, content string, width, height, boxWidth int) string {
	return OverlayPopup(styles, bg, overlayBox(styles, content, boxWidth, styles.Theme.Primary), width, height)
}

func overlayBox(styles Styles, content string, width int, border color.Color) string {
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(border).BorderBackground(styles.Theme.Mantle).
		Background(styles.Theme.Mantle).Padding(0, 2).Width(max(width-2, 1)).Render(content)
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
	box := renderPopup(styles, w, h, p)
	return OverlayPopup(styles, bg, box, w, h)
}

// PopupWidth picks a box width that fits narrow terminals.
// Target is 75% of terminal up to 80 cells; floor at 20 so wrapping still
// produces readable lines, and always leave a 2-cell margin.
func PopupWidth(termWidth int) int {
	if termWidth <= 0 {
		termWidth = 80
	}
	return max(1, min(max(termWidth*3/4, 44), 80, termWidth-2))
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
