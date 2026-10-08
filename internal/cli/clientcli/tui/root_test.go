package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// fakeView is a minimal tea.Model that records what it received.
type fakeView struct {
	name       string
	initCalls  int
	lastMsg    tea.Msg
	lastSize   tea.WindowSizeMsg
	cancelled  bool
	renderText string
}

func newFake(name string) *fakeView {
	return &fakeView{name: name, renderText: name}
}

func (f *fakeView) Init() tea.Cmd {
	f.initCalls++
	return nil
}
func (f *fakeView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	f.lastMsg = msg
	if s, ok := msg.(tea.WindowSizeMsg); ok {
		f.lastSize = s
	}
	return f, nil
}
func (f *fakeView) View() tea.View { return tea.NewView(f.renderText) }
func (f *fakeView) Cancel()        { f.cancelled = true }

func TestRootInitCallsInitialView(t *testing.T) {
	v := newFake("a")
	r := NewRoot(v)
	r.Init()
	if v.initCalls != 1 {
		t.Fatalf("init calls want 1 got %d", v.initCalls)
	}
}

func TestRootForwardsMessagesToActive(t *testing.T) {
	a := newFake("a")
	r := NewRoot(a)
	r.Update(tea.KeyPressMsg{Text: "x"})
	if a.lastMsg == nil {
		t.Fatal("active view received nothing")
	}
}

func TestRootPushRunsInitAndFeedsSize(t *testing.T) {
	a := newFake("a")
	r := NewRoot(a)
	r.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	b := newFake("b")
	r.Update(NavToMsg{View: b})

	if b.initCalls != 1 {
		t.Fatalf("b Init want 1 got %d", b.initCalls)
	}
	if b.lastSize.Width != 80 || b.lastSize.Height != 24 {
		t.Fatalf("b did not receive size: %+v", b.lastSize)
	}
	if r.Depth() != 2 {
		t.Fatalf("depth want 2 got %d", r.Depth())
	}
	if r.Active() != b {
		t.Fatal("active should be b")
	}
}

func TestRootPopCancelsAndRestoresSize(t *testing.T) {
	a, b := newFake("a"), newFake("b")
	r := NewRoot(a)
	r.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	r.Update(NavToMsg{View: b})
	// After resize while b is active:
	r.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	r.Update(NavBackMsg{})

	if !b.cancelled {
		t.Fatal("popped view should have Cancel() called")
	}
	if a.lastSize.Width != 120 || a.lastSize.Height != 40 {
		t.Fatalf("a should have received latest size, got %+v", a.lastSize)
	}
	if r.Depth() != 1 || r.Active() != a {
		t.Fatalf("depth/active wrong after pop: %d %v", r.Depth(), r.Active())
	}
}

func TestRootPopAtRootEmptiesStack(t *testing.T) {
	// Popping the last frame is allowed: the host detects the empty
	// stack via Depth() and switches to a sibling rail (e.g. menu).
	a := newFake("a")
	r := NewRoot(a)
	r.Update(NavBackMsg{})
	if r.Depth() != 0 {
		t.Fatalf("depth should be 0 after popping last frame, got %d", r.Depth())
	}
	if !a.cancelled {
		t.Fatal("popped view should have been cancelled")
	}
}

func TestRootPopEmptyIsNoop(t *testing.T) {
	r := NewEmptyRoot()
	r.Update(NavBackMsg{})
	if r.Depth() != 0 {
		t.Fatalf("depth should remain 0, got %d", r.Depth())
	}
}

func TestRootReplaceCancelsOldAndActivatesNew(t *testing.T) {
	a, b, c := newFake("a"), newFake("b"), newFake("c")
	r := NewRoot(a)
	r.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	r.Update(NavToMsg{View: b})
	r.Update(NavReplaceMsg{View: c})

	if !b.cancelled {
		t.Fatal("b should be cancelled on replace")
	}
	if c.initCalls != 1 {
		t.Fatalf("c Init want 1 got %d", c.initCalls)
	}
	if r.Depth() != 2 {
		t.Fatalf("depth want 2 got %d", r.Depth())
	}
	if r.Active() != c {
		t.Fatal("active should be c")
	}
}

func TestRootViewDelegatesToActive(t *testing.T) {
	a := newFake("a")
	a.renderText = "hello"
	r := NewRoot(a)
	if got := r.View().Content; got != "hello" {
		t.Fatalf("view content want 'hello' got %q", got)
	}
}

// refreshingView is a view that also implements Refresher. It defines its
// own Update so the model Root stores back after a size round-trip is still
// the Refresher (an embedded fakeView.Update would return the inner value).
type refreshingView struct {
	fakeView
	refreshCalls int
	cmd          tea.Cmd
}

func newRefreshing(name string) *refreshingView {
	return &refreshingView{fakeView: fakeView{name: name, renderText: name}}
}

func (f *refreshingView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	f.fakeView.Update(msg)
	return f, nil
}

func (f *refreshingView) Refresh() tea.Cmd {
	f.refreshCalls++
	return f.cmd
}

func TestRootPopRefreshesRevealedView(t *testing.T) {
	a := newRefreshing("a")
	fired := false
	a.cmd = func() tea.Msg { fired = true; return nil }

	r := NewRoot(a)
	r.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	r.Update(NavToMsg{View: newFake("b")})
	if a.refreshCalls != 0 {
		t.Fatalf("refresh before pop want 0 got %d", a.refreshCalls)
	}

	_, cmd := r.Update(NavBackMsg{})
	if a.refreshCalls != 1 {
		t.Fatalf("refresh calls want 1 got %d", a.refreshCalls)
	}
	if cmd == nil {
		t.Fatal("pop returned no cmd; the refresh cmd was dropped")
	}
	runBatch(cmd)
	if !fired {
		t.Fatal("refresh cmd never ran")
	}
}

// pop() used to return cb() alone; batching Refresh alongside it must not
// drop either cmd when both are non-nil.
func TestRootPopRunsRefreshAndOnPop(t *testing.T) {
	a := newRefreshing("a")
	refreshFired, popFired := false, false
	a.cmd = func() tea.Msg { refreshFired = true; return nil }

	r := NewRoot(a)
	r.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	r.Update(NavToMsg{View: newFake("b"), OnPop: func() tea.Cmd {
		return func() tea.Msg { popFired = true; return nil }
	}})

	_, cmd := r.Update(NavBackMsg{})
	if a.refreshCalls != 1 {
		t.Fatalf("refresh calls want 1 got %d", a.refreshCalls)
	}
	if cmd == nil {
		t.Fatal("pop returned no cmd")
	}
	runBatch(cmd)
	if !refreshFired {
		t.Error("refresh cmd was dropped")
	}
	if !popFired {
		t.Error("OnPop cmd was dropped")
	}
}

func TestRootPopSkipsRefreshOnNonRefresher(t *testing.T) {
	r := NewRoot(newFake("a"))
	r.Update(NavToMsg{View: newFake("b")})
	if _, cmd := r.Update(NavBackMsg{}); cmd != nil {
		t.Fatal("pop of a non-Refresher parent should return no cmd")
	}
}

// runBatch executes cmd and, if it produced a tea.BatchMsg, every command in it.
func runBatch(cmd tea.Cmd) {
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			c()
		}
	}
}
