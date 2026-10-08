package views

// Run logs TUI screen: picker (when multiple runs match a filter) plus
// full-screen live-tailing viewer for a chosen run. Composes the shared
// primitives from tui_logview.go and consumes logs.RunSession events.

import (
	"context"
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	logsclient "github.com/stperic/zzrouter/internal/client/logs"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// runItem wraps logsclient.Run to satisfy tui.Item for the picker.
type runItem struct{ logsclient.Run }

func (r runItem) Title() string       { return r.ID }
func (r runItem) Description() string { return r.Model }
func (r runItem) FilterValue() string { return r.ID + " " + r.Model }

// ============================================================================
// Messages
// ============================================================================

// runlogsRunsLoadedMsg is the result of the initial ResolveRuns call.
type runlogsRunsLoadedMsg struct {
	runs []logsclient.Run
	err  error
}

// runlogsSessionStartedMsg carries a newly-constructed RunSession (or
// the error from trying to build one).
type runlogsSessionStartedMsg struct {
	session *logsclient.RunSession
	err     error
}

// runlogsEventMsg wraps one event from the session event channel.
type runlogsEventMsg struct {
	ev logsclient.Event
}

// runlogsStreamDoneMsg is sent when the event channel closes.
type runlogsStreamDoneMsg struct{}

// ============================================================================
// Mode and model
// ============================================================================

type runlogsViewMode int

const (
	runlogsViewLoading runlogsViewMode = iota
	runlogsViewPicker
	runlogsViewViewer
)

type RunlogsViewModel struct {
	tui.ViewContext

	client *pkgClient.Client
	styles ui.Styles
	Filter logsclient.RunFilter

	mode       runlogsViewMode
	termWidth  int
	termHeight int
	loadTick   int
	err        error

	// Picker state
	runs []logsclient.Run
	list tui.List

	// Viewer state
	run     logsclient.Run
	session *logsclient.RunSession
	ring    *logLineRing
	follow  followState
	search  logSearchState

	// Viewer scroll: index into the ring of the line shown at the top
	// of the viewport. When follow==followLive, this is maintained to
	// keep the last line pinned to the bottom.
	scrollTop int

	// totalLines is the cumulative count of lines ever emitted — not
	// the ring's current Len(). Shown in the footer alongside the
	// buffer size.
	totalLines int

	// pendingNew tracks how many new lines arrived while the viewer
	// was paused. Displayed as "+N new" in the status line and reset
	// when follow goes LIVE.
	pendingNew int
}

func NewRunlogsViewModel(client *pkgClient.Client, styles ui.Styles, filter logsclient.RunFilter) *RunlogsViewModel {
	list := tui.NewList()
	list.SetKeys(tui.NavigationKeys{
		Up:       tui.RunlogsKeys.Up,
		Down:     tui.RunlogsKeys.Down,
		PageUp:   tui.RunlogsKeys.PageUp,
		PageDown: tui.RunlogsKeys.PageDown,
		Home:     tui.RunlogsKeys.Home,
		End:      tui.RunlogsKeys.End,
	})
	return &RunlogsViewModel{
		client: client,
		styles: styles,
		Filter: filter,
		mode:   runlogsViewLoading,
		ring:   newLogLineRing(logViewDefaultCap),
		follow: followLive,
		list:   list,
	}
}

// Init implements tea.Model.
func (m *RunlogsViewModel) Init() tea.Cmd {
	return tea.Batch(m.fetchRunsCmd(), shared.SpinnerTickCmd())
}

// Refresh implements tui.Refresher: reload the run list when a child view
// pops, since runs may have started or ended behind it.
func (m *RunlogsViewModel) Refresh() tea.Cmd { return m.fetchRunsCmd() }

func (m *RunlogsViewModel) fetchRunsCmd() tea.Cmd {
	client := m.client
	filter := m.Filter
	return func() tea.Msg {
		runs, err := logsclient.ResolveRuns(context.Background(), client, filter)
		return runlogsRunsLoadedMsg{runs: runs, err: err}
	}
}

// ============================================================================
// Update
// ============================================================================

// Update implements tea.Model.
func (m *RunlogsViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
		m.list.SetVisible(shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles)))
		return m, nil

	case tui.BreadcrumbClickMsg:
		// Level 0 (root) → leave the view entirely.
		if msg.Level == 0 {
			return m, tui.NavBack()
		}
		// Viewer with multiple runs collapses back to picker.
		if m.mode == runlogsViewViewer && msg.Level == 1 && len(m.runs) > 1 {
			m.closeSession()
			m.mode = runlogsViewPicker
			return m, nil
		}
		return m, tui.NavBack()

	case shared.SpinnerTickMsg:
		if m.mode == runlogsViewLoading {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case runlogsRunsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.mode = runlogsViewPicker // show the error in a familiar frame
			return m, nil
		}
		m.runs = msg.runs
		items := make([]tui.Item, len(msg.runs))
		for i, r := range msg.runs {
			items[i] = runItem{r}
		}
		m.list.SetItems(items)
		if len(msg.runs) == 1 {
			return m, m.enterViewer(msg.runs[0])
		}
		m.mode = runlogsViewPicker
		m.list.SetCursor(0)
		return m, nil

	case runlogsSessionStartedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.session = msg.session
		return m, waitForRunlogsEvent(m.session.Events())

	case runlogsEventMsg:
		return m, m.handleEvent(msg.ev)

	case runlogsStreamDoneMsg:
		// Event channel closed. Underlying run transitioned to
		// stopped, or we cancelled.
		m.follow = followStopped
		m.session = nil
		return m, nil

	case tea.MouseClickMsg:
		return m, m.handleMouseClick(msg)

	case tea.MouseWheelMsg:
		m.handleMouseWheel(msg)
		return m, nil

	case tea.KeyPressMsg:
		switch m.mode {
		case runlogsViewPicker:
			return m, m.updatePicker(msg)
		case runlogsViewViewer:
			return m, m.updateViewer(msg)
		}
	}
	return m, nil
}

// handleMouseClick routes right-click (back) and left-click (select)
// events across picker and viewer modes.
func (m *RunlogsViewModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button == tea.MouseRight {
		switch m.mode {
		case runlogsViewViewer:
			if m.search.active {
				m.search.Reset()
				return nil
			}
			if len(m.runs) > 1 {
				m.closeSession()
				m.mode = runlogsViewPicker
				return nil
			}
			return tui.NavBack()
		case runlogsViewPicker, runlogsViewLoading:
			return tui.NavBack()
		}
		return nil
	}
	if m.mode != runlogsViewPicker {
		return nil
	}
	enter, ok := m.list.Click(msg, shared.TuiTableClickY)
	if ok && enter {
		return m.enterViewer(m.runs[m.list.Cursor()])
	}
	return nil
}

// handleMouseWheel routes wheel events to picker list scrolling or
// viewer log scrolling.
func (m *RunlogsViewModel) handleMouseWheel(msg tea.MouseWheelMsg) {
	switch m.mode {
	case runlogsViewPicker:
		m.list.UpdateWheel(msg)
	case runlogsViewViewer:
		visible := m.viewerVisibleRows()
		switch msg.Button {
		case tea.MouseWheelUp:
			if m.scrollTop > 0 {
				m.scrollTop--
				m.follow = m.pauseIfLive()
			}
		case tea.MouseWheelDown:
			m.scrollDownOne(visible)
		}
	}
}

// handleEvent folds one logs.Event into the viewer state and re-arms
// the channel read.
func (m *RunlogsViewModel) handleEvent(ev logsclient.Event) tea.Cmd {
	switch e := ev.(type) {
	case logsclient.EventEntry:
		// A successful entry clears any stale transient error.
		m.err = nil
		m.ring.Append(e.Entry.Text)
		m.totalLines++
		if m.follow == followLive {
			m.scrollToBottom()
		} else {
			m.pendingNew++
		}
		if m.search.active {
			m.search.Recompute(m.ring)
		}
	case logsclient.EventReset:
		m.ring.Reset()
		m.totalLines = 0
		m.pendingNew = 0
		m.scrollTop = 0
		if m.search.active {
			m.search.Recompute(m.ring)
		}
	case logsclient.EventDone:
		m.follow = followStopped
	case logsclient.EventError:
		// Surface the error but keep the stream alive. Next event
		// may be valid.
		m.err = e.Err
	}
	if m.session != nil {
		return waitForRunlogsEvent(m.session.Events())
	}
	return nil
}

// enterViewer transitions from picker → viewer and kicks off the
// session-creation tea.Cmd.
func (m *RunlogsViewModel) enterViewer(run logsclient.Run) tea.Cmd {
	m.run = run
	m.mode = runlogsViewViewer
	m.ring = newLogLineRing(logViewDefaultCap)
	m.follow = followLive
	m.err = nil
	m.scrollTop = 0
	m.totalLines = 0
	m.pendingNew = 0
	m.search.Reset()

	client := m.client
	runID := run.ID
	return func() tea.Msg {
		sess, err := logsclient.NewRunSession(context.Background(), client, runID)
		return runlogsSessionStartedMsg{session: sess, err: err}
	}
}

// closeSession cancels the live session if one exists. Safe to call
// multiple times.
func (m *RunlogsViewModel) closeSession() {
	if m.session != nil {
		m.session.Close()
		m.session = nil
	}
}

// Cancel implements tui.Canceller. Root invokes this when the view leaves
// the stack so the live session can shut down.
func (m *RunlogsViewModel) Cancel() { m.closeSession() }

// waitForRunlogsEvent returns a tea.Cmd that blocks on the session
// event channel and wraps the next event as a tea.Msg. When the
// channel closes, it returns runlogsStreamDoneMsg.
func waitForRunlogsEvent(ch <-chan logsclient.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return runlogsStreamDoneMsg{}
		}
		return runlogsEventMsg{ev: ev}
	}
}

// ============================================================================
// Picker-mode input
// ============================================================================

func (m *RunlogsViewModel) updatePicker(msg tea.KeyPressMsg) tea.Cmd {
	if m.list.UpdateKey(msg) {
		return nil
	}
	switch {
	case key.Matches(msg, tui.RunlogsKeys.Enter):
		c := m.list.Cursor()
		if c >= 0 && c < len(m.runs) {
			return m.enterViewer(m.runs[c])
		}
	case key.Matches(msg, tui.RunlogsKeys.Refresh):
		m.mode = runlogsViewLoading
		return tea.Batch(m.fetchRunsCmd(), shared.SpinnerTickCmd())
	case key.Matches(msg, tui.RunlogsKeys.Back):
		return tui.NavBack()
	case key.Matches(msg, tui.RunlogsKeys.Quit):
		return tea.Quit
	}
	return nil
}

// ============================================================================
// Viewer-mode input
// ============================================================================

// ============================================================================
// Breadcrumb + View
// ============================================================================

// Breadcrumb implements tui.Breadcrumber.
func (m *RunlogsViewModel) Breadcrumb() []string {
	switch m.mode {
	case runlogsViewLoading:
		return []string{"Logs"}
	case runlogsViewPicker:
		return []string{"Logs", pickerTitle(m.Filter)}
	case runlogsViewViewer:
		return []string{"Logs", fmt.Sprintf("%s · %s", m.run.Provider, shortID(m.run.ID))}
	}
	return []string{"Logs"}
}

// View implements tea.Model.
func (m *RunlogsViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
}

func (m *RunlogsViewModel) viewContent() string {
	width := m.termWidth
	switch m.mode {
	case runlogsViewLoading:
		return m.viewLoading(width)
	case runlogsViewPicker:
		return m.viewPicker(width)
	case runlogsViewViewer:
		return m.viewViewer(width)
	}
	return ""
}

func (m *RunlogsViewModel) viewLoading(width int) string {
	out, _ := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:     true,
		LoadTick:    m.loadTick,
		LoadingText: "Loading runs...",
		BackHint:    tui.Hint(tui.RunlogsKeys.Back, "back"),
	})
	return out
}

func (m *RunlogsViewModel) viewPicker(width int) string {
	errEmptyHints := tui.JoinHints(
		tui.Hint(tui.RunlogsKeys.Refresh, "refresh"),
		tui.Hint(tui.RunlogsKeys.Back, "back"),
	)
	retryBackHints := tui.JoinHints(
		tui.Hint(tui.RunlogsKeys.Refresh, "retry"),
		tui.Hint(tui.RunlogsKeys.Back, "back"),
	)
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Err:       m.err,
		Empty:     len(m.runs) == 0,
		EmptyText: "No runs match filter",
		BackHint:  retryBackHints,
		EmptyHint: errEmptyHints,
	}); done {
		return out
	}

	var b strings.Builder
	allRows := make([][]string, len(m.runs))
	for i, r := range m.runs {
		allRows[i] = []string{shortID(r.ID), r.Model, r.Node, r.Status, fmtStartedAt(r.StartedAt)}
	}

	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
	start, end := m.list.VisibleRange()

	rowStyles := make([]lipgloss.Style, end-start)
	for i, r := range m.runs[start:end] {
		rowStyles[i] = shared.StatusStyle(m.styles, r.Status)
	}

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"RUN ID", "MODEL", "NODE", "STATUS", "STARTED"},
		Width:     width,
		Vis:       vis,
		Cursor:    m.list.Cursor(),
		Offset:    m.list.Offset(),
		Styles:    m.styles,
		StatusCol: 3,
		RowStyles: rowStyles,
		AllRows:   allRows,
	})
	for _, row := range allRows[start:end] {
		t.Row(row...)
	}
	b.WriteString(t.Render() + "\n")

	status := fmt.Sprintf("%s  •  %d runs", pickerTitle(m.Filter), len(m.runs))
	hints := tui.JoinHints(
		tui.Hint(tui.RunlogsKeys.Up, "navigate"),
		tui.Hint(tui.RunlogsKeys.Enter, "open"),
		tui.Hint(tui.RunlogsKeys.Refresh, "refresh"),
		tui.Hint(tui.RunlogsKeys.Back, "back"),
	)
	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, status, hints))
	return b.String()
}

// ============================================================================
// Rendering helpers
// ============================================================================

// pickerTitle derives a human-readable title from the current filter.
func pickerTitle(f logsclient.RunFilter) string {
	var parts []string
	if f.Provider != "" {
		parts = append(parts, "provider="+f.Provider)
	}
	if f.Model != "" {
		parts = append(parts, "model="+f.Model)
	}
	if f.Node != "" {
		parts = append(parts, "node="+f.Node)
	}
	if f.Status != "" {
		parts = append(parts, "status="+f.Status)
	}
	if f.RunID != "" {
		parts = append(parts, "id~"+f.RunID)
	}
	if len(parts) == 0 {
		return "all runs"
	}
	return strings.Join(parts, " ")
}

// shortID truncates a run ID to a fixed prefix for display in tables
// and headers. Long hash IDs are hard to read in full.
const shortIDLen = 8

func shortID(id string) string {
	if len(id) <= shortIDLen {
		return id
	}
	return id[:shortIDLen] + "…"
}

// fmtStartedAt formats an RFC3339 timestamp as HH:MM, or empty when
// the input is unparseable. Matches the existing provider tables'
// compact style.
func fmtStartedAt(ts string) string {
	if len(ts) < 16 {
		return ts
	}
	// RFC3339: 2026-04-11T14:02:33Z → take positions 11..16 = "14:02"
	return ts[11:16]
}
