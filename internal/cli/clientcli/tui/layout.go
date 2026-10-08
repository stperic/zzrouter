// Package tui provides shared Bubbletea v2 primitives for the zzrouter client:
// layout math, list/detail/confirm models, typed navigation, and a root host
// that composes views implementing tea.Model plus optional capability
// interfaces (Breadcrumber, HelpKeyer, Titler).
package tui

import (
	"strings"

	lipgloss "charm.land/lipgloss/v2"
)

// Chrome heights. A parent rendering a child view reserves these rows for
// title + breadcrumb; the child receives a height budget already reduced by
// TitleBarLines + BreadcrumbLines.
const (
	TitleBarLines   = 2 // title bar + separator
	BreadcrumbLines = 2 // breadcrumb + blank
	TableHeaderAdd  = 2 // table header + separator
	TableTerminator = 1 // trailing newline after table.Render()
	ConfirmLines    = 2 // confirm dialog message + blank
)

// RenderedHeight counts terminal rows a rendered string occupies. A trailing
// partial line (no final \n) still counts as one row. Empty string = 0.
func RenderedHeight(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// VisibleRows returns data-row capacity given a height budget and footer.
// Layout: tableChrome + dataRows + shared.TableTerminator + footer.
func VisibleRows(height int, footer string) int {
	rows := height - TableHeaderAdd - TableTerminator - RenderedHeight(footer)
	if rows < 3 {
		return 3
	}
	return rows
}

// Truncate clamps a string to a target display width (lipgloss-aware, so
// wide glyphs and ANSI escapes measure correctly). If truncated, suffix is
// appended and still fits within target.
func Truncate(s string, target int, suffix string) string {
	if target <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= target {
		return s
	}
	sw := lipgloss.Width(suffix)
	if sw >= target {
		// Suffix alone exceeds budget; fall back to rune slicing.
		runes := []rune(suffix)
		if len(runes) > target {
			return string(runes[:target])
		}
		return suffix
	}
	target -= sw
	runes := []rune(s)
	// Binary search the longest prefix that fits.
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if lipgloss.Width(string(runes[:mid])) <= target {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]) + suffix
}
