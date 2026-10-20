package instance

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func TestEndJoinKeepsTheJoinedAttemptResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := NewInstance("join", "provider", "model", 0, 0, 0)
		require.True(t, run.ClaimEnd())
		result := make(chan error, 1)
		go func() { result <- run.WaitForEnd(t.Context()) }()
		synctest.Wait()
		failure := errors.New("stop failed")
		run.FinishEnd(failure)
		require.True(t, run.ClaimEnd())
		run.FinishEnd(nil)
		require.ErrorIs(t, <-result, failure)
		require.NoError(t, run.WaitForEnd(t.Context()))
		require.False(t, run.ClaimEnd(), "a completed stop keeps ownership")
	})
}

func TestEndJoinReportsDeadline(t *testing.T) {
	run := NewInstance("join", "provider", "model", 0, 0, 0)
	require.True(t, run.ClaimEnd())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, run.WaitForEnd(ctx), context.Canceled)
	run.FinishEnd(ErrStopping)
	require.ErrorIs(t, run.WaitForEnd(t.Context()), ErrStopping)
	require.True(t, run.ClaimEnd())
	run.FinishEnd(nil)
}
