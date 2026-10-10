// Package tui provides shared Bubbletea v2 primitives for the zzrouter client:
// layout math, list/detail/confirm models, typed navigation, and a root host
// that composes views implementing tea.Model plus optional capability
// interfaces (Breadcrumber, HelpKeyer, Titler).
package tui

// Chrome heights. A parent rendering a child view reserves these rows for
// title + breadcrumb; the child receives a height budget already reduced by
// TitleBarLines + BreadcrumbLines.
const (
	TitleBarLines   = 2 // title bar + separator
	BreadcrumbLines = 2 // breadcrumb + blank
	ConfirmLines    = 2 // confirm dialog message + blank
)
