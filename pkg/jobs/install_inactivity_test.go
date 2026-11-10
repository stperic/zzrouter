package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stretchr/testify/require"
)

func TestQuietInstallSurvivesLongOperationWithoutSyntheticProgress(t *testing.T) {
	r, fc := retentionRegistry(t, nil)
	h, err := r.StartDetached(KindInstall, "", Meta{"provider": "engine"})
	require.NoError(t, err)
	h.Progress(60, "install dependencies", Bytes{Done: 10, Total: 100})
	before, err := r.Get(h.ID())
	require.NoError(t, err)
	// Cover both a 90-minute dependency step and an eight-hour MTP operation.
	fc.Advance(9 * time.Hour)
	r.sweep()
	after, err := r.Get(h.ID())
	require.NoError(t, err)
	require.Equal(t, PhaseRunning, after.Phase)
	require.Equal(t, before, after, "silence must not require heartbeat or invented progress events")
	require.NoError(t, h.Context().Err())
	h.Done()
	terminal, err := r.Get(h.ID())
	require.NoError(t, err)
	require.Equal(t, PhaseDone, terminal.Phase)
	require.Equal(t, fc.Now().Add(constants.IdempotencyReplayWindow), terminal.ExpiresAt)
	fc.Advance(constants.IdempotencyReplayWindow)
	r.sweep()
	_, err = r.Get(h.ID())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestQuietInstallRetainsOwnerCancellationAndShutdown(t *testing.T) {
	for _, ending := range []string{"cancel", "shutdown", "failure"} {
		t.Run(ending, func(t *testing.T) {
			r, fc := retentionRegistry(t, nil)
			h, err := r.StartDetached(KindInstall, "", nil)
			require.NoError(t, err)
			h.Progress(60, "installing", Bytes{})
			fc.Advance(9 * time.Hour)
			r.sweep()
			require.NoError(t, h.Context().Err())
			switch ending {
			case "cancel":
				require.NoError(t, r.Cancel(h.ID()))
				require.ErrorIs(t, context.Cause(h.Context()), ErrCanceled)
				h.Fail(context.Cause(h.Context()))
			case "shutdown":
				r.Stop()
				require.ErrorIs(t, h.Context().Err(), context.Canceled)
				require.NotErrorIs(t, context.Cause(h.Context()), ErrCanceled)
				_, err = r.Get(h.ID())
				require.ErrorIs(t, err, ErrNotFound)
				return
			case "failure":
				h.Fail(errors.New("installer failed"))
			}
			event, err := r.Get(h.ID())
			require.NoError(t, err)
			require.Equal(t, PhaseFailed, event.Phase)
			require.Equal(t, fc.Now().Add(constants.IdempotencyReplayWindow), event.ExpiresAt)
			require.ErrorIs(t, h.Context().Err(), context.Canceled)
		})
	}
}

func TestInstallExplicitInactivityPolicyStillReaps(t *testing.T) {
	r, fc := retentionRegistry(t, map[Kind]time.Duration{KindInstall: time.Minute})
	h, err := r.StartDetached(KindInstall, "", nil)
	require.NoError(t, err)
	h.Progress(60, "installing", Bytes{})
	fc.Advance(time.Minute + time.Nanosecond)
	r.sweep()
	event, err := r.Get(h.ID())
	require.NoError(t, err)
	require.Equal(t, PhaseFailed, event.Phase)
	require.Equal(t, errInactive.Error(), event.Err)
	require.ErrorIs(t, h.Context().Err(), context.Canceled)
}
