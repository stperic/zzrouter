package install

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServiceStepsRequireOwnerAndNeverExecuteShell(t *testing.T) {
	step := Step{ServiceAction: ServiceStart, Command: "this-command-must-never-execute", Timeout: time.Second}
	require.ErrorContains(t, executeStep(context.Background(), step), "provider installation API")
	var calls []ServiceAction
	ctx := WithServiceControl(context.Background(), func(ctx context.Context, action ServiceAction) error {
		_, deadline := ctx.Deadline()
		require.True(t, deadline, "service steps keep their per-step bound")
		calls = append(calls, action)
		return nil
	})
	require.NoError(t, executeStep(ctx, step))
	require.Equal(t, []ServiceAction{ServiceStart}, calls)
	step.ServiceAction = "shell"
	require.ErrorContains(t, executeStep(ctx, step), "unsupported")
	require.Len(t, calls, 1)
}
