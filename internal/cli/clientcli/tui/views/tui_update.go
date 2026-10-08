package views

import (
	"errors"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Software update view.
//
// The update routes read the scheduler of whichever node answers and
// take no node selector, so this view is about ONE node: the one the
// client is pointed at. It says so rather than implying the cluster,
// because a worker updating itself is not visible from here.
//
// An install is followed by polling the status, and only by that. The
// apply route can answer with a job id, and there is a stream behind it,
// but it is the wrong thing to watch here for three reasons: a
// successful apply restarts the node, killing the stream on the path
// that worked; a node whose tree is root-owned hands the work to a
// privileged updater and returns no job at all; and a node with no jobs
// registry returns no job either. The status endpoint answers in every
// one of those cases, and carries what a stream cannot — restart
// required, pending confirmation, what the privileged updater did.
// Frame-level progress already has a home in Activity, which subscribes
// to every in-flight job.

type updateViewMode int

const (
	updateViewStatus updateViewMode = iota
	updateViewHistory
	updateViewWatch
)

// updateAction is which action a confirmation or a watch is about.
type updateAction int

const (
	updateActionNone updateAction = iota
	updateActionApply
	updateActionRollback
)

// updatePollInterval paces the status polling that stands in for a
// stream on a delegated install. The privileged updater publishes a run
// record as it goes; two seconds is often enough to see the phases
// without making a busy loop out of it.
//
// A var, not a const, so a test can drive the poll chain without
// waiting on the clock — the thing worth asserting is that a delegated
// watch schedules one at all.
var updatePollInterval = 2 * time.Second

const (
	// updateWatchTimeout bounds that polling. A privileged run that
	// never publishes a finish — the updater was killed, the unit
	// failed to start — must not leave the view watching forever with
	// no way to say so.
	updateWatchTimeout = 10 * time.Minute

	// updateWatchLines caps the frames kept for display, so a long
	// install cannot grow the view without bound.
	updateWatchLines = 12
)

// updateStatusMsg carries a status fetch.
type updateStatusMsg struct {
	status *pkgClient.UpdateStatus
	err    error
	// polled marks the fetches driven by a watch, so an ordinary refresh
	// landing mid-watch does not schedule a second polling chain
	// alongside the first.
	polled bool
	// epoch is the watch this fetch belongs to; a reply outliving its
	// watch is dropped.
	epoch int
}

// updateHistoryMsg carries a history fetch.
type updateHistoryMsg struct {
	entries []pkgClient.UpdateHistoryEntry
	err     error
}

// updateActionMsg carries the answer to an apply or a rollback.
type updateActionMsg struct {
	action updateAction
	result *pkgClient.UpdateActionResult
	err    error
}

// updatePollMsg is the tick that drives a watch. It carries the epoch of
// the watch that scheduled it, so a tick outliving its watch is dropped.
type updatePollMsg struct{ epoch int }

// updateWatch is an install or rollback being followed through the only
// witness there is: the node's own status.
type updateWatch struct {
	action      updateAction
	delegatedTo string
	lines       []string
	done        bool
	err         error
	startedAt   time.Time
	// epoch discards ticks from a watch that has been superseded — a
	// second action started while the first was still polling would
	// otherwise leave two chains running, each re-arming the other.
	epoch int
}

// UpdateViewModel is the software-update view.
type UpdateViewModel struct {
	tui.ViewContext

	client     *pkgClient.Client
	styles     ui.Styles
	termWidth  int
	termHeight int

	loading  bool
	loadTick int
	err      error
	// disabled is the node answering 503: auto-update is switched off
	// here. A state of its own, not an error, because nothing is broken
	// and the fix is a config line.
	disabled bool
	notice   string

	mode    updateViewMode
	status  *pkgClient.UpdateStatus
	history []pkgClient.UpdateHistoryEntry
	list    tui.List

	confirm       tui.Confirm
	confirmAction updateAction

	watch *updateWatch
	// pollEpoch increments per watch; see updateWatch.epoch.
	pollEpoch int
}

// NewUpdateViewModel constructs the software-update view.
func NewUpdateViewModel(client *pkgClient.Client, styles ui.Styles) *UpdateViewModel {
	list := tui.NewList()
	list.SetKeys(tui.NavigationKeys{
		Up: tui.UpdateKeys.Up, Down: tui.UpdateKeys.Down,
		PageUp: tui.UpdateKeys.PageUp, PageDown: tui.UpdateKeys.PageDown,
		Home: tui.UpdateKeys.Home, End: tui.UpdateKeys.End,
	})
	return &UpdateViewModel{
		client:  client,
		styles:  styles,
		loading: true,
		list:    list,
		confirm: tui.NewConfirm(),
	}
}

func (m *UpdateViewModel) Init() tea.Cmd {
	return tea.Batch(m.fetchStatusCmd(false), shared.SpinnerTickCmd())
}

func (m *UpdateViewModel) Breadcrumb() []string {
	title := "Software Update"
	switch m.mode {
	case updateViewHistory:
		return []string{title, "History"}
	case updateViewWatch:
		if m.watch != nil && m.watch.action == updateActionRollback {
			return []string{title, "Rolling back"}
		}
		return []string{title, "Installing"}
	default:
		return []string{title}
	}
}

func (m *UpdateViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
}

func (m *UpdateViewModel) viewContent() string {
	switch m.mode {
	case updateViewHistory:
		return m.viewHistory(m.termWidth)
	case updateViewWatch:
		return m.viewWatch(m.termWidth)
	default:
		return m.viewStatus(m.termWidth)
	}
}

func (m *UpdateViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
		m.list.SetVisible(shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles)))
		return m, nil

	case tui.BreadcrumbClickMsg:
		if msg.Level == 0 {
			return m, tui.NavBack()
		}
		return m, nil

	case shared.SpinnerTickMsg:
		if m.loading || m.watching() {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case updateStatusMsg:
		return m, m.onStatus(msg)

	case updateHistoryMsg:
		m.loading = false
		m.err = msg.err
		m.history = msg.entries
		m.syncHistoryItems()
		return m, nil

	case updateActionMsg:
		return m, m.onActionResult(msg)

	case updatePollMsg:
		if !m.watching() || msg.epoch != m.watch.epoch {
			return m, nil
		}
		return m, m.fetchStatusCmd(true)

	case tea.KeyPressMsg:
		if m.confirmAction != updateActionNone {
			return m, m.updateConfirm(msg)
		}
		return m, m.updateKeys(msg)
	}
	return m, nil
}

// watching reports whether an action is being followed through.
func (m *UpdateViewModel) watching() bool {
	return m.watch != nil && !m.watch.done
}

// delegatedWatch reports a watch whose work belongs to the privileged
// updater, which is read from a different field of the same status.
func (m *UpdateViewModel) delegatedWatch() bool {
	return m.watching() && m.watch.delegatedTo != ""
}

func (m *UpdateViewModel) updateKeys(msg tea.KeyPressMsg) tea.Cmd {
	if m.mode == updateViewHistory && m.list.UpdateKey(msg) {
		return nil
	}

	switch {
	case key.Matches(msg, tui.UpdateKeys.Back):
		if m.mode != updateViewStatus {
			// A finished watch is read, then dismissed; an unfinished
			// one keeps running — the work is on the node, not here.
			m.mode = updateViewStatus
			if m.watch != nil && m.watch.done {
				m.watch = nil
			}
			return m.fetchStatusCmd(false)
		}
		return tui.NavBack()

	case key.Matches(msg, tui.UpdateKeys.Refresh):
		m.notice = ""
		m.loading = true
		if m.mode == updateViewHistory {
			return tea.Batch(m.fetchHistoryCmd(), shared.SpinnerTickCmd())
		}
		return tea.Batch(m.fetchStatusCmd(false), shared.SpinnerTickCmd())

	case key.Matches(msg, tui.UpdateKeys.History):
		m.mode = updateViewHistory
		m.notice = ""
		m.loading = true
		return tea.Batch(m.fetchHistoryCmd(), shared.SpinnerTickCmd())
	}

	if m.mode != updateViewStatus || m.disabled {
		return nil
	}

	switch {
	case key.Matches(msg, tui.UpdateKeys.Check):
		m.notice = "Checking for a newer release…"
		m.loading = true
		return tea.Batch(m.checkCmd(), shared.SpinnerTickCmd())

	case key.Matches(msg, tui.UpdateKeys.Apply):
		if m.watching() {
			return nil
		}
		m.confirmAction = updateActionApply
		return nil

	case key.Matches(msg, tui.UpdateKeys.Rollback):
		if m.watching() {
			return nil
		}
		m.confirmAction = updateActionRollback
		return nil
	}
	return nil
}

func (m *UpdateViewModel) updateConfirm(msg tea.KeyPressMsg) tea.Cmd {
	action := m.confirmAction
	switch m.confirm.UpdateKey(msg) {
	case tui.ConfirmYes:
		m.confirmAction = updateActionNone
		m.notice = ""
		m.loading = true
		return tea.Batch(m.actionCmd(action), shared.SpinnerTickCmd())
	case tui.ConfirmNo, tui.ConfirmCancelled:
		m.confirmAction = updateActionNone
	}
	return nil
}

// onStatus folds a status fetch in, and keeps a watch ticking until the
// node says the work is over.
//
// A status fetch that fails mid-watch is not a failure of the watch: a
// successful apply restarts the node, so every call fails for as long as
// it takes to come back. The poll keeps going until the deadline and the
// node answers again on the far side, which is the point of polling
// rather than streaming.
func (m *UpdateViewModel) onStatus(msg updateStatusMsg) tea.Cmd {
	m.loading = false
	if msg.err != nil {
		if pkgClient.UpdateDisabled(msg.err) {
			m.disabled = true
			m.err = nil
			return nil
		}
		if msg.polled && m.watching() && msg.epoch == m.watch.epoch {
			m.watch.lines = appendWatchLine(m.watch.lines, "waiting for the node to answer again…")
			return m.continueWatch()
		}
		m.err = msg.err
		return nil
	}
	m.disabled = false
	m.err = nil
	m.status = msg.status

	if !msg.polled || !m.watching() || msg.epoch != m.watch.epoch {
		return nil
	}
	if m.recordWatchProgress(msg.status) {
		return nil
	}
	return m.continueWatch()
}

// recordWatchProgress notes where the work has got to and reports
// whether it is over.
//
// The two cases read different fields of the same status. A privileged
// updater publishes a run record, and only that record says when it
// finished. Work this node did itself shows up in the state, which
// settles back to idle or failed when the apply is no longer running.
func (m *UpdateViewModel) recordWatchProgress(s *pkgClient.UpdateStatus) bool {
	if m.delegatedWatch() {
		run := s.PrivilegedRun
		if run == nil {
			return false
		}
		m.watch.lines = appendWatchLine(m.watch.lines, privilegedRunLine(run))
		if !run.Finished() {
			return false
		}
		m.watch.done = true
		if !run.Success {
			m.watch.err = updateRunError(run)
		}
		return true
	}

	m.watch.lines = appendWatchLine(m.watch.lines, localRunLine(s))
	switch s.State {
	case updateStateFailed:
		m.watch.done = true
		m.watch.err = errors.New(updateStateError(s))
		return true
	case updateStateIdle:
		m.watch.done = true
		return true
	}
	return false
}

// continueWatch re-arms the poll, or ends the watch when the node has
// had long enough to say something.
func (m *UpdateViewModel) continueWatch() tea.Cmd {
	if utils.Now().Sub(m.watch.startedAt) > updateWatchTimeout {
		m.watch.done = true
		m.watch.err = errWatchTimedOut
		return nil
	}
	return pollUpdateCmd(m.watch.epoch)
}

// onActionResult starts the watch, unless the node already finished the
// work before it answered.
func (m *UpdateViewModel) onActionResult(msg updateActionMsg) tea.Cmd {
	m.loading = false
	if msg.err != nil {
		switch {
		case pkgClient.UpdateDisabled(msg.err):
			m.disabled = true
		case pkgClient.UpdateNothingToApply(msg.err):
			m.notice = "No update is waiting. Press C to check for one."
		case pkgClient.UpdateNoBackups(msg.err):
			m.notice = "No previous version on this node to roll back to."
		default:
			m.err = msg.err
		}
		return nil
	}

	res := msg.result

	// A rollback this node performed itself is already over when it
	// answers: no job, no handoff, just the backup it restored.
	if msg.action == updateActionRollback && !res.Delegated() {
		m.notice = rollbackDoneNotice(res)
		return m.fetchStatusCmd(false)
	}

	m.pollEpoch++
	m.watch = &updateWatch{
		action:      msg.action,
		delegatedTo: res.DelegatedTo,
		startedAt:   utils.Now(),
		epoch:       m.pollEpoch,
	}
	if res.Delegated() {
		m.watch.lines = []string{
			"Handed to the privileged updater at " + res.DelegatedTo + ".",
			"That work runs in a process this node does not own; watching its status.",
		}
	}
	m.mode = updateViewWatch
	return tea.Batch(pollUpdateCmd(m.watch.epoch), shared.SpinnerTickCmd())
}
