package instance

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestActivityEvidenceIsIndependentOfEvictionHold(t *testing.T) {
	run := NewInstance("run", "provider", "model", 0, time.Minute, 10)
	run.MarkRunning(false, nil)
	require.Equal(t, "idle", run.ActivitySnapshot(time.Now()).State, "a loaded service is not work")
	run.ConfigureActivityProbe(time.Minute)
	require.Equal(t, "unknown", run.ActivitySnapshot(time.Now()).State)
	run.ObserveActivity(true)
	busy := run.ActivitySnapshot(time.Now())
	require.Equal(t, "active", busy.State)
	require.Equal(t, 1, busy.BusyEngines)
	probeAt := run.activityProbe.at
	later := run.ActivitySnapshot(probeAt.Add(20 * time.Second))
	require.Equal(t, probeAt.UTC(), later.ObservedAt)
	require.Equal(t, float64(20), later.AgeSeconds)
	require.Equal(t, float64(40), later.ValidForSeconds)
	aggregate := SummarizeActivity([]Activity{later}, probeAt.Add(20*time.Second))
	require.Equal(t, later.ObservedAt, aggregate.ObservedAt)
	require.Equal(t, float64(35), aggregate.Current(probeAt.Add(25*time.Second), false).ValidForSeconds)
	require.Equal(t, "unknown", busy.Current(time.Now().Add(2*time.Minute), false).State)
	run.ActivityUnknown(false)
	require.Equal(t, "unknown", run.ActivitySnapshot(time.Now()).State)
	require.True(t, run.IdleSnapshot().Busy, "failed probes must keep the existing protective hold")
	run.ActivityUnknown(true)
	require.False(t, run.IdleSnapshot().Busy)
	require.Equal(t, "unknown", run.ActivitySnapshot(time.Now()).State, "releasing the hold is not an idle observation")
	lease, err := run.Acquire(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, "active", run.ActivitySnapshot(time.Now()).State, "an admitted request is positive evidence even when the engine probe failed")
	lease.Release()
	require.Equal(t, "unknown", run.ActivitySnapshot(time.Now()).State)
	run.ObserveActivity(false)
	require.Equal(t, "idle", run.ToInfo("node").Activity.State)
}

func TestActivityReceivingAndAggregation(t *testing.T) {
	now := time.Now()
	peer := &Activity{State: "active", ActiveRequests: 1, ObservedAt: now.Add(24 * time.Hour), AgeSeconds: 10, ValidForSeconds: 20}
	value := ReceiveActivity(peer, now)
	require.Equal(t, now.Add(-10*time.Second).UTC(), value.ObservedAt)
	current := value.Current(now.Add(4*time.Second), false)
	require.Equal(t, float64(16), current.ValidForSeconds)
	require.Equal(t, float64(14), current.AgeSeconds)
	require.Equal(t, "active", current.State)
	require.Equal(t, "unknown", value.Current(now.Add(21*time.Second), false).State)
	require.Equal(t, "unknown", value.Current(now, true).State)
	unknown := Activity{State: "unknown", ObservedAt: now}
	require.Equal(t, "active", SummarizeActivity([]Activity{*value, unknown}, now).State)
	require.Equal(t, "unknown", SummarizeActivity([]Activity{unknown}, now).State)
	require.Equal(t, "idle", SummarizeActivity(nil, now).State)
	for _, age := range []float64{-1, math.Inf(1), math.NaN(), float64(math.MaxInt64) / float64(time.Second)} {
		copy := *peer
		copy.AgeSeconds = age
		require.Nil(t, ReceiveActivity(&copy, now))
	}
	peer.ActiveRequests = 0
	require.Nil(t, ReceiveActivity(peer, now), "active requires positive evidence")
}
