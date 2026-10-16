package instance

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
)

func TestNewInstance(t *testing.T) {
	inst := NewInstance("test-id", "vllm", "llama3", 8000, 5*time.Minute, 50)

	assert.Equal(t, "test-id", inst.ID)
	assert.Equal(t, "vllm", inst.Provider)
	assert.Equal(t, "llama3", inst.Model)
	assert.Equal(t, 8000, inst.Port)
	assert.Equal(t, StatusStarting, inst.Status)
	assert.Equal(t, 5*time.Minute, inst.KeepAlive)
	assert.Equal(t, LaunchModeNative, inst.LaunchMode)
	assert.NotNil(t, inst.Logs)
	assert.NotNil(t, inst.Context())
}

func TestNewInstance_DefaultLogBuffer(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 0)
	// Should use default buffer of 100
	assert.Equal(t, 100, cap(inst.Logs))
}

func TestRecordFeatureInitializesNilMap(t *testing.T) {
	i := NewInstance("feature", "provider", "model", 0, 0, 0)
	assert.Nil(t, i.Resolved().Features)
	assert.False(t, i.RecordFeature("tools", "present"), "starting runs reject observations")
	i.MarkRunning(false, nil)
	assert.True(t, i.RecordFeature("tools", "present"))
	assert.Equal(t, map[string]string{"tools": "present"}, i.Resolved().Features)
	assert.False(t, i.RecordFeature("tools", "present"), "unchanged evidence does not republish")
	assert.True(t, i.RecordFeature("vision", "unsupported"))
	assert.Equal(t, "present", i.Resolved().Features["tools"])
}

func TestInstance_StatusTransitions(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)

	assert.Equal(t, StatusStarting, inst.GetStatus())

	inst.MarkRunning(false, nil)
	assert.Equal(t, StatusRunning, inst.GetStatus())
	assert.True(t, inst.IsHealthy())
	assert.True(t, inst.IsRunning())
	assert.Empty(t, inst.GetErrorMessage())

	inst.MarkUnhealthy("bad health")
	assert.Equal(t, StatusUnhealthy, inst.GetStatus())
	assert.False(t, inst.IsHealthy())
	assert.True(t, inst.IsRunning()) // unhealthy is still "running"
	assert.Equal(t, "bad health", inst.GetErrorMessage())

	inst.MarkFailed("process died")
	assert.Equal(t, StatusFailed, inst.GetStatus())
	assert.False(t, inst.IsHealthy())
	assert.False(t, inst.IsRunning())
	assert.Equal(t, "process died", inst.GetErrorMessage())
	assert.False(t, inst.FailedAt.IsZero())
}

func TestInstance_StatusTransitions_Concurrent(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			switch n % 4 {
			case 0:
				inst.MarkRunning(false, nil)
			case 1:
				inst.MarkUnhealthy("test")
			case 2:
				inst.MarkFailed("test")
			case 3:
				inst.Admit()
			}
			_ = inst.GetStatus()
			_ = inst.IsHealthy()
			_ = inst.IsRunning()
		}(i)
	}
	wg.Wait()
}

func TestInstance_StampActivityWithHints(t *testing.T) {
	ptr := func(d time.Duration) *time.Duration { return &d }

	t.Run("nil hints stamps activity using configured keep-alive", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 1*time.Hour, 10)
		inst.StampActivityWithHints(nil)
		assert.Equal(t, 1*time.Hour, inst.KeepAlive, "KeepAlive unchanged on nil hints")
		assert.False(t, inst.GetLastActivity().IsZero())
	})

	t.Run("hints with nil KeepAlive leaves KeepAlive alone", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 1*time.Hour, 10)
		inst.StampActivityWithHints(&keepalive.Override{Duration: nil})
		assert.Equal(t, 1*time.Hour, inst.KeepAlive)
		assert.False(t, inst.GetLastActivity().IsZero())
	})

	t.Run("KeepAlive override replaces configured value", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 1*time.Hour, 10)
		inst.StampActivityWithHints(&keepalive.Override{Duration: ptr(5 * time.Minute)})
		assert.Equal(t, 5*time.Minute, inst.KeepAlive)
	})

	t.Run("KeepAlive=0 override preserves unload-promptly semantics", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 1*time.Hour, 10)
		inst.StampActivityWithHints(&keepalive.Override{Duration: ptr(0)})
		assert.Equal(t, time.Duration(0), inst.KeepAlive)
		assert.Equal(t, time.Duration(0), inst.GetKeepAlive())
	})

	t.Run("negative KeepAlive override preserves indefinite semantics", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 1*time.Hour, 10)
		inst.StampActivityWithHints(&keepalive.Override{Duration: ptr(-1 * time.Second)})
		assert.Equal(t, -1*time.Second, inst.GetKeepAlive())
	})

	t.Run("last-one-wins across successive calls", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 1*time.Hour, 10)
		inst.StampActivityWithHints(&keepalive.Override{Duration: ptr(10 * time.Minute)})
		inst.StampActivityWithHints(&keepalive.Override{Duration: ptr(30 * time.Second)})
		assert.Equal(t, 30*time.Second, inst.KeepAlive)
	})
}

func TestInstance_ActiveRequests(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)

	assert.Equal(t, int64(0), inst.GetActiveRequests())
	first := acquireTestLease(t, inst)
	assert.Equal(t, int64(1), inst.GetActiveRequests())
	second := acquireTestLease(t, inst)
	assert.Equal(t, int64(2), inst.GetActiveRequests())
	first.Release()
	assert.Equal(t, int64(1), inst.GetActiveRequests())
	second.Release()
	assert.Equal(t, int64(0), inst.GetActiveRequests())
}

func TestInstance_ConcurrencyGating(t *testing.T) {
	t.Run("unlimited_when_not_initialized", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 0, 10)
		assert.Equal(t, 0, inst.ConcurrencyLimit())
		acquireTestLease(t, inst).Release()
	})

	t.Run("unlimited_when_zero", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 0, MaxQueuedRequests: 100, QueueTimeout: 0})
		assert.Equal(t, 0, inst.ConcurrencyLimit())
		acquireTestLease(t, inst).Release()
	})

	t.Run("single_slot_reject", func(t *testing.T) {
		inst := NewInstance("id", "mlx", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: 0})

		first := acquireTestLease(t, inst)
		lease, err := inst.Acquire(context.Background(), nil)
		assert.ErrorIs(t, err, ErrQueueFull)
		assert.Nil(t, lease)
		first.Release()
		acquireTestLease(t, inst).Release()
	})

	t.Run("multi_slot", func(t *testing.T) {
		inst := NewInstance("id", "vllm", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 2, MaxQueuedRequests: 100, QueueTimeout: 0})
		assert.Equal(t, 2, inst.ConcurrencyLimit())

		first := acquireTestLease(t, inst)
		second := acquireTestLease(t, inst)
		lease, err := inst.Acquire(context.Background(), nil)
		assert.ErrorIs(t, err, ErrQueueFull)
		assert.Nil(t, lease)
		first.Release()
		third := acquireTestLease(t, inst)
		second.Release()
		third.Release()
	})

	t.Run("queue_timeout_succeeds", func(t *testing.T) {
		inst := NewInstance("id", "vllm", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: 100 * time.Millisecond})

		first := acquireTestLease(t, inst)
		go func() {
			time.Sleep(20 * time.Millisecond)
			first.Release()
		}()
		acquireTestLease(t, inst).Release()
	})

	t.Run("queue_timeout_expires", func(t *testing.T) {
		inst := NewInstance("id", "vllm", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: 20 * time.Millisecond})

		first := acquireTestLease(t, inst)
		activity := inst.GetLastActivity()
		lease, err := inst.Acquire(context.Background(), nil)
		assert.ErrorIs(t, err, ErrQueueWaitExpired)
		assert.Nil(t, lease)
		assert.Equal(t, int64(1), inst.GetActiveRequests())
		assert.Equal(t, activity, inst.GetLastActivity())
		first.Release()
	})

	t.Run("context_cancellation", func(t *testing.T) {
		inst := NewInstance("id", "vllm", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: 5 * time.Second})

		first := acquireTestLease(t, inst)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		lease, err := inst.Acquire(ctx, nil)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, lease)
		first.Release()
	})

	t.Run("tracks_active_requests", func(t *testing.T) {
		inst := NewInstance("id", "mlx", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 2, MaxQueuedRequests: 100, QueueTimeout: 0})

		assert.Equal(t, int64(0), inst.GetActiveRequests())
		first := acquireTestLease(t, inst)
		assert.Equal(t, int64(1), inst.GetActiveRequests())
		second := acquireTestLease(t, inst)
		assert.Equal(t, int64(2), inst.GetActiveRequests())
		first.Release()
		assert.Equal(t, int64(1), inst.GetActiveRequests())
		second.Release()
		assert.Equal(t, int64(0), inst.GetActiveRequests())
	})

	t.Run("tracks_active_requests_unlimited", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 0, 10)

		lease := acquireTestLease(t, inst)
		assert.Equal(t, int64(1), inst.GetActiveRequests())
		lease.Release()
		assert.Equal(t, int64(0), inst.GetActiveRequests())
	})

	t.Run("concurrent_stress", func(t *testing.T) {
		const slots = 3
		const goroutines = 50
		inst := NewInstance("id", "vllm", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: slots, MaxQueuedRequests: 100, QueueTimeout: 5 * time.Second})

		var wg sync.WaitGroup
		var errCount atomic.Int64
		for range goroutines {
			wg.Go(func() {
				lease, err := inst.Acquire(context.Background(), nil)
				if err != nil {
					errCount.Add(1)
					return
				}
				time.Sleep(time.Millisecond)
				lease.Release()
			})
		}
		wg.Wait()

		assert.Equal(t, int64(0), errCount.Load(), "all goroutines should eventually acquire a slot")
		assert.Equal(t, int64(0), inst.GetActiveRequests(), "all requests should be released")
	})

	t.Run("toinfo_includes_limit", func(t *testing.T) {
		inst := NewInstance("id", "mlx", "m", 8090, 0, 10)
		inst.InitConcurrencyLimit(ConcurrencyConfig{MaxConcurrentRequests: 1, MaxQueuedRequests: 100, QueueTimeout: 0})

		info := inst.ToInfo("node1")
		assert.Equal(t, 1, info.MaxConcurrentRequests)
	})
}

func TestInstance_ActiveRequests_Concurrent(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)
	leases := make([]*Lease, 100)

	var wg sync.WaitGroup
	for n := range leases {
		wg.Go(func() {
			lease, err := inst.Acquire(context.Background(), nil)
			if assert.NoError(t, err) {
				leases[n] = lease
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int64(100), inst.GetActiveRequests())
	for _, lease := range leases {
		if lease != nil {
			wg.Go(lease.Release)
		}
	}
	wg.Wait()
	assert.Equal(t, int64(0), inst.GetActiveRequests())
}

func TestInstance_TryLog(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 2)

	inst.TryLog("msg1")
	inst.TryLog("msg2")
	inst.TryLog("msg3") // should be dropped (buffer full)

	assert.Equal(t, "msg1", <-inst.Logs)
	assert.Equal(t, "msg2", <-inst.Logs)
}

func TestInstance_CloseLogs(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)

	inst.CloseLogs()
	inst.TryLog("after close") // should not panic

	// Double close should not panic
	inst.CloseLogs()
}

func TestInstance_ToInfo(t *testing.T) {
	inst := NewInstance("test-id", "vllm", "llama3", 8000, 5*time.Minute, 10)
	inst.SourceRepo = "huggingface"
	inst.SizeBytes = 1024
	inst.Processor = "GPU"
	inst.ContextLength = 4096
	inst.ProcessID = 12345
	inst.HealthURL = "http://localhost:8000/health"
	inst.Config = Config{
		Parameters: map[string]string{"key": "val"},
		EnvVars:    map[string]string{"ENV": "test"},
	}
	inst.MarkRunning(false, nil)

	info := inst.ToInfo("node1")

	assert.Equal(t, "test-id", info.ID)
	assert.Equal(t, "vllm", info.Provider)
	assert.Equal(t, "llama3", info.Model)
	assert.Equal(t, "huggingface", info.SourceRepo)
	assert.Equal(t, int64(1024), info.SizeBytes)
	assert.Equal(t, "GPU", info.Processor)
	assert.Equal(t, 4096, info.ContextLength)
	assert.Equal(t, 12345, info.ProcessID)
	assert.Equal(t, 8000, info.Port)
	assert.Equal(t, StatusRunning, info.Status)
	assert.Equal(t, "node1", info.Node)
	assert.Equal(t, "val", info.Parameters["key"])
	assert.Equal(t, "test", info.Environment["ENV"])
	assert.Equal(t, "http://node1:8000/health", info.HealthURL,
		"HealthURL must be rewritten with the snapshot hostname so cross-node callers can dial it")
}

func TestInstanceToInfoReportsSelectedRuntime(t *testing.T) {
	inst := NewInstance("test-id", "provider", "model", 8000, time.Minute, 1)
	inst.Config.Runtime = "requested-runtime"
	inst.SetResolved(Resolved{Runtime: "selected-feature"})
	assert.Equal(t, "selected-feature", inst.ToInfo("node").Runtime)
	assert.Equal(t, "requested-runtime", inst.SnapshotConfig().Runtime)
}

func TestResolvedWireEndpointsContract(t *testing.T) {
	for _, endpoints := range [][]string{nil, {}, {"messages"}} {
		inst := NewInstance("test-id", "provider", "model", 8000, time.Minute, 1)
		inst.SetResolved(Resolved{WireEndpoints: endpoints})
		assert.Equal(t, endpoints, inst.Resolved().WireEndpoints)
		if len(endpoints) > 0 {
			endpoints[0] = "changed-input"
			got := inst.Resolved()
			assert.Equal(t, "messages", got.WireEndpoints[0])
			got.WireEndpoints[0] = "changed-output"
			assert.Equal(t, "messages", inst.Resolved().WireEndpoints[0])
		}
	}
}

func TestRewriteHealthURLHost(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		hostname string
		want     string
	}{
		{"localhost", "http://localhost:8000/health", "node1", "http://node1:8000/health"},
		{"127.0.0.1", "http://127.0.0.1:8000/health", "node1", "http://node1:8000/health"},
		{"ipv6 loopback", "http://[::1]:8000/health", "node1", "http://node1:8000/health"},
		{"already remote", "http://10.0.0.5:8000/health", "node1", "http://10.0.0.5:8000/health"},
		{"empty url", "", "node1", ""},
		{"empty hostname", "http://localhost:8000/health", "", "http://localhost:8000/health"},
		{"no port", "http://localhost/health", "node1", "http://node1/health"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteHealthURLHost(tc.input, tc.hostname)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestInstance_MarkRunningFirstStamp(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)
	require.True(t, inst.StartedAt.IsZero())

	inst.MarkRunning(false, nil)
	first := inst.StartedAt
	assert.False(t, first.IsZero())

	time.Sleep(time.Millisecond)
	inst.MarkRunning(false, nil)
	assert.Equal(t, first, inst.StartedAt) // should not change
}

func TestInstance_MarkRunningCommitsOnlyTransitions(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)
	defer inst.Cancel()
	var firsts []bool
	commit := func(first bool) { firsts = append(firsts, first) }
	require.True(t, inst.MarkRunning(false, commit))
	lastHealthy := inst.LastHealthy
	require.False(t, inst.MarkRunning(true, commit))
	assert.Equal(t, lastHealthy, inst.LastHealthy)
	assert.Equal(t, []bool{true}, firsts)
	inst.MarkUnhealthy("temporary health failure")
	require.True(t, inst.MarkRunning(true, commit))
	assert.Equal(t, []bool{true, false}, firsts)
	assert.Equal(t, StatusRunning, inst.GetStatus())
	assert.Empty(t, inst.GetErrorMessage())
}

func TestInstance_SetLoadedWithoutContext(t *testing.T) {
	inst := &Instance{Status: StatusRunning}
	loaded := Loaded{ContextLength: 8192}
	inst.SetLoaded(loaded)
	assert.Equal(t, loaded, inst.Loaded())
	assert.Equal(t, 8192, inst.ContextLength)
}

func TestInstanceLoadedProcessor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		loaded Loaded
		want   string
	}{
		{"unknown", Loaded{}, ""},
		{"layers", Loaded{GPULayers: 8, Layers: 29}, "28% GPU / 72% CPU"},
		{"full GPU", Loaded{GPULayers: 29, Layers: 29}, "100% GPU"},
		{"full CPU", Loaded{Layers: 29}, "100% CPU"},
		{"bytes precede layers", Loaded{Size: 100, VRAM: 50, GPULayers: 8, Layers: 29}, "50% GPU / 50% CPU"},
		{"GPU bytes precede CPU layers", Loaded{Size: 100, VRAM: 100, Layers: 29}, "100% GPU"},
		{"CPU bytes precede GPU layers", Loaded{Size: 100, GPULayers: 29, Layers: 29}, "100% CPU"},
		{"override weights precede layers", Loaded{CPUOverrides: true, CPUWeights: 70, GPUWeights: 30, GPULayers: 49, Layers: 49}, "30% GPU / 70% CPU"},
		{"embedding buffer without overrides", Loaded{CPUWeights: 12, GPUWeights: 88, GPULayers: 49, Layers: 49}, "100% GPU"},
		{"overrides without weights", Loaded{CPUOverrides: true, GPULayers: 8, Layers: 29}, "28% GPU / 72% CPU"},
		{"overrides without placement", Loaded{CPUOverrides: true}, ""},
		{"bytes precede override weights", Loaded{Size: 100, VRAM: 80, CPUOverrides: true, CPUWeights: 70, GPUWeights: 30, GPULayers: 49, Layers: 49}, "80% GPU / 20% CPU"},
		{"CPU weights", Loaded{CPUOverrides: true, CPUWeights: 70, GPULayers: 49, Layers: 49}, "100% CPU"},
		{"GPU weights", Loaded{CPUOverrides: true, GPUWeights: 30}, "100% GPU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := NewInstance("id", "engine", "model", 0, 0, 0)
			run.SetLoaded(tc.loaded)
			run.UpdateLoaded(func(l *Loaded) { l.ContextLength = 8192 })
			assert.Equal(t, tc.want, run.ToInfo("").Processor)
			assert.Equal(t, 8192, run.ToInfo("").ContextLength)
			assert.Equal(t, tc.loaded.Size, run.ToInfo("").SizeBytes)
			assert.Equal(t, tc.loaded.Layers, run.Loaded().Layers)
		})
	}
}

func TestInstanceFailurePreservesLoadedEvidence(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "starting", true: "running"}[started], func(t *testing.T) {
			run := NewInstance("id", "engine", "model", 0, 0, 0)
			if started {
				run.MarkRunning(false, nil)
			}
			run.SetLoaded(Loaded{Size: 100, VRAM: 50, GPULayers: 8, Layers: 29, ContextLength: 8192, Digest: "keep"})
			run.MarkFailed("failed")
			assert.Equal(t, "50% GPU / 50% CPU", run.ToInfo("").Processor)
			assert.Equal(t, 8192, run.ToInfo("").ContextLength)
			assert.Equal(t, "keep", run.Loaded().Digest)
			assert.Equal(t, int64(29), run.Loaded().Layers)
		})
	}
}

func TestInstance_Cancel(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)
	ctx := inst.Context()

	select {
	case <-ctx.Done():
		t.Fatal("context should not be done yet")
	default:
	}

	inst.Cancel()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context should be done after cancel")
	}
}

func TestInstance_ReadinessNotifiesIdle(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)
	registry := NewRegistry()
	require.NoError(t, registry.Register(inst))
	<-registry.Changes()
	inst.UpdateLastUsed()
	select {
	case <-registry.Changes():
	case <-time.After(time.Second):
		t.Fatal("readiness did not notify idle observer")
	}
	assert.False(t, inst.IdleSnapshot().Released, "zero remains unarmed until release")
	assert.False(t, inst.GetLastActivity().IsZero())
}

func TestInstance_WaitForGoroutines_CleanDrain(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)

	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		time.Sleep(10 * time.Millisecond)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, inst.WaitForGoroutines(ctx))
}

func TestInstance_WaitForGoroutines_ContextDeadline(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)

	// Tracked goroutine that only exits on instance.Cancel() — simulating a
	// health-monitor loop that ignores the external context.
	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		<-inst.Context().Done()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := inst.WaitForGoroutines(ctx)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 500*time.Millisecond, "WaitForGoroutines should return promptly when ctx deadline fires")

	// Clean up so the test doesn't leak the blocked goroutine into sibling tests.
	inst.Cancel()
	require.NoError(t, inst.WaitForGoroutines(context.Background()))
}

func TestInstance_GetProcessGroupID(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)
	assert.Equal(t, 0, inst.GetProcessGroupID())

	inst.SetProcessInfo(1234, 5678, nil)
	assert.Equal(t, 5678, inst.GetProcessGroupID())
	assert.Equal(t, 1234, inst.GetProcessID())
}

func TestInstance_GetProcessGroupID_Concurrent(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				inst.SetProcessInfo(n, n*10, nil)
			} else {
				_ = inst.GetProcessGroupID()
				_ = inst.GetProcessID()
			}
		}(i)
	}
	wg.Wait()
}

func TestInstance_LastActivityTime(t *testing.T) {
	t.Run("reads the activity stamp", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, time.Hour, 10)

		// Stamp readiness activity
		inst.UpdateLastUsed()
		usedAt := inst.GetLastActivity()

		// Set LastActivity to a later time
		later := usedAt.Add(time.Minute)
		inst.mu.Lock()
		inst.LastActivity = later
		inst.mu.Unlock()

		// Read the updated activity stamp
		assert.Equal(t, later, inst.GetLastActivity())
	})

	t.Run("readiness sets activity", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, time.Hour, 10)
		inst.UpdateLastUsed()

		activity := inst.GetLastActivity()
		assert.False(t, activity.IsZero())
	})

	t.Run("returns zero when nothing set", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, time.Hour, 10)
		assert.True(t, inst.GetLastActivity().IsZero())
	})
}

// LeftStarting closes on every way out of starting, once, and a channel
// asked for after the run left is already closed.
func TestLeftStartingClosesOnEveryExit(t *testing.T) {
	for name, leave := range map[string]func(*Instance){
		"running":   func(i *Instance) { i.MarkRunning(false, nil) },
		"unhealthy": func(i *Instance) { i.MarkUnhealthy("x") },
		"failed":    func(i *Instance) { i.MarkFailed("x") },
		"stopping":  func(i *Instance) { i.SetStatus(StatusStopping) },
	} {
		t.Run(name, func(t *testing.T) {
			inst := NewInstance("id", "p", "m", 1, time.Minute, 0)
			left := inst.LeftStarting()
			select {
			case <-left:
				t.Fatal("closed while starting")
			default:
			}
			leave(inst)
			<-left
			inst.SetStatus(StatusStarting)
			inst.SetStatus(StatusStopped) // a second exit must not close it again
			<-inst.LeftStarting()
		})
	}
}

// Admit is refused once the run stopped waiting, and clears waiting_on.
func TestAdmit(t *testing.T) {
	inst := NewInstance("id", "p", "m", 1, time.Minute, 0)
	inst.SetWaitingOn("other")
	assert.Equal(t, "other", inst.ToInfo("n").WaitingOn)
	require.True(t, inst.Admit())
	assert.True(t, inst.Admitted())
	assert.Empty(t, inst.WaitingOn())
	stopped := NewInstance("id2", "p", "m", 2, time.Minute, 0)
	stopped.SetStatus(StatusStopping)
	assert.False(t, stopped.Admit())
}

func TestIdleSnapshotReadsKeepAliveAndActivity(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, time.Minute, 1)
	idle := inst.IdleSnapshot()
	assert.Equal(t, time.Minute, idle.KeepAlive)
	assert.Equal(t, inst.GetLastActivity(), idle.Activity)
}

// One side owns the signal: a launch claim excludes every stop, a stop claim excludes the launch, and a stop
// may retry its own claim.
func TestClaimTerminationOwner(t *testing.T) {
	launched := NewInstance("a", "p", "m", 0, time.Minute, 1)
	assert.Zero(t, launched.ClaimTermination(false), "no process yet")
	launched.SetProcessInfo(42, 42, nil)
	assert.Equal(t, 42, launched.ClaimTermination(false))
	assert.Zero(t, launched.ClaimTermination(true))

	stopped := NewInstance("b", "p", "m", 0, time.Minute, 1)
	stopped.SetProcessInfo(7, 7, nil)
	assert.Equal(t, 7, stopped.ClaimTermination(true))
	assert.Zero(t, stopped.ClaimTermination(false))
	assert.Equal(t, 7, stopped.ClaimTermination(true), "a stop retries its own claim")
}

func TestToInfoKeepAliveReadsAsItsParameter(t *testing.T) {
	assert.Equal(t, "-1", NewInstance("pinned", "ollama", "m", 0, -1, 1).ToInfo("node").KeepAlive)
	assert.Equal(t, "5m0s", NewInstance("timed", "vllm", "m", 8000, 5*time.Minute, 1).ToInfo("node").KeepAlive)
}

func TestStartedAtAccessorConcurrentReadiness(t *testing.T) {
	inst := NewInstance("served", "ollama", "m", 0, -1, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			inst.MarkRunning(false, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			_ = inst.GetStartedAt()
		}
	}()
	wg.Wait()
	assert.False(t, inst.GetStartedAt().IsZero())
}
