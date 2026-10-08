package tui

import (
	tea "charm.land/bubbletea/v2"
)

// Root is a tea.Model that hosts a stack of views and dispatches messages
// to the active (top-of-stack) view. It owns:
//
//   - the navigation stack (NavTo/NavBack/NavReplace)
//   - the last window size (propagated on arrival and re-sent on activation
//     because bubbles/v2/viewport renders blank for one frame when its
//     SetSize was missed while inactive)
//   - Canceller invocation when a view leaves the stack
//   - Cursor forwarding: Root's View() returns the active view's tea.View
//     verbatim, so any Cursor set by the active view reaches the terminal
//     under WithAltScreen. This is the v2 idiom — cursor is a field on View,
//     not a separate Model method.
//
// Root deliberately does no chrome rendering of its own. Views are
// responsible for their own borders, breadcrumbs, and footers. The root
// only decides whose View() is drawn.
type Root struct {
	stack    []tea.Model
	onPop    []func() tea.Cmd // parallel to stack; entry i runs when stack[i] pops
	lastSize tea.WindowSizeMsg
	hasSize  bool
}

// NewRoot returns a Root with the given initial view on the stack.
func NewRoot(initial tea.Model) *Root {
	return &Root{stack: []tea.Model{initial}, onPop: []func() tea.Cmd{nil}}
}

// NewEmptyRoot returns a Root with no initial view. The host should push
// a view via NavTo before forwarding non-nav messages. While empty,
// Update is a no-op for non-nav messages and View returns an empty view.
func NewEmptyRoot() *Root {
	return &Root{}
}

// Init runs the initial view's Init.
func (r *Root) Init() tea.Cmd {
	if len(r.stack) == 0 {
		return nil
	}
	return r.stack[0].Init()
}

// Active returns the top-of-stack view (or nil if empty).
func (r *Root) Active() tea.Model {
	if len(r.stack) == 0 {
		return nil
	}
	return r.stack[len(r.stack)-1]
}

// Depth returns the current stack depth.
func (r *Root) Depth() int { return len(r.stack) }

// Update handles nav messages at the root and forwards everything else to
// the active view. WindowSizeMsg is stored and forwarded in parallel so
// inactive views can be re-informed on activation.
func (r *Root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		r.lastSize = m
		r.hasSize = true
		// fall through and forward to active view

	case NavToMsg:
		return r, r.push(m.View, m.OnPop)

	case NavBackMsg:
		return r, r.pop()

	case NavReplaceMsg:
		return r, r.replace(m.View)

	case NavQuitMsg:
		return r, tea.Quit
	}

	if len(r.stack) == 0 {
		return r, nil
	}
	top := r.stack[len(r.stack)-1]
	updated, cmd := top.Update(msg)
	r.stack[len(r.stack)-1] = updated
	return r, cmd
}

// View returns the active view's tea.View. When the stack is empty it
// returns an empty view. Cursor is carried through because tea.View
// already contains it in v2.
func (r *Root) View() tea.View {
	if len(r.stack) == 0 {
		return tea.NewView("")
	}
	return r.stack[len(r.stack)-1].View()
}

// TeardownAll invokes Cancel() on every stacked view that satisfies
// Canceller. Intended as a defer around tea.Program.Run so that
// Ctrl-C / forced exits don't leak goroutines owned by active views
// (e.g. jobstream SSE subscriptions). Safe to call on an empty stack;
// does not modify the stack (views remain so the deferred caller can
// still render a terminal frame if they want).
func (r *Root) TeardownAll() {
	if r == nil {
		return
	}
	for _, v := range r.stack {
		if c, ok := v.(Canceller); ok {
			c.Cancel()
		}
	}
}

// --- navigation ---

func (r *Root) push(v tea.Model, onPop func() tea.Cmd) tea.Cmd {
	if v == nil {
		return nil
	}
	r.stack = append(r.stack, v)
	r.onPop = append(r.onPop, onPop)
	return r.activate(v)
}

func (r *Root) pop() tea.Cmd {
	if len(r.stack) == 0 {
		return nil
	}
	leaving := r.stack[len(r.stack)-1]
	cb := r.onPop[len(r.onPop)-1]
	r.stack = r.stack[:len(r.stack)-1]
	r.onPop = r.onPop[:len(r.onPop)-1]
	if c, ok := leaving.(Canceller); ok {
		c.Cancel()
	}
	// Re-deliver window size to the now-active view (if any); it may have
	// missed a resize while inactive. Empty stack is a valid post-pop
	// state — the host detects it and switches rendering.
	// tea.Batch drops nil entries, so collect unconditionally.
	var cmds []tea.Cmd
	if len(r.stack) > 0 {
		if r.hasSize {
			newTop := r.stack[len(r.stack)-1]
			updated, cmd := newTop.Update(r.lastSize)
			r.stack[len(r.stack)-1] = updated
			cmds = append(cmds, cmd)
		}
		// The child may have changed state the parent renders.
		if rf, ok := r.stack[len(r.stack)-1].(Refresher); ok {
			cmds = append(cmds, rf.Refresh())
		}
	}
	if cb != nil {
		cmds = append(cmds, cb())
	}
	return tea.Batch(cmds...)
}

func (r *Root) replace(v tea.Model) tea.Cmd {
	if v == nil || len(r.stack) == 0 {
		return nil
	}
	leaving := r.stack[len(r.stack)-1]
	r.stack[len(r.stack)-1] = v
	r.onPop[len(r.onPop)-1] = nil // replacement clears any pending OnPop
	if c, ok := leaving.(Canceller); ok {
		c.Cancel()
	}
	return r.activate(v)
}

// activate runs a view's Init and feeds it the last known window size.
// Returns a tea.Batch of both commands so the view sees Init's command
// and the WindowSizeMsg round-trip before its first render.
//
// Batch ordering is NOT guaranteed: Init's command and the size-update
// command may complete in either order. Views that need strict sequencing
// (e.g. "load data only after first size is known") must chain manually
// via tea.Sequence or by emitting the follow-up command from the handler
// that processes the first result — do not rely on batch order.
func (r *Root) activate(v tea.Model) tea.Cmd {
	cmds := []tea.Cmd{v.Init()}
	if r.hasSize {
		// Synthesize a WindowSizeMsg into the new view so it can size
		// its sub-models (viewports especially) before the first frame.
		updated, cmd := v.Update(r.lastSize)
		r.stack[len(r.stack)-1] = updated
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}
