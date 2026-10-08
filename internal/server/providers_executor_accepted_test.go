package server

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An agent has only the 202 body in front of it, so the body must name where
// to watch the job rather than leaving it a template to assemble.
func TestAcceptedJobNamesWhereToWatch(t *testing.T) {
	got := acceptedJob("job_abc123", "llamacpp", "worker-1", nil)

	assert.Equal(t, true, got["accepted"])
	assert.Equal(t, "job_abc123", got["job_id"])
	assert.Equal(t, "/zzrouter/v1/jobs/job_abc123", got["job_url"])
	assert.Equal(t, "/zzrouter/v1/jobs/job_abc123/stream", got["stream_url"])
	assert.Equal(t, "llamacpp", got["provider"])
	assert.Equal(t, "worker-1", got["node"])

	// The step-machine endpoint adds its step index without losing the links.
	step := acceptedJob("job_abc123", "llamacpp", "worker-1", map[string]any{"step": 3})
	assert.Equal(t, 3, step["step"])
	assert.Equal(t, "/zzrouter/v1/jobs/job_abc123/stream", step["stream_url"])
}

// A lifecycle job that reports done must leave the cluster view describing
// what is actually installed. Install and uninstall used to refresh only by
// accident — they flip config, and a config mutation republishes — while
// upgrade changes no config and so left every read serving the version it
// had just replaced.
func TestAfterLifecycleRepublishesState(t *testing.T) {
	t.Run("republishes after the hook succeeds", func(t *testing.T) {
		var order []string
		e := &ProvidersExecutor{republishState: func() { order = append(order, "republish") }}

		err := e.afterLifecycle(func() error { order = append(order, "hook"); return nil })()

		require.NoError(t, err)
		assert.Equal(t, []string{"hook", "republish"}, order,
			"the snapshot must be current before the job reports done")
	})

	t.Run("republishes even when the hook fails", func(t *testing.T) {
		republished := false
		e := &ProvidersExecutor{republishState: func() { republished = true }}

		err := e.afterLifecycle(func() error { return errors.New("finalize blew up") })()

		// The bytes on disk changed either way, so the reported state should
		// describe what is there rather than what was there before.
		require.Error(t, err)
		assert.True(t, republished)
	})

	t.Run("upgrade passes no hook and still republishes", func(t *testing.T) {
		republished := false
		e := &ProvidersExecutor{republishState: func() { republished = true }}

		require.NoError(t, e.afterLifecycle(nil)())
		assert.True(t, republished)
	})
}
