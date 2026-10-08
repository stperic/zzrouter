package views

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/jobstream"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Upgrade progress streaming.
//
// Upgrade returns 202 + job_id exactly as install does, but the prompt used
// to report success the moment that 202 arrived. That reads as "the upgrade
// finished" when it only means "the request was accepted": the install runs
// on for however long it takes, and could still fail. This subscribes to the
// job so the status line tracks the real work and only claims success on the
// terminal frame.

// upgradeJob is the whole of an in-flight upgrade: the subscription, who it
// is for, and the last frame it produced.
//
// The frame is kept because progress is emitted per step, and a step can be
// silent for minutes — vLLM's install step covers a multi-GB pip resolve.
// Re-rendering the last frame on every tick, against the time it arrived, is
// what lets a slow step read as slow rather than hung.
//
// One pointer rather than four sibling fields, so "no upgrade in flight" is a
// single nil and ending one is a single assignment.
type upgradeJob struct {
	row     *jobstream.Row
	name    string
	frame   pkgClient.JobEvent
	frameAt time.Time
}

func newUpgradeJob(row *jobstream.Row, name string) *upgradeJob {
	// frameAt starts now, not at the first frame: the wait before a job says
	// anything at all is exactly when a caller most needs to see it is alive.
	return &upgradeJob{row: row, name: name, frameAt: utils.Now()}
}

// providersUpgradeJobSubscribedMsg carries a subscribed upgrade job, or the
// error from the POST that was meant to start it.
type providersUpgradeJobSubscribedMsg struct {
	row  *jobstream.Row
	next tea.Cmd
	name string
	err  error
}

// startUpgradeJobCmd POSTs the upgrade and subscribes to the returned job in
// one shot, mirroring startInstallJobCmd.
func startUpgradeJobCmd(ctx context.Context, client *pkgClient.Client, name, version, node string) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.UpgradeProvider(name, version, node)
		if err != nil {
			return providersUpgradeJobSubscribedMsg{name: name, err: err}
		}
		if resp == nil || resp.JobID == "" {
			// Nothing to stream. Not an error: report it as done rather
			// than inventing a job that will never emit a frame.
			return providersActionMsg{action: "upgrade", name: name}
		}
		row, next := jobstream.Subscribe(ctx, client, resp.JobID, resp.Node)
		return providersUpgradeJobSubscribedMsg{row: row, next: next, name: name}
	}
}

// upgradeProgressLine renders one progress frame for the status line.
//
// The step text arrives on the event's own Step field, and the install
// coordinator also mirrors it onto Meta, so both are read: a frame carrying
// only one of them still renders something useful rather than a bare percent.
// The elapsed suffix only appears once a step has been quiet long enough
// that a frozen line becomes a question. Below this, steps come and go fast
// enough that a timer is noise.
const upgradeStallThreshold = 5 * time.Second

func upgradeProgressLine(name string, ev pkgClient.JobEvent, elapsed time.Duration) string {
	label := ev.Step
	if label == "" && ev.Meta != nil {
		if s, ok := ev.Meta["step"].(string); ok {
			label = s
		}
	}
	if label == "" {
		label = ev.Phase
	}

	var line string
	switch {
	case label != "" && ev.Percent > 0:
		line = fmt.Sprintf("Upgrading %s… %d%% %s", name, ev.Percent, label)
	case label != "":
		line = fmt.Sprintf("Upgrading %s… %s", name, label)
	case ev.Percent > 0:
		line = fmt.Sprintf("Upgrading %s… %d%%", name, ev.Percent)
	default:
		line = fmt.Sprintf("Upgrading %s…", name)
	}
	if elapsed >= upgradeStallThreshold {
		line += fmt.Sprintf(" (%s elapsed)", formatElapsed(elapsed))
	}
	return line
}

// formatElapsed renders a duration the way someone watching a stalled step
// reads it: whole seconds under a minute, then minutes and seconds.
func formatElapsed(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
}

// refreshUpgradeStatus re-renders the status line from the last frame the
// job emitted, advancing the elapsed clock. Called on every spinner tick so
// a step that emits nothing for minutes still shows it is being waited on.
func (m *ProvidersViewModel) refreshUpgradeStatus() {
	if m.upgrade == nil {
		return
	}
	m.actionStatus = upgradeProgressLine(m.upgrade.name, m.upgrade.frame, utils.Now().Sub(m.upgrade.frameAt))
}

// handleUpgradeFrame consumes one frame for the in-flight upgrade job and
// reports whether it owned the frame.
func (m *ProvidersViewModel) handleUpgradeFrame(msg jobstream.FrameMsg) (tea.Cmd, bool) {
	if m.upgrade == nil || msg.JobID != m.upgrade.row.JobID {
		return nil, false
	}

	if msg.Done {
		m.upgrade.row.Done = true
		m.upgrade.row.Cancel()
		name := m.upgrade.name
		m.upgrade = nil
		err := msg.Err
		// Hand off to the shared action handler, which reports the outcome
		// and re-fetches the list so the version column reflects the result.
		return func() tea.Msg {
			return providersActionMsg{action: "upgrade", name: name, err: err}
		}, true
	}

	// Restart the elapsed clock: it measures silence since the last frame,
	// which is what tells a slow step apart from a stuck one.
	m.upgrade.frame = msg.Event
	m.upgrade.frameAt = utils.Now()
	m.actionStatus = upgradeProgressLine(m.upgrade.name, msg.Event, 0)
	// Re-arm the read. A frame handler that returns no command consumes one
	// frame and then waits forever: the status line freezes on the first
	// step, and because the terminal frame never arrives either, a failed
	// upgrade reports nothing at all.
	return jobstream.Next(m.upgrade.row), true
}
