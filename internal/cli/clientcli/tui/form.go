package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/ui/form"
)

// RenderForm scrolls content around the focused field and keeps actions visible.
// focusLine is the field's line before wrapping; -1 leaves scrolling to the caller.
func RenderForm(styles ui.Styles, viewport *Detail, bg, content, hints string, width, height, focusLine int) string {
	inner := max(form.OverlayWidth(width)-6, 1)
	wrap := lipgloss.NewStyle().Width(inner)
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	focused := 0
	for i, line := range lines {
		lines[i] = wrap.Render(line)
		if i < focusLine {
			focused += lipgloss.Height(lines[i])
		}
	}
	// Actions occupy one line in small terminals, with save and cancel always present.
	footer := wrap.Render(styles.Help.Render(hints))
	if height < 12 {
		footer = styles.Help.Render(shared.FitHints(hints, inner))
	}
	available := max(height-3-lipgloss.Height(footer), 1)
	if focusLine >= 0 {
		viewport.EnsureVisible(focused, available)
	}
	body := viewport.Render(strings.Join(lines, "\n"), available) + "\n" + footer
	return form.RenderOverlay(body, bg, width, height, styles)
}
