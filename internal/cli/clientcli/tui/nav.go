package tui

import tea "charm.land/bubbletea/v2"

// Nav messages drive the Root model's navigation stack. Views emit these
// from their Update methods (via tea.Cmd) when the user takes an action
// that should push, pop, or replace the active view.
//
// The target view is a concrete tea.Model constructed by the caller — so
// payload shape is compile-time checked. No ID lookup, no type assertion
// on payload, no registry.

// NavToMsg pushes a new view onto the stack. The current view remains in
// memory and will be restored when NavBackMsg is received.
//
// OnPop is an optional callback invoked by Root after this view pops off
// the stack (and its Cancel runs, if any), before the parent view is
// re-activated. Use it for completion hooks — e.g. quickstart marking a
// step done when the launched view returns.
type NavToMsg struct {
	View  tea.Model
	OnPop func() tea.Cmd
}

// NavBackMsg pops the top view. If the stack has only one frame, the
// message is ignored (the root view is always present).
type NavBackMsg struct{}

// NavReplaceMsg replaces the top view without pushing. Use this for
// "open detail after create" flows where the form shouldn't sit in the
// back-stack.
type NavReplaceMsg struct{ View tea.Model }

// NavQuitMsg asks the root to quit (equivalent to tea.Quit but routed
// through the root so the root can run shutdown hooks first).
type NavQuitMsg struct{}

// NavTo returns a tea.Cmd that pushes the given view.
func NavTo(v tea.Model) tea.Cmd { return func() tea.Msg { return NavToMsg{View: v} } }

// NavToWithPop returns a tea.Cmd that pushes the given view with a callback
// invoked when that view later pops.
func NavToWithPop(v tea.Model, onPop func() tea.Cmd) tea.Cmd {
	return func() tea.Msg { return NavToMsg{View: v, OnPop: onPop} }
}

// BreadcrumbClickMsg is dispatched by the host when the user clicks an
// ancestor segment in the breadcrumb bar. Level is the segment index
// (0 = root). The active view's Update should map this to its internal
// navigation. A view that doesn't render breadcrumbs can ignore it.
type BreadcrumbClickMsg struct{ Level int }

// NavBack returns a tea.Cmd that pops the top view.
func NavBack() tea.Cmd { return func() tea.Msg { return NavBackMsg{} } }

// Canceller is an optional interface for views holding a context.CancelFunc.
// Root calls Cancel() when the view leaves the stack (pop or replace), so
// in-flight goroutines can abort and stale messages can be dropped.
type Canceller interface {
	Cancel()
}

// View identifies a TUI view for navigation dispatching.
type View int

const (
	ViewMenu View = iota
	ViewSearch
	ViewModels
	ViewNodes
	ViewProviders
	ViewChat
	ViewLogs
	ViewRunLogs
	ViewParams
	ViewVkeys
	ViewTeams
	ViewQuickstart
	ViewDeploys
	ViewPricing
	ViewUpdate
)
