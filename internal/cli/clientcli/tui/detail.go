package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Detail holds scroll state for a scrollable detail pane. It is
// content-agnostic — views pre-render their detail string and pass it to
// Render along with a height budget.
type Detail struct {
	scroll  int
	visible int // set by Render
	keys    NavigationKeys
}

// NewDetail creates a Detail using the default navigation keymap.
func NewDetail() Detail { return Detail{keys: DefaultNavigationKeys()} }

// SetKeys overrides the keymap in place.
func (d *Detail) SetKeys(k NavigationKeys) { d.keys = k }

// Reset scroll position. Call on entry to the detail view.
func (d *Detail) Reset() { d.scroll = 0 }

// SetVisible records the viewport height so PageUp/PageDown know the page
// size. Call this in the WindowSizeMsg handler alongside List.SetVisible.
// If the view uses Detail.Render (which sets visible internally), this call
// is unnecessary — it only matters when the view delegates rendering to a
// shared helper like renderDetailView.
func (d *Detail) SetVisible(n int) {
	if n < 1 {
		n = 1
	}
	d.visible = n
}

// Scroll returns the scroll offset. Only meaningful AFTER Render has been
// called at least once for the current frame, because UpdateKey Down does
// not clamp to content height until Render clamps it.
func (d Detail) Scroll() int { return d.scroll }

// UpdateKey handles Up/Down/PgUp/PgDn/Home/End. Returns true on match.
// Call SetVisible (or supply via Render) so PgUp/PgDn know the page size.
func (d *Detail) UpdateKey(msg tea.KeyPressMsg) bool {
	switch {
	case key.Matches(msg, d.keys.Up):
		if d.scroll > 0 {
			d.scroll--
		}
	case key.Matches(msg, d.keys.Down):
		d.scroll++
	case key.Matches(msg, d.keys.PageUp):
		d.scroll -= d.visible
		if d.scroll < 0 {
			d.scroll = 0
		}
	case key.Matches(msg, d.keys.PageDown):
		d.scroll += d.visible
	case key.Matches(msg, d.keys.Home):
		d.scroll = 0
	case key.Matches(msg, d.keys.End):
		d.scroll = 1 << 30 // clamped on next Render
	default:
		return false
	}
	return true
}

// UpdateWheel handles wheel events.
func (d *Detail) UpdateWheel(msg tea.MouseWheelMsg) {
	switch msg.Button {
	case tea.MouseWheelUp:
		if d.scroll > 0 {
			d.scroll--
		}
	case tea.MouseWheelDown:
		d.scroll++
	}
}

// Render returns the visible slice of content given a height budget, and
// clamps the internal scroll position. Content is split on \n; the returned
// string is "\n"-joined without a trailing newline.
func (d *Detail) Render(content string, height int) string {
	if height < 1 {
		height = 1
	}
	d.visible = height
	lines := strings.Split(content, "\n")
	total := len(lines)
	maxScroll := total - height
	if maxScroll < 0 {
		maxScroll = 0
	}
	if d.scroll > maxScroll {
		d.scroll = maxScroll
	}
	if d.scroll < 0 {
		d.scroll = 0
	}
	end := d.scroll + height
	if end > total {
		end = total
	}
	return strings.Join(lines[d.scroll:end], "\n")
}
