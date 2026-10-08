package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

type stackCancellable struct {
	cancelled bool
}

func (s *stackCancellable) Init() tea.Cmd                       { return nil }
func (s *stackCancellable) Update(tea.Msg) (tea.Model, tea.Cmd) { return s, nil }
func (s *stackCancellable) View() tea.View                      { return tea.NewView("") }
func (s *stackCancellable) Cancel()                             { s.cancelled = true }

type stackPlain struct{}

func (stackPlain) Init() tea.Cmd                       { return nil }
func (stackPlain) Update(tea.Msg) (tea.Model, tea.Cmd) { return stackPlain{}, nil }
func (stackPlain) View() tea.View                      { return tea.NewView("") }

func TestTeardownAll_CancelsEveryStackedView(t *testing.T) {
	a := &stackCancellable{}
	b := &stackCancellable{}
	r := NewRoot(a)
	r.push(b, nil)
	r.TeardownAll()
	if !a.cancelled || !b.cancelled {
		t.Fatalf("TeardownAll must call Cancel on every stacked view (a=%v b=%v)", a.cancelled, b.cancelled)
	}
}

func TestTeardownAll_SkipsNonCancellable(t *testing.T) {
	r := NewRoot(stackPlain{})
	r.push(&stackCancellable{}, nil)
	r.TeardownAll() // must not panic on the plain view
}

func TestTeardownAll_NilSafeAndEmptyStackSafe(t *testing.T) {
	var r *Root
	r.TeardownAll() // nil-safe
	r2 := NewEmptyRoot()
	r2.TeardownAll() // empty stack
}
