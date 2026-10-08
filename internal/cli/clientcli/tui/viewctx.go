package tui

import "context"

// ViewContext pairs a context.Context with its cancel func for a single view.
// Root calls Cancel() when the view leaves the stack; in-flight tea.Cmd
// functions should check Context().Err() before returning and return nil
// if cancelled (v2 tolerates nil messages from Cmds).
//
// Zero value is usable — Context() lazily creates a live context on first
// call, so views that embed ViewContext don't need a special constructor.
//
// Typical use:
//
//	type myView struct {
//	    tui.ViewContext
//	    ...
//	}
//
//	func (v *myView) load() tea.Cmd {
//	    ctx := v.Context()
//	    return func() tea.Msg {
//	        data, err := api.Get(ctx, ...)
//	        if ctx.Err() != nil { return nil }
//	        return loadedMsg{data, err}
//	    }
//	}
type ViewContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// NewViewContext returns a fresh, live ViewContext. Equivalent to the
// zero value followed by Context(); prefer the zero value in view structs.
func NewViewContext() ViewContext {
	ctx, cancel := context.WithCancel(context.Background())
	return ViewContext{ctx: ctx, cancel: cancel}
}

// Context returns the view's context, lazy-creating it on first call. Safe
// to call after Cancel — returns the cancelled context.
//
// NOT goroutine-safe: the lazy-init assignment races if two concurrent
// callers both hit the nil branch. Relies on Bubbletea v2's single-threaded
// message pump — all Update calls run serially, so views that only touch
// Context() from Update/View (or from the Cmd closure that captures it) are
// safe. Do NOT call Context() from a goroutine started outside a tea.Cmd.
func (v *ViewContext) Context() context.Context {
	if v.ctx == nil {
		v.ctx, v.cancel = context.WithCancel(context.Background())
	}
	return v.ctx
}

// Cancel satisfies the Canceller interface. Safe to call multiple times
// and safe on the zero value (no-op if Context() was never called).
func (v *ViewContext) Cancel() {
	if v.cancel != nil {
		v.cancel()
	}
}
