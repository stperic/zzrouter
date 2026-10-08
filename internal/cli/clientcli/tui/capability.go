package tui

import tea "charm.land/bubbletea/v2"

// Views are tea.Model implementations. The root host probes them for the
// following optional capability interfaces via type assertion. A view that
// doesn't participate in a given chrome concern simply omits the method.
//
// This mirrors Bubbletea v2's own evolution (Cursor, focus, resize reported
// via optional interfaces rather than mandatory Model methods).
//
// Titler, Breadcrumber and HelpKeyer are pull: Root queries them while
// composing chrome. Refresher is push: Root tells the view to act, and does
// not store the returned model back, so implement it on a pointer receiver.

// Titler returns the short title shown in the app chrome.
type Titler interface {
	Title() string
}

// Breadcrumber returns the breadcrumb trail. Ordered from root to leaf.
type Breadcrumber interface {
	Breadcrumb() []string
}

// HelpKeyer returns the one-line help string rendered in the footer.
type HelpKeyer interface {
	HelpKey() string
}

// Refresher reloads a view's data. Root calls it on the view that becomes
// active again after a child pops, so state the child changed (a model
// started from chat, a provider installed) shows immediately instead of
// waiting for the next poll tick.
//
// Two things follow from the fact that only the top view receives messages:
//
//   - Fetch silently. The view is already on screen, so flipping it back to
//     a loading state blanks content the user is looking at. Signal the
//     reload in the footer instead.
//   - Treat in-flight state as lost, not pending. Replies this view was
//     waiting on, and any self-scheduling poll timer it had running, were
//     delivered to the child and dropped. Reset those guards and restart
//     the poll loop rather than skipping the reload because they look busy.
//
// Mutating state in Refresh requires a pointer receiver: Root discards the
// receiver afterwards.
type Refresher interface {
	Refresh() tea.Cmd
}
