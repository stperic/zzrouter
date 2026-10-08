package views

import (
	"errors"
	"testing"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
)

// Refresh runs when a child view pops. While that child was on top it was the
// only view receiving messages, so this view's pending replies and poll timer
// went there and were dropped. Refresh must reload anyway — respecting the
// stale guards would leave the view showing pre-child state indefinitely.
func TestModelsRefreshReloadsDespiteStaleInFlightGuards(t *testing.T) {
	m := &ModelsViewModel{loading: true, fetching: true, ticking: true}

	cmd := m.Refresh()

	if cmd == nil {
		t.Fatal("Refresh returned no cmd; stale guards suppressed the reload")
	}
	if m.loading {
		t.Error("loading still set; the view would render the loading prelude over its content")
	}
	if m.ticking {
		t.Error("ticking still set; modelsLoadedMsg will not restart the poll timer")
	}
	if !m.fetching {
		t.Error("fetching not set; the footer spinner will not show during the reload")
	}
}

// The footer spinner is the only signal that a silent reload is running, so it
// must be absent when idle and present while fetching, without a status line
// that would shift the body.
func TestModelsRefreshHintOnlyWhileFetching(t *testing.T) {
	m := &ModelsViewModel{}
	if got := m.withRefreshHint("esc back", 80); got != "esc back" {
		t.Errorf("idle hints want unchanged, got %q", got)
	}

	m.fetching = true
	if got := m.withRefreshHint("esc back", 80); got == "esc back" {
		t.Error("fetching hints want a spinner appended, got unchanged")
	}
}

// Appending the spinner is only safe while it fits: a wrapped footer shifts
// the body by the row the inline hint exists to avoid.
func TestModelsRefreshHintDroppedWhenItWouldWrap(t *testing.T) {
	m := &ModelsViewModel{fetching: true}
	hints := "navigate  details  new route  sort  refresh  back"

	if got := m.withRefreshHint(hints, len(hints)+8); got != hints {
		t.Errorf("narrow terminal want hints unchanged, got %q", got)
	}
	if got := m.withRefreshHint(hints, len(hints)+40); got == hints {
		t.Error("wide terminal want a spinner appended, got unchanged")
	}
}

// Two model groups can list the same replica, so their child rows share a
// kind/node/model. Without the route in the key, restoreCursor would snap the
// selection to whichever route happened to sort first.
func TestModelsRowIDDistinguishesRoutesSharingAReplica(t *testing.T) {
	rowIn := func(route string) modelRow {
		return modelRow{Kind: rowKindDeployChild, Node: "worker-1", RawModel: "qwen3", routeName: route}
	}
	a, b := rowIn("fast"), rowIn("cheap")

	if a.id() == b.id() {
		t.Fatalf("same id for rows in different routes: %+v", a.id())
	}

	m := &ModelsViewModel{rows: []modelRow{a, b}}
	m.syncListItems()
	m.restoreCursor(b.id(), true)
	if got := m.list.Cursor(); got != 1 {
		t.Errorf("cursor want 1 (the cheap route child) got %d", got)
	}
}

// A reload with nothing selected must leave the cursor alone rather than
// matching a row that happens to have empty node/model fields.
func TestModelsRestoreCursorNoopsWithoutSelection(t *testing.T) {
	m := &ModelsViewModel{rows: []modelRow{{Kind: rowKindModel}, {Kind: rowKindModel, RawModel: "qwen3"}}}
	m.syncListItems()
	m.list.SetCursor(1)

	m.restoreCursor(rowID{}, false)

	if got := m.list.Cursor(); got != 1 {
		t.Errorf("cursor moved on a no-selection restore: want 1 got %d", got)
	}
}

// The spinner has to advance while fetching, not just while loading, or it
// renders as a frozen glyph for the whole reload.
func TestModelsSpinnerTicksWhileFetching(t *testing.T) {
	m := &ModelsViewModel{fetching: true}

	_, cmd := m.Update(shared.SpinnerTickMsg{})

	if m.loadTick == 0 {
		t.Error("loadTick did not advance while fetching")
	}
	if cmd == nil {
		t.Error("tick loop not rescheduled; spinner would stop after one frame")
	}
}

// The 30s poll is a one-shot tick re-armed only by its own handler, so the
// message dropped while a child view was on top kills the chain for the rest
// of the session. Refresh must arm a fresh one.
func TestModelsRefreshRearmsAutoPoll(t *testing.T) {
	m := &ModelsViewModel{}
	before := m.pollEpoch

	m.Refresh()

	if m.pollEpoch == before {
		t.Fatal("pollEpoch unchanged; a late message from the dead chain would still be honoured")
	}
	// A message from the retired chain must not re-arm a second poll.
	_, cmd := m.Update(modelsAutoRefreshMsg{epoch: before})
	if cmd != nil {
		t.Error("retired chain re-armed; two polls would now run in parallel")
	}
}

// Refresh runs while a spinner chain may already be live; arming a second
// doubles the tick rate.
func TestModelsRefreshDoesNotDoubleSpinnerChain(t *testing.T) {
	m := &ModelsViewModel{spinning: true}
	m.Refresh()
	if !m.spinning {
		t.Error("spinning cleared by Refresh")
	}

	fresh := &ModelsViewModel{}
	fresh.Refresh()
	if !fresh.spinning {
		t.Error("Refresh did not arm a spinner chain when none was live")
	}
}

// A transient error on a background reload used to replace the whole list
// with an error page, because msg.rows is nil on failure.
func TestModelsFailedRefreshKeepsContent(t *testing.T) {
	m := &ModelsViewModel{rows: []modelRow{{Kind: rowKindModel, RawModel: "qwen3"}}}
	m.syncListItems()

	m.Update(modelsLoadedMsg{err: errRefreshFailed})

	if len(m.rows) != 1 {
		t.Fatalf("rows wiped by a failed reload: got %d", len(m.rows))
	}
	if m.err != nil {
		t.Error("err set; the view would render an error page over its content")
	}
	if m.statusMsg == "" {
		t.Error("failure not surfaced in the status line")
	}
}

// With nothing on screen there is no content to protect, so the error must
// still reach the prelude.
func TestModelsFailedFirstLoadShowsError(t *testing.T) {
	m := &ModelsViewModel{}

	m.Update(modelsLoadedMsg{err: errRefreshFailed})

	if m.err == nil {
		t.Error("first-load failure swallowed; the view would show an empty list")
	}
}

var errRefreshFailed = errors.New("coordinator unreachable")
