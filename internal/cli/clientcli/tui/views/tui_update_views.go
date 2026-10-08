package views

import (
	"fmt"
	"strings"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// viewStatus renders what this node knows about its own updates.
func (m *UpdateViewModel) viewStatus(width int) string {
	if m.disabled {
		return m.viewDisabled(width)
	}
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.loading && m.status == nil,
		LoadTick:  m.loadTick,
		Err:       m.err,
		Empty:     m.status == nil,
		EmptyText: "This node reported no update status.",
		BackHint:  tui.Hint(tui.UpdateKeys.Back, "back"),
		EmptyHint: tui.JoinHints(
			tui.Hint(tui.UpdateKeys.Refresh, "refresh"),
			tui.Hint(tui.UpdateKeys.Back, "back"),
		),
	}); done {
		return out
	}

	s := m.status

	d := shared.NewDetail(m.styles, width)
	// Whose updates these are. The routes read the scheduler of the node
	// that answers and take no node selector, so this is one node's
	// business and saying otherwise would be a lie about the cluster.
	d.Text(" ", m.styles.Help.Render(
		"Updates for "+shared.NodeAddress()+": this node only; workers manage their own."))
	d.Write("\n")

	d.Field("State", s.State)
	d.Field("Running", s.CurrentVersion.String())
	d.Field("Channel", s.Channel)
	if s.Source != "" {
		d.Field("Release feed", s.Source)
	}
	if v := s.LatestVersion.String(); v != "" {
		d.Field("Latest seen", v)
	}
	if s.PendingRelease != nil {
		d.Field("Waiting to install", releaseLabel(s.PendingRelease))
	}
	d.Field("Last checked", updateTimeField(s.LastCheckTime))
	d.Field("Next check", updateTimeField(s.NextCheckTime))
	if s.LastUpdateTime != nil {
		d.Field("Last updated", updateTimeField(s.LastUpdateTime))
	}
	if s.DownloadProgress > 0 && s.DownloadProgress < 100 {
		d.Field("Downloading", fmt.Sprintf("%d%%", s.DownloadProgress))
	}

	// Conditions are the longest strings here — an upstream error can
	// run past any width — and Text wraps them into the body rather than
	// letting the terminal fold them where it likes.
	for _, line := range updateConditions(s) {
		d.Text(" ", m.styles.Error.Render(line))
	}
	if s.UpdateAvailable {
		d.Text(" ", m.styles.Success.Render("An update is waiting. Press A to install it."))
	}
	if m.confirmAction != updateActionNone {
		d.Text(" ", m.styles.Error.Render(m.confirmPrompt()))
	}
	if m.notice != "" {
		d.Text(" ", m.styles.Help.Render(m.notice))
	}

	var b strings.Builder
	b.WriteString(d.String())
	b.WriteString(shared.RenderViewFooter(m.styles, width, tui.UpdateKeys.StatusHintsString()))
	return b.String()
}

// confirmPrompt spells out what the key press will do, including the
// part an operator will not think of: the node has to restart to run
// what was installed.
func (m *UpdateViewModel) confirmPrompt() string {
	switch m.confirmAction {
	case updateActionApply:
		target := "the waiting release"
		if m.status != nil && m.status.PendingRelease != nil {
			target = releaseLabel(m.status.PendingRelease)
		}
		return "Install " + target + " on " + shared.NodeAddress() +
			"? The node restarts to run it. (y/n)"
	case updateActionRollback:
		return "Roll " + shared.NodeAddress() + " back to the version it came from? (y/n)"
	}
	return ""
}

// viewDisabled is the 503 answer rendered as a state rather than an
// error: nothing is broken, the feature is switched off, and the fix is
// a line of config.
func (m *UpdateViewModel) viewDisabled(width int) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(m.styles.Title.Render("  Automatic updates are switched off on this node.") + "\n\n")
	b.WriteString(m.styles.Help.Render(
		"  Set update.enabled: true in node.yaml to manage updates from here.") + "\n")
	b.WriteString(m.styles.Help.Render(
		"  History is still readable: press H.") + "\n")
	b.WriteString(shared.RenderViewFooter(m.styles, width, tui.JoinHints(
		tui.Hint(tui.UpdateKeys.History, "history"),
		tui.Hint(tui.UpdateKeys.Refresh, "refresh"),
		tui.Hint(tui.UpdateKeys.Back, "back"),
	)))
	return b.String()
}

// updateConditions lists the things standing between this node and an
// update, each only when it applies.
func updateConditions(s *pkgClient.UpdateStatus) []string {
	var out []string
	if s.Blocked != "" {
		out = append(out, "Blocked: "+s.Blocked)
	}
	if s.DelegatedTo != "" {
		out = append(out, "This node does not install its own updates; a privileged updater does, via "+s.DelegatedTo+".")
	}
	if s.RestartRequired {
		line := "Installed, but still running the previous binary: restart to pick it up."
		if s.RestartRequiredReason != "" {
			line += " (" + s.RestartRequiredReason + ")"
		}
		out = append(out, line)
	}
	if pc := s.PendingConfirm; pc != nil {
		out = append(out, fmt.Sprintf(
			"Running %s unconfirmed after %d boot(s); it rolls back to %s if this node never answers its own health check.",
			pc.ToVersion, pc.Attempts, pc.FromVersion))
	}
	if s.MaintenanceWindowActive {
		out = append(out, "Inside the maintenance window.")
	}
	if s.Error != "" {
		out = append(out, "Last error: "+s.Error)
	}
	return out
}

// viewWatch renders an install or rollback being followed through,
// whether that is a job stream or the status of a run this node cannot
// see into.
func (m *UpdateViewModel) viewWatch(width int) string {
	w := m.watch
	if w == nil {
		return m.viewStatus(width)
	}

	var b strings.Builder
	b.WriteString("\n")
	head := "Installing on " + shared.NodeAddress()
	if w.action == updateActionRollback {
		head = "Rolling back " + shared.NodeAddress()
	}
	b.WriteString(m.styles.Title.Render("  "+head) + "\n")
	if w.delegatedTo != "" {
		b.WriteString(m.styles.Help.Render(
			"  A privileged updater owns this work; there is no job to stream, so this is the node's own status.") + "\n")
	}
	b.WriteString("\n")

	for _, line := range w.lines {
		b.WriteString(shared.WrapIndent(line, "  ", width) + "\n")
	}

	b.WriteString("\n")
	switch {
	case w.err != nil:
		b.WriteString(m.styles.Error.Render("  "+w.err.Error()) + "\n")
	case w.done:
		b.WriteString(m.styles.Success.Render("  Finished. Press Esc for the status.") + "\n")
	default:
		b.WriteString(m.styles.Help.Render(
			"  "+shared.SpinnerFrame(m.loadTick)+" working… Esc leaves this running on the node.") + "\n")
		if w.delegatedTo == "" {
			b.WriteString(m.styles.Help.Render("  "+watchDetailHint) + "\n")
		}
	}

	b.WriteString(shared.RenderViewFooter(m.styles, width, tui.JoinHints(
		tui.Hint(tui.UpdateKeys.Back, "status"),
	)))
	return b.String()
}

// watchDetailHint points at the view that does carry frame-level
// progress. The update job is an ordinary job, and Activity subscribes
// to every one of them.
const watchDetailHint = "Activity shows this job's individual steps."

// viewHistory renders what this node has installed and rolled back.
func (m *UpdateViewModel) viewHistory(width int) string {
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.loading,
		LoadTick:  m.loadTick,
		Err:       m.err,
		Empty:     len(m.history) == 0,
		EmptyText: "This node has not recorded an update yet.",
		BackHint:  tui.Hint(tui.UpdateKeys.Back, "status"),
		EmptyHint: tui.JoinHints(
			tui.Hint(tui.UpdateKeys.Refresh, "refresh"),
			tui.Hint(tui.UpdateKeys.Back, "status"),
		),
	}); done {
		return out
	}

	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
	rows := make([][]string, len(m.history))
	for i, e := range m.history {
		rows[i] = []string{
			e.Timestamp.Format("2006-01-02 15:04"),
			e.FromVersion.String() + " → " + e.ToVersion.String(),
			historyOutcome(e),
			historyTrigger(e),
			formatUpdateDuration(e.Duration),
		}
	}

	start, end := m.list.VisibleRange()
	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"WHEN", "VERSION", "RESULT", "TRIGGER", "TOOK"},
		Width:     width,
		Vis:       vis,
		Cursor:    m.list.Cursor(),
		Offset:    m.list.Offset(),
		Styles:    m.styles,
		StatusCol: -1,
		AllRows:   rows,
	})
	for _, row := range rows[start:end] {
		t.Row(row...)
	}

	var b strings.Builder
	b.WriteString(t.Render() + "\n")
	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width,
		shared.TableStatus("Updates", len(m.history), m.list.Offset(), vis),
		tui.UpdateKeys.HistoryHintsString()))
	return b.String()
}

// historyOutcome says what happened, with the reason when it failed —
// a bare "failed" sends the operator to the logs for something the
// record already holds.
func historyOutcome(e pkgClient.UpdateHistoryEntry) string {
	if e.Success {
		return "ok"
	}
	if e.Error != "" {
		return "failed: " + e.Error
	}
	return "failed"
}

// historyTrigger separates the updates a person asked for from the ones
// the scheduler took on its own.
func historyTrigger(e pkgClient.UpdateHistoryEntry) string {
	if e.Automatic {
		return "scheduled"
	}
	return "requested"
}

// formatUpdateDuration renders how long an update took. The API sends
// nanoseconds; seconds is the resolution an operator cares about.
func formatUpdateDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// releaseLabel names a release the way its publisher does, falling back
// to the parsed version when the tag is missing.
func releaseLabel(r *pkgClient.UpdateRelease) string {
	if r == nil {
		return ""
	}
	if r.TagName != "" {
		return r.TagName
	}
	return r.Version.String()
}

// updateTimeField renders a timestamp, or says plainly that there is
// none rather than leaving a blank an operator has to interpret.
func updateTimeField(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
