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

func TestInstance_StatusTransitions(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)

	assert.Equal(t, StatusStarting, inst.GetStatus())

	inst.MarkRunning()
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
				inst.MarkRunning()
			case 1:
				inst.MarkUnhealthy("test")
			case 2:
				inst.MarkFailed("test")
			case 3:
				inst.MarkStarting()
			}
			_ = inst.GetStatus()
			_ = inst.IsHealthy()
			_ = inst.IsRunning()
		}(i)
	}
	wg.Wait()
}

func TestInstance_KeepAlive(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 0, 10)
		assert.False(t, inst.IsIdle())
		assert.False(t, inst.IsExpired(time.Now()))
		assert.True(t, inst.ShouldUnload()) // 0 = immediate unload
	})

	t.Run("indefinite", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, -1, 10)
		assert.False(t, inst.IsIdle())
		assert.False(t, inst.IsExpired(time.Now()))
		assert.False(t, inst.ShouldUnload())
	})

	t.Run("idle_detection", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 50*time.Millisecond, 10)
		inst.UpdateLastUsed()
		assert.False(t, inst.IsIdle())

		time.Sleep(60 * time.Millisecond)
		assert.True(t, inst.IsIdle())
		assert.True(t, inst.ShouldUnload())
	})

	t.Run("expired", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 100*time.Millisecond, 10)
		inst.UpdateLastUsed()

		assert.False(t, inst.IsExpired(time.Now()))
		assert.True(t, inst.IsExpired(time.Now().Add(200*time.Millisecond)))
	})
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
		assert.True(t, inst.ShouldUnload(), "KeepAlive=0 means immediate unload")
	})

	t.Run("negative KeepAlive override preserves indefinite semantics", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 1*time.Hour, 10)
		inst.StampActivityWithHints(&keepalive.Override{Duration: ptr(-1 * time.Second)})
		assert.False(t, inst.ShouldUnload(), "negative KeepAlive means indefinite")
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
	assert.Equal(t, int64(1), inst.IncrementActiveRequests())
	assert.Equal(t, int64(2), inst.IncrementActiveRequests())
	assert.Equal(t, int64(2), inst.GetActiveRequests())
	assert.Equal(t, int64(1), inst.DecrementActiveRequests())
	assert.Equal(t, int64(1), inst.GetActiveRequests())
}

func TestInstance_ConcurrencyGating(t *testing.T) {
	t.Run("unlimited_when_not_initialized", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 0, 10)
		assert.Equal(t, 0, inst.ConcurrencyLimit())
		// Should always succeed immediately
		err := inst.AcquireConcurrency(context.Background())
		assert.NoError(t, err)
		inst.ReleaseConcurrency() // no-op, should not panic
	})

	t.Run("unlimited_when_zero", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(0, 0)
		assert.Equal(t, 0, inst.ConcurrencyLimit())
		err := inst.AcquireConcurrency(context.Background())
		assert.NoError(t, err)
	})

	t.Run("single_slot_reject", func(t *testing.T) {
		inst := NewInstance("id", "mlx", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(1, 0) // queue_timeout=0 → reject immediately

		// First acquire succeeds
		err := inst.AcquireConcurrency(context.Background())
		require.NoError(t, err)

		// Second acquire is rejected immediately
		err = inst.AcquireConcurrency(context.Background())
		assert.ErrorIs(t, err, ErrAtCapacity)

		// Release frees the slot
		inst.ReleaseConcurrency()

		// Now it succeeds again
		err = inst.AcquireConcurrency(context.Background())
		assert.NoError(t, err)
		inst.ReleaseConcurrency()
	})

	t.Run("multi_slot", func(t *testing.T) {
		inst := NewInstance("id", "vllm", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(2, 0)
		assert.Equal(t, 2, inst.ConcurrencyLimit())

		// Two acquires succeed
		require.NoError(t, inst.AcquireConcurrency(context.Background()))
		require.NoError(t, inst.AcquireConcurrency(context.Background()))

		// Third is rejected
		err := inst.AcquireConcurrency(context.Background())
		assert.ErrorIs(t, err, ErrAtCapacity)

		// Release one, now third succeeds
		inst.ReleaseConcurrency()
		assert.NoError(t, inst.AcquireConcurrency(context.Background()))

		inst.ReleaseConcurrency()
		inst.ReleaseConcurrency()
	})

	t.Run("queue_timeout_succeeds", func(t *testing.T) {
		inst := NewInstance("id", "vllm", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(1, 100*time.Millisecond)

		// Fill the slot
		require.NoError(t, inst.AcquireConcurrency(context.Background()))

		// Release after 20ms in background
		go func() {
			time.Sleep(20 * time.Millisecond)
			inst.ReleaseConcurrency()
		}()

		// Should succeed within the 100ms timeout
		err := inst.AcquireConcurrency(context.Background())
		assert.NoError(t, err)
		inst.ReleaseConcurrency()
	})

	t.Run("queue_timeout_expires", func(t *testing.T) {
		inst := NewInstance("id", "vllm", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(1, 20*time.Millisecond)

		// Fill the slot
		require.NoError(t, inst.AcquireConcurrency(context.Background()))

		// Should timeout
		err := inst.AcquireConcurrency(context.Background())
		assert.ErrorIs(t, err, ErrAtCapacity)

		inst.ReleaseConcurrency()
	})

	t.Run("context_cancellation", func(t *testing.T) {
		inst := NewInstance("id", "vllm", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(1, 5*time.Second) // long timeout

		// Fill the slot
		require.NoError(t, inst.AcquireConcurrency(context.Background()))

		// Cancel context immediately
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := inst.AcquireConcurrency(ctx)
		assert.ErrorIs(t, err, context.Canceled)

		inst.ReleaseConcurrency()
	})

	t.Run("tracks_active_requests", func(t *testing.T) {
		inst := NewInstance("id", "mlx", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(2, 0)

		assert.Equal(t, int64(0), inst.GetActiveRequests())

		require.NoError(t, inst.AcquireConcurrency(context.Background()))
		assert.Equal(t, int64(1), inst.GetActiveRequests())

		require.NoError(t, inst.AcquireConcurrency(context.Background()))
		assert.Equal(t, int64(2), inst.GetActiveRequests())

		inst.ReleaseConcurrency()
		assert.Equal(t, int64(1), inst.GetActiveRequests())

		inst.ReleaseConcurrency()
		assert.Equal(t, int64(0), inst.GetActiveRequests())
	})

	t.Run("tracks_active_requests_unlimited", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, 0, 10)
		// No InitConcurrencyLimit — unlimited mode

		require.NoError(t, inst.AcquireConcurrency(context.Background()))
		assert.Equal(t, int64(1), inst.GetActiveRequests())

		inst.ReleaseConcurrency()
		assert.Equal(t, int64(0), inst.GetActiveRequests())
	})

	t.Run("concurrent_stress", func(t *testing.T) {
		const slots = 3
		const goroutines = 50
		inst := NewInstance("id", "vllm", "m", 0, 0, 10)
		inst.InitConcurrencyLimit(slots, 5*time.Second)

		var wg sync.WaitGroup
		errCount := int64(0)
		for range goroutines {
			wg.Go(func() {
				if err := inst.AcquireConcurrency(context.Background()); err != nil {
					atomic.AddInt64(&errCount, 1)
					return
				}
				// Simulate brief work
				time.Sleep(time.Millisecond)
				inst.ReleaseConcurrency()
			})
		}
		wg.Wait()

		assert.Equal(t, int64(0), errCount, "all goroutines should eventually acquire a slot")
		assert.Equal(t, int64(0), inst.GetActiveRequests(), "all requests should be released")
	})

	t.Run("toinfo_includes_limit", func(t *testing.T) {
		inst := NewInstance("id", "mlx", "m", 8090, 0, 10)
		inst.InitConcurrencyLimit(1, 0)

		info := inst.ToInfo("node1")
		assert.Equal(t, 1, info.MaxConcurrentRequests)
	})
}

func TestInstance_ActiveRequests_Concurrent(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			inst.IncrementActiveRequests()
		})
	}
	wg.Wait()
	assert.Equal(t, int64(100), inst.GetActiveRequests())
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
	inst.MarkRunning()

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

func TestInstance_SetStartedAtIfZero(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 10)
	require.True(t, inst.StartedAt.IsZero())

	inst.SetStartedAtIfZero()
	first := inst.StartedAt
	assert.False(t, first.IsZero())

	time.Sleep(time.Millisecond)
	inst.SetStartedAtIfZero()
	assert.Equal(t, first, inst.StartedAt) // should not change
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

func TestInstance_StopKeepAliveTimer(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, time.Hour, 10)
	inst.UpdateLastUsed() // starts the timer
	inst.StopKeepAliveTimer()
	// Should not panic, timer should be nil
	inst.StopKeepAliveTimer()
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
	t.Run("prefers LastActivity over lastUsedAt", func(t *testing.T) {
		inst := NewInstance("id", "p", "m", 0, time.Hour, 10)

		// Set lastUsedAt via UpdateLastUsed
		inst.UpdateLastUsed()
		usedAt := inst.GetLastUsedAt()

		// Set LastActivity to a later time
		later := usedAt.Add(time.Minute)
		inst.mu.Lock()
		inst.LastActivity = later
		inst.mu.Unlock()

		// GetLastActivity should return LastActivity, not lastUsedAt
		assert.Equal(t, later, inst.GetLastActivity())
	})

	t.Run("falls back to lastUsedAt", func(t *testing.T) {
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
