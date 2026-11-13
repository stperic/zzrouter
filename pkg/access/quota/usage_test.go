package quota

import (
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestCurrentUsage(t *testing.T) {
	q := QuotaConfig{SpendLimit: 10, ResetPeriod: "monthly"}
	missing := CurrentUsage(q, nil)
	require.False(t, missing.Tracked)
	require.Nil(t, missing.RemainingUSD)
	require.NotEmpty(t, missing.ResetAt)
	state := &SpendState{SpendMicro: 2_000_000, ReservedMicro: 1_000_000, TokensIn: 5, RequestCount: 2, Period: PeriodDaily, PeriodStart: PeriodStartFor(utils.NowUTC(), PeriodDaily)}
	current := CurrentUsage(q, state)
	require.True(t, current.Tracked)
	require.Equal(t, 2.0, current.SpendUSD)
	require.Equal(t, 1.0, current.ReservedUSD)
	require.Equal(t, 7.0, *current.RemainingUSD)
	require.Equal(t, "daily", current.Period, "an existing window keeps its period until rollover")
	require.Equal(t, NextPeriodStart(state.PeriodStart, PeriodDaily).Format(time.RFC3339), current.ResetAt)
	state.PeriodStart = state.PeriodStart.AddDate(0, 0, -3)
	expired := CurrentUsage(q, state)
	require.True(t, expired.Tracked)
	require.Zero(t, expired.SpendUSD)
	require.Zero(t, expired.ReservedUSD)
	require.Zero(t, expired.RequestCount)
	require.Equal(t, "monthly", expired.Period)
	require.Equal(t, 10.0, *expired.RemainingUSD)
	require.Equal(t, int64(2_000_000), state.SpendMicro, "a read never modifies the ledger")
	unlimited := CurrentUsage(QuotaConfig{}, state)
	require.False(t, unlimited.Tracked)
	require.Nil(t, unlimited.RemainingUSD)
	require.Empty(t, unlimited.ResetAt)
}
