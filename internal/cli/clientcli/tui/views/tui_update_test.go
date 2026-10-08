package views

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// shortenPollInterval makes the poll tick fire immediately for the rest
// of the test. tea.Tick fixes its duration when the command is BUILT, so
// this has to happen before the action that schedules one.
func shortenPollInterval(t *testing.T) {
	t.Helper()
	saved := updatePollInterval
	updatePollInterval = time.Millisecond
	t.Cleanup(func() { updatePollInterval = saved })
}

// schedulesPoll runs cmd (and anything it batches) and reports whether a
// poll tick comes back.
func schedulesPoll(t *testing.T, cmd tea.Cmd) bool {
	t.Helper()
	found := make(chan bool, 1)
	go func() { found <- containsPollMsg(cmd) }()
	select {
	case ok := <-found:
		return ok
	case <-time.After(2 * time.Second):
		return false
	}
}

func containsPollMsg(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case updatePollMsg:
		return true
	case tea.BatchMsg:
		for _, sub := range msg {
			if containsPollMsg(sub) {
				return true
			}
		}
	}
	return false
}

func newUpdateView(t *testing.T) *UpdateViewModel {
	t.Helper()
	v := NewUpdateViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	updated, _ := v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return updated.(*UpdateViewModel)
}

func statusLoaded(t *testing.T, v *UpdateViewModel, status *pkgClient.UpdateStatus) *UpdateViewModel {
	t.Helper()
	updated, _ := v.Update(updateStatusMsg{status: status})
	return updated.(*UpdateViewModel)
}

// An apply is watched by polling the status, whatever the node answered
// with. The alternative — following the job the apply may return — dies
// on the path that works, because a successful apply restarts the node;
// and it is not offered at all by a node whose updates belong to a
// privileged updater, or by one with no jobs registry.
func TestUpdateView_DelegatedApplyWatchesStatus(t *testing.T) {
	shortenPollInterval(t)
	v := statusLoaded(t, newUpdateView(t), &pkgClient.UpdateStatus{State: "idle", Channel: "stable"})

	updated, cmd := v.Update(updateActionMsg{
		action: updateActionApply,
		result: &pkgClient.UpdateActionResult{
			DelegatedTo: "/var/lib/zzrouter/update/request.json",
			Action:      "apply",
		},
	})
	v = updated.(*UpdateViewModel)

	require.NotNil(t, v.watch)
	assert.Equal(t, "/var/lib/zzrouter/update/request.json", v.watch.delegatedTo)
	assert.Equal(t, updateViewWatch, v.mode)
	assert.Contains(t, strings.Join(v.watch.lines, " "), "privileged updater")

	// Not just "some command": the watch has to have scheduled the poll
	// that stands in for the stream it does not get. Returning only a
	// spinner tick leaves the view spinning over a run nobody is reading.
	require.NotNil(t, cmd, "nothing would ever advance a delegated watch")
	assert.True(t, schedulesPoll(t, cmd), "a delegated watch scheduled no poll")
}

// The poll ends when the privileged updater says the run finished, and
// carries its failure through rather than reporting a finish as success.
func TestUpdateView_DelegatedWatchEndsOnTheRunItCannotSee(t *testing.T) {
	finished := time.Now()

	t.Run("finished and failed", func(t *testing.T) {
		v := newUpdateView(t)
		v.watch = &updateWatch{action: updateActionApply, delegatedTo: "/req.json", startedAt: time.Now(), epoch: v.pollEpoch}
		v.mode = updateViewWatch

		updated, cmd := v.Update(updateStatusMsg{polled: true, epoch: v.pollEpoch, status: &pkgClient.UpdateStatus{
			PrivilegedRun: &pkgClient.UpdatePrivilegedRun{
				Action: "apply", State: "failed", FinishedAt: &finished, Error: "unit failed to start",
			},
		}})
		v = updated.(*UpdateViewModel)

		assert.True(t, v.watch.done)
		require.Error(t, v.watch.err)
		assert.Contains(t, v.watch.err.Error(), "unit failed to start")
		assert.Nil(t, cmd, "a finished run must stop the poll")
	})

	t.Run("still running", func(t *testing.T) {
		v := newUpdateView(t)
		v.watch = &updateWatch{action: updateActionApply, delegatedTo: "/req.json", startedAt: time.Now(), epoch: v.pollEpoch}
		v.mode = updateViewWatch

		updated, cmd := v.Update(updateStatusMsg{polled: true, epoch: v.pollEpoch, status: &pkgClient.UpdateStatus{
			PrivilegedRun: &pkgClient.UpdatePrivilegedRun{Action: "apply", State: "installing", Phase: "download"},
		}})
		v = updated.(*UpdateViewModel)

		assert.False(t, v.watch.done)
		assert.NotNil(t, cmd, "the watch stopped before the run did")
	})

	// A privileged run that never publishes a finish — the updater was
	// killed, the unit never started — must not hold the view forever
	// with nothing to say.
	t.Run("nothing published in time", func(t *testing.T) {
		v := newUpdateView(t)
		v.watch = &updateWatch{
			action:      updateActionApply,
			delegatedTo: "/req.json",
			startedAt:   time.Now().Add(-2 * updateWatchTimeout),
			epoch:       v.pollEpoch,
		}
		v.mode = updateViewWatch

		updated, cmd := v.Update(updateStatusMsg{polled: true, epoch: v.pollEpoch, status: &pkgClient.UpdateStatus{}})
		v = updated.(*UpdateViewModel)

		assert.True(t, v.watch.done)
		require.Error(t, v.watch.err)
		assert.Nil(t, cmd)
	})
}

// A refresh landing while a delegated watch is running must not start a
// second polling chain beside the first — that is how a two-second poll
// becomes a one-second poll, then half a second.
func TestUpdateView_AnOrdinaryRefreshDoesNotJoinThePoll(t *testing.T) {
	v := newUpdateView(t)
	v.watch = &updateWatch{action: updateActionApply, delegatedTo: "/req.json", startedAt: time.Now(), epoch: v.pollEpoch}
	v.mode = updateViewWatch

	_, cmd := v.Update(updateStatusMsg{status: &pkgClient.UpdateStatus{
		PrivilegedRun: &pkgClient.UpdatePrivilegedRun{Action: "apply", State: "installing"},
	}})
	assert.Nil(t, cmd, "an unpolled status fetch scheduled a poll of its own")
}

// The three statuses these routes answer with are outcomes, and each
// deserves its own words. A 503 in particular is not a failure: the
// feature is off, and the view says how to turn it on.
func TestUpdateView_TellsTheOutcomesApart(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		assert func(t *testing.T, v *UpdateViewModel)
	}{
		{
			name: "auto-update switched off",
			err:  &pkgClient.APIError{StatusCode: http.StatusServiceUnavailable, Message: "not enabled"},
			assert: func(t *testing.T, v *UpdateViewModel) {
				assert.True(t, v.disabled)
				assert.NoError(t, v.err, "a switched-off feature is a state, not an error")
				assert.Contains(t, v.viewContent(), "update.enabled")
			},
		},
		{
			name: "nothing waiting to install",
			err:  &pkgClient.APIError{StatusCode: http.StatusConflict, Message: "no update is available"},
			assert: func(t *testing.T, v *UpdateViewModel) {
				assert.False(t, v.disabled)
				assert.NoError(t, v.err)
				assert.Contains(t, v.notice, "Press C")
			},
		},
		{
			name: "nothing to roll back to",
			err:  &pkgClient.APIError{StatusCode: http.StatusNotFound, Message: "no backups available"},
			assert: func(t *testing.T, v *UpdateViewModel) {
				assert.Contains(t, v.notice, "roll back")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := statusLoaded(t, newUpdateView(t), &pkgClient.UpdateStatus{State: "idle", Channel: "stable"})
			updated, _ := v.Update(updateActionMsg{action: updateActionApply, err: tc.err})
			tc.assert(t, updated.(*UpdateViewModel))
		})
	}
}

// Installing and rolling back are both confirmed first, and the prompt
// names the node and the restart — the two things an operator pressing
// a letter has not thought about.
func TestUpdateView_ApplyAndRollbackAreConfirmed(t *testing.T) {
	v := statusLoaded(t, newUpdateView(t), &pkgClient.UpdateStatus{
		State: "update-available", Channel: "stable", UpdateAvailable: true,
		PendingRelease: &pkgClient.UpdateRelease{TagName: "v0.2.0"},
	})

	updated, cmd := v.Update(keyPress("a"))
	v = updated.(*UpdateViewModel)
	require.Equal(t, updateActionApply, v.confirmAction)
	assert.Nil(t, cmd, "the install must not start before the answer")
	assert.Contains(t, v.confirmPrompt(), "v0.2.0")
	assert.Contains(t, v.confirmPrompt(), "restarts")

	updated, _ = v.Update(keyPress("n"))
	v = updated.(*UpdateViewModel)
	assert.Equal(t, updateActionNone, v.confirmAction, "declining must leave the node alone")

	updated, _ = v.Update(keyPress("b"))
	v = updated.(*UpdateViewModel)
	assert.Equal(t, updateActionRollback, v.confirmAction)
}

// R is refresh in every other view. Binding rollback to it would put a
// version change one habitual keystroke away.
func TestUpdateKeys_RollbackIsNotOnTheRefreshKey(t *testing.T) {
	assert.True(t, key.Matches(keyPress("r"), tui.UpdateKeys.Refresh))
	assert.False(t, key.Matches(keyPress("r"), tui.UpdateKeys.Rollback))
	assert.True(t, key.Matches(keyPress("b"), tui.UpdateKeys.Rollback))
}

// The status body has to say whose updates these are: the routes read
// the scheduler of whichever node answers, so a view that implied the
// cluster would be describing machines it never asked.
func TestUpdateView_SaysItIsAboutOneNode(t *testing.T) {
	v := statusLoaded(t, newUpdateView(t), &pkgClient.UpdateStatus{
		State:          "idle",
		Channel:        "stable",
		CurrentVersion: &pkgClient.UpdateVersion{Major: 0, Minor: 1, Patch: 1, BuildMeta: "09091713"},
	})

	body := v.viewContent()
	assert.Contains(t, body, "this node only")
	assert.Contains(t, body, "0.1.1+09091713")
}

// Conditions are the difference between "nothing is happening" and
// "nothing can happen": each one is a reason an install would not land,
// and each has to be visible without opening a log.
func TestUpdateView_ShowsWhatStandsInTheWay(t *testing.T) {
	v := statusLoaded(t, newUpdateView(t), &pkgClient.UpdateStatus{
		State:                 "idle",
		Channel:               "stable",
		Blocked:               "the binary is not writable by this user",
		RestartRequired:       true,
		RestartRequiredReason: "no supervisor would bring the node back",
		PendingConfirm: &pkgClient.UpdatePendingConfirm{
			FromVersion: "0.1.0", ToVersion: "0.1.1", Attempts: 2,
		},
	})

	body := v.viewContent()
	assert.Contains(t, body, "not writable")
	assert.Contains(t, body, "no supervisor")
	assert.Contains(t, body, "unconfirmed")
}

// An apply on a node that installs its own updates may answer with a job
// id or without one — openUpdateJobDetached returns nothing when the
// node has no jobs registry. Either way the work has started, and either
// way the status is what says so. Reading "no job id" as "already
// finished" would report an install that is still running as done.
func TestUpdateView_ApplyWithoutAJobIDStillWatches(t *testing.T) {
	shortenPollInterval(t)
	v := statusLoaded(t, newUpdateView(t), &pkgClient.UpdateStatus{State: "idle", Channel: "stable"})

	updated, cmd := v.Update(updateActionMsg{
		action: updateActionApply,
		result: &pkgClient.UpdateActionResult{}, // 202, no job, no handoff
	})
	v = updated.(*UpdateViewModel)

	require.NotNil(t, v.watch, "the apply was treated as already over")
	assert.Equal(t, updateViewWatch, v.mode)
	assert.Empty(t, v.notice)
	assert.True(t, schedulesPoll(t, cmd))
}

// A rollback this node performs itself is finished when it answers:
// there is no job and no handoff, only the backup it restored. It must
// not open a watch that would poll a node doing nothing.
func TestUpdateView_LocalRollbackIsDoneWhenItAnswers(t *testing.T) {
	v := statusLoaded(t, newUpdateView(t), &pkgClient.UpdateStatus{State: "idle", Channel: "stable"})

	updated, _ := v.Update(updateActionMsg{
		action: updateActionRollback,
		result: &pkgClient.UpdateActionResult{BackupPath: "/opt/zzrouter/versions/0.1.0"},
	})
	v = updated.(*UpdateViewModel)

	assert.Nil(t, v.watch, "a finished rollback does not need watching")
	assert.Equal(t, updateViewStatus, v.mode)
	assert.Contains(t, v.notice, "0.1.0")
	assert.Contains(t, v.notice, "restart")
}

// A local install ends when the node's own state settles, and a failure
// has to arrive as a failure — the scheduler leaves the state on the
// phase it died in until something turns it into `failed`.
func TestUpdateView_LocalWatchEndsOnTheNodesState(t *testing.T) {
	t.Run("settled back to idle", func(t *testing.T) {
		v := newUpdateView(t)
		v.watch = &updateWatch{action: updateActionApply, startedAt: time.Now(), epoch: v.pollEpoch}
		v.mode = updateViewWatch

		updated, cmd := v.Update(updateStatusMsg{polled: true, epoch: v.pollEpoch,
			status: &pkgClient.UpdateStatus{State: "idle"}})
		v = updated.(*UpdateViewModel)

		assert.True(t, v.watch.done)
		assert.NoError(t, v.watch.err)
		assert.Nil(t, cmd)
	})

	t.Run("failed, with the reason", func(t *testing.T) {
		v := newUpdateView(t)
		v.watch = &updateWatch{action: updateActionApply, startedAt: time.Now(), epoch: v.pollEpoch}
		v.mode = updateViewWatch

		updated, cmd := v.Update(updateStatusMsg{polled: true, epoch: v.pollEpoch,
			status: &pkgClient.UpdateStatus{State: "failed", Error: "checksum mismatch"}})
		v = updated.(*UpdateViewModel)

		assert.True(t, v.watch.done)
		require.Error(t, v.watch.err)
		assert.Contains(t, v.watch.err.Error(), "checksum mismatch")
		assert.Nil(t, cmd)
	})

	t.Run("still downloading", func(t *testing.T) {
		shortenPollInterval(t)
		v := newUpdateView(t)
		v.watch = &updateWatch{action: updateActionApply, startedAt: time.Now(), epoch: v.pollEpoch}
		v.mode = updateViewWatch

		updated, cmd := v.Update(updateStatusMsg{polled: true, epoch: v.pollEpoch,
			status: &pkgClient.UpdateStatus{State: "downloading", DownloadProgress: 42}})
		v = updated.(*UpdateViewModel)

		assert.False(t, v.watch.done)
		assert.Contains(t, strings.Join(v.watch.lines, " "), "42%")
		assert.True(t, schedulesPoll(t, cmd))
	})
}

// The success path takes the node away: a successful apply restarts it,
// so every call fails until it is back. A watch that treated that as the
// install failing would report a failure on the path that worked.
func TestUpdateView_AWatchSurvivesTheNodeRestarting(t *testing.T) {
	shortenPollInterval(t)
	v := newUpdateView(t)
	v.watch = &updateWatch{action: updateActionApply, startedAt: time.Now(), epoch: v.pollEpoch}
	v.mode = updateViewWatch

	updated, cmd := v.Update(updateStatusMsg{
		polled: true, epoch: v.pollEpoch,
		err: &pkgClient.APIError{StatusCode: 0, Message: "connection refused"},
	})
	v = updated.(*UpdateViewModel)

	assert.False(t, v.watch.done, "the node going away ended the watch")
	assert.NoError(t, v.err, "a restart is not a view-level error")
	assert.True(t, schedulesPoll(t, cmd), "the poll must keep trying across the restart")
}

// A tick from a watch that has been superseded must be dropped, or two
// chains re-arm each other and the poll rate doubles per action.
func TestUpdateView_TicksFromASupersededWatchAreDropped(t *testing.T) {
	v := newUpdateView(t)
	v.watch = &updateWatch{action: updateActionApply, startedAt: time.Now(), epoch: 7}
	v.mode = updateViewWatch

	_, cmd := v.Update(updatePollMsg{epoch: 6})
	assert.Nil(t, cmd, "a tick from an older watch kept a second chain alive")

	_, cmd = v.Update(updatePollMsg{epoch: 7})
	assert.NotNil(t, cmd, "the live watch stopped ticking")
}
