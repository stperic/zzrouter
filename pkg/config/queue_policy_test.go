package config

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

func TestLease_WaitExpires(t *testing.T) {
	rt := &AppRuntimeConfig{}
	assert.Equal(t, constants.DefaultQueueWait, rt.GetQueueTimeout())
	assert.Positive(t, rt.GetQueueTimeout())
	for _, value := range []string{"invalid", "-1s"} {
		rt.QueueTimeout = value
		assert.Equal(t, constants.DefaultQueueWait, rt.GetQueueTimeout())
	}
	rt.QueueTimeout = "0s"
	assert.Zero(t, rt.GetQueueTimeout())
	rt.QueueTimeout = "20ms"
	assert.Equal(t, 20*time.Millisecond, rt.GetQueueTimeout())
}

func TestQueueTimeoutCaller(t *testing.T) {
	rt := &AppRuntimeConfig{MaxConcurrentRequests: 1, QueueTimeout: "caller"}
	require.Equal(t, constants.QueueUntilCallerDeadline, rt.GetQueueTimeout())
	inst := instance.NewInstance("id", "p", "m", 0, 0, 0)
	inst.InitConcurrencyLimit(instance.ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: rt.QueueDepth(), QueueTimeout: rt.GetQueueTimeout()})
	held, err := inst.Acquire(context.Background(), nil)
	require.NoError(t, err)
	defer held.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	lease, err := inst.Acquire(ctx, nil)
	assert.Nil(t, lease)
	assert.Equal(t, context.DeadlineExceeded, err)
	assert.Equal(t, context.DeadlineExceeded, ctx.Err())
}

func TestQueueDepth(t *testing.T) {
	assert.Equal(t, constants.DefaultQueueDepthFactor*2, (&AppRuntimeConfig{MaxConcurrentRequests: 2}).QueueDepth())
	assert.Zero(t, (&AppRuntimeConfig{}).QueueDepth(), "an unlimited instance never queues")
}
