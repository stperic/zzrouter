package views

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// errWatchTimedOut ends a delegated watch that never saw a finish. The
// update itself may still complete — the updater runs in its own
// process — so the wording sends the operator back to the status rather
// than declaring a failure.
var errWatchTimedOut = errors.New(
	"the privileged updater has not reported a result yet; press R on the status to check again")

// fetchStatusCmd reads this node's update status. polled marks the
// fetches a watch drives, so an ordinary refresh cannot be mistaken for
// one and start a second polling chain.
//
// The context check is what stops a popped view from making requests:
// ViewContext.Cancel cancels the context, not the timer, so a tick
// scheduled before the pop still fires.
func (m *UpdateViewModel) fetchStatusCmd(polled bool) tea.Cmd {
	client, ctx := m.client, m.Context()
	epoch := m.pollEpoch
	return func() tea.Msg {
		status, err := client.GetUpdateStatus()
		if ctx.Err() != nil {
			return nil
		}
		return updateStatusMsg{status: status, err: err, polled: polled, epoch: epoch}
	}
}

// fetchHistoryCmd reads what this node has installed and rolled back,
// newest first.
func (m *UpdateViewModel) fetchHistoryCmd() tea.Cmd {
	client, ctx := m.client, m.Context()
	return func() tea.Msg {
		history, err := client.GetUpdateHistory()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return updateHistoryMsg{err: err}
		}
		entries := history.Entries
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Timestamp.After(entries[j].Timestamp)
		})
		return updateHistoryMsg{entries: entries}
	}
}

// checkCmd asks the node to consult its release feed now, then re-reads
// the status: the check's own answer says whether something is
// available, but the status is what the rest of the view renders.
func (m *UpdateViewModel) checkCmd() tea.Cmd {
	client, ctx := m.client, m.Context()
	return func() tea.Msg {
		if _, err := client.CheckForUpdate(); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return updateStatusMsg{err: err}
		}
		status, err := client.GetUpdateStatus()
		if ctx.Err() != nil {
			return nil
		}
		return updateStatusMsg{status: status, err: err}
	}
}

// actionCmd runs an apply or a rollback.
func (m *UpdateViewModel) actionCmd(action updateAction) tea.Cmd {
	client, ctx := m.client, m.Context()
	return func() tea.Msg {
		var (
			res *pkgClient.UpdateActionResult
			err error
		)
		if action == updateActionRollback {
			res, err = client.RollbackUpdate()
		} else {
			res, err = client.ApplyUpdate()
		}
		if ctx.Err() != nil {
			return nil
		}
		return updateActionMsg{action: action, result: res, err: err}
	}
}

// pollUpdateCmd schedules the next poll, stamped with the epoch of the
// watch that asked for it.
func pollUpdateCmd(epoch int) tea.Cmd {
	return tea.Tick(updatePollInterval, func(time.Time) tea.Msg {
		return updatePollMsg{epoch: epoch}
	})
}

// Update states this view branches on. Repeated rather than imported:
// client-side code does not depend on pkg/update.
const (
	updateStateIdle   = "idle"
	updateStateFailed = "failed"
)

// localRunLine renders where an install this node is doing itself has
// got to.
func localRunLine(s *pkgClient.UpdateStatus) string {
	if s == nil || s.State == "" {
		return ""
	}
	if s.DownloadProgress > 0 && s.DownloadProgress < 100 {
		return fmt.Sprintf("%s · %d%%", s.State, s.DownloadProgress)
	}
	return s.State
}

// updateStateError reports why a failed apply failed, falling back to
// wording that at least says where to look.
func updateStateError(s *pkgClient.UpdateStatus) string {
	if s != nil && s.Error != "" {
		return s.Error
	}
	return "the node reported the update as failed without saying why"
}

// appendWatchLine adds a line, dropping consecutive repeats — a poll
// that finds the same phase twice should not print it twice — and keeps
// the tail bounded.
func appendWatchLine(lines []string, line string) []string {
	if line == "" {
		return lines
	}
	if n := len(lines); n > 0 && lines[n-1] == line {
		return lines
	}
	lines = append(lines, line)
	if len(lines) > updateWatchLines {
		lines = lines[len(lines)-updateWatchLines:]
	}
	return lines
}

// privilegedRunLine renders what the privileged updater last published.
func privilegedRunLine(run *pkgClient.UpdatePrivilegedRun) string {
	if run == nil {
		return ""
	}
	parts := []string{run.Action}
	if run.Phase != "" {
		parts = append(parts, run.Phase)
	} else if run.State != "" {
		parts = append(parts, run.State)
	}
	if run.Progress > 0 {
		parts = append(parts, fmt.Sprintf("%d%%", run.Progress))
	}
	if run.Finished() {
		if run.Success {
			parts = append(parts, "finished")
		} else {
			parts = append(parts, "failed")
		}
	}
	return strings.Join(parts, " · ")
}

// updateRunError turns a failed privileged run into an error a person
// can read.
func updateRunError(run *pkgClient.UpdatePrivilegedRun) error {
	if run.Error != "" {
		return errors.New(run.Error)
	}
	return fmt.Errorf("the privileged updater reported %s as failed", run.Action)
}

// rollbackDoneNotice describes a rollback the node performed itself. It
// says restart, because a rollback swaps the binary and the running
// process is still the one being replaced.
func rollbackDoneNotice(res *pkgClient.UpdateActionResult) string {
	if res != nil && res.BackupPath != "" {
		return "Rolled back to " + res.BackupPath + ". The new version takes effect on the next restart."
	}
	return "Rolled back. The new version takes effect on the next restart."
}

// historyItem wraps an entry to satisfy tui.Item.
type historyItem struct{ pkgClient.UpdateHistoryEntry }

func (h historyItem) Title() string { return h.ToVersion.String() }
func (h historyItem) Description() string {
	return h.Timestamp.Format(time.RFC3339)
}
func (h historyItem) FilterValue() string {
	return h.FromVersion.String() + " " + h.ToVersion.String()
}

func (m *UpdateViewModel) syncHistoryItems() {
	items := make([]tui.Item, len(m.history))
	for i, e := range m.history {
		items[i] = historyItem{e}
	}
	m.list.SetItems(items)
}
