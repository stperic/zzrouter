package jobs

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
	"github.com/stretchr/testify/require"
)

func retentionRegistry(t *testing.T) (*Registry, *clocktest.FakeClock) {
	t.Helper()
	fc := clocktest.NewFakeClock(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	r, err := NewRegistry(Config{NodeName: "node", Clock: fc, JanitorInterval: 2 * constants.IdempotencyReplayWindow})
	require.NoError(t, err)
	t.Cleanup(r.Stop)
	return r, fc
}

func TestDefaultRetentionSweep(t *testing.T) {
	r, fc := retentionRegistry(t)
	h, err := r.StartDetached(KindDownload, "", nil)
	require.NoError(t, err)
	h.Done()
	fc.Advance(constants.IdempotencyReplayWindow - time.Nanosecond)
	r.sweep()
	_, err = r.Get(h.ID())
	require.NoError(t, err)
	fc.Advance(time.Nanosecond)
	r.sweep()
	require.Empty(t, r.jobs)
}

func TestTerminalExpiryAcrossReadSurfaces(t *testing.T) {
	for _, terminal := range []string{"done", "failed", "inactive"} {
		t.Run(terminal, func(t *testing.T) {
			r, fc := retentionRegistry(t)
			h, err := r.StartDetached(KindInstall, "", nil)
			require.NoError(t, err)
			checkLive := func() {
				ev, getErr := r.Get(h.ID())
				require.NoError(t, getErr)
				require.True(t, ev.ExpiresAt.IsZero())
				data, marshalErr := json.Marshal(ev)
				require.NoError(t, marshalErr)
				require.NotContains(t, string(data), "expires_at")
			}
			checkLive()
			h.Progress(42, "installing", Bytes{})
			checkLive()
			switch terminal {
			case "done":
				h.Done()
			case "failed":
				h.Fail(errors.New("refused"))
			case "inactive":
				fc.Advance(KindInstall.DefaultInactivityTimeout() + time.Nanosecond)
				r.sweep()
			}
			ev, err := r.Get(h.ID())
			require.NoError(t, err)
			require.Equal(t, fc.Now().Add(constants.IdempotencyReplayWindow), ev.ExpiresAt)
			fc.Advance(time.Second)
			h.Done()
			h.Fail(errors.New("duplicate"))
			again, err := r.Get(h.ID())
			require.NoError(t, err)
			require.Equal(t, ev.ExpiresAt, again.ExpiresAt)
			sub, err := r.Subscribe(h.ID(), SubscribeOptions{})
			require.NoError(t, err)
			events := drain(sub.Events())
			require.Equal(t, ev.ExpiresAt, events[len(events)-1].ExpiresAt)
			require.Equal(t, ev.ExpiresAt, r.List("")[0].ExpiresAt)
			fc.Advance(constants.IdempotencyReplayWindow - time.Second)
			_, err = r.Get(h.ID())
			require.ErrorIs(t, err, ErrNotFound)
			_, err = r.Subscribe(h.ID(), SubscribeOptions{})
			require.ErrorIs(t, err, ErrNotFound)
			require.ErrorIs(t, r.Cancel(h.ID()), ErrNotFound)
			require.Empty(t, r.List(""))
			require.Empty(t, r.HandOut(h.ID()), "hand-out cannot resurrect an expired job")
			require.Len(t, r.jobs, 1, "expiry is enforced before the sweep")
		})
	}
}

func TestHandOutMovesExpiryAndTerminalReplay(t *testing.T) {
	r, fc := retentionRegistry(t)
	h, err := r.StartDetached(KindRun, "", nil)
	require.NoError(t, err)
	fc.Advance(time.Hour)
	require.Equal(t, h.ID(), r.HandOut(h.ID()))
	require.True(t, r.List("")[0].ExpiresAt.IsZero())
	h.Done()
	fc.Advance(23 * time.Hour)
	require.Equal(t, h.ID(), r.HandOut(h.ID()))
	expected := fc.Now().Add(constants.IdempotencyReplayWindow)
	fc.Advance(time.Hour)
	r.sweep()
	ev, err := r.Get(h.ID())
	require.NoError(t, err)
	require.Equal(t, expected, ev.ExpiresAt)
	sub, err := r.Subscribe(h.ID(), SubscribeOptions{})
	require.NoError(t, err)
	events := drain(sub.Events())
	require.Equal(t, expected, events[len(events)-1].ExpiresAt)
}

func TestRetentionConcurrentTerminalReadSubscribe(t *testing.T) {
	r, fc := retentionRegistry(t)
	h, err := r.StartDetached(KindRun, "", nil)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			for range 50 {
				h.Progress(20, "loading", Bytes{})
				h.Done()
				h.Fail(errors.New("duplicate"))
				_, _ = r.Get(h.ID())
				_ = r.List("")
				r.HandOut(h.ID())
				sub, subErr := r.Subscribe(h.ID(), SubscribeOptions{})
				if subErr == nil {
					sub.Unsubscribe()
				}
				r.sweep()
			}
		})
	}
	wg.Wait()
	ev, err := r.Get(h.ID())
	require.NoError(t, err)
	require.Equal(t, fc.Now().Add(constants.IdempotencyReplayWindow), ev.ExpiresAt)
}

func TestKindFromID(t *testing.T) {
	for k, prefix := range kindPrefixes {
		require.Equal(t, k, KindFromID(prefix+"0123456789abcdef"))
	}
	for _, id := range []string{"", "job_x", "unknown_x", "xrun_x"} {
		require.Empty(t, KindFromID(id))
	}
}

func TestReuseRenewsLiveJobAndRejectsGoneJob(t *testing.T) {
	r, fc := retentionRegistry(t)
	h, err := r.StartDetached(KindDownload, "", Meta{"source": "worker"})
	require.NoError(t, err)
	h.Done()
	fc.Advance(23 * time.Hour)
	ev, err := r.Reuse(h.ID())
	require.NoError(t, err)
	require.Equal(t, h.ID(), ev.JobID)
	require.Equal(t, "worker", ev.Meta["source"])
	require.Equal(t, fc.Now().Add(constants.IdempotencyReplayWindow), ev.ExpiresAt)
	fc.Advance(constants.IdempotencyReplayWindow)
	for _, id := range []string{h.ID(), "never-registered", ""} {
		ev, err = r.Reuse(id)
		require.ErrorIs(t, err, ErrNotFound)
		require.Empty(t, ev.JobID)
	}
}

func TestTerminalCompactionPreservesReplay(t *testing.T) {
	for _, terminal := range []string{"done", "failed", "inactive"} {
		t.Run(terminal, func(t *testing.T) {
			r, fc := retentionRegistry(t)
			h, err := r.StartDetached(KindInstall, "", Meta{"provider": "engine"})
			require.NoError(t, err)
			h.Progress(42, "installing", Bytes{Done: 10, Total: 20})
			sub, err := r.Subscribe(h.ID(), SubscribeOptions{})
			require.NoError(t, err)
			switch terminal {
			case "done":
				h.Done()
			case "failed":
				h.Fail(errors.New("install refused"))
			case "inactive":
				fc.Advance(KindInstall.DefaultInactivityTimeout() + time.Nanosecond)
				r.sweep()
			}
			live := drain(sub.Events())
			require.Len(t, live, 3)
			replay, err := r.Subscribe(h.ID(), SubscribeOptions{})
			require.NoError(t, err)
			require.Equal(t, live, drain(replay.Events()))
			j := h.(*job)
			require.Equal(t, len(j.ring), cap(j.ring), "terminal rings retain only populated slots")
		})
	}
}
