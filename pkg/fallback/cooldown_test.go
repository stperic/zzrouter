package fallback

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCooldownManager_SetAndCheck(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	assert.False(t, cm.InCooldown("deploy-a"))

	cm.SetCooldown("deploy-a", 1*time.Second, ReasonRateLimit)
	assert.True(t, cm.InCooldown("deploy-a"))
	assert.False(t, cm.InCooldown("deploy-b"))
}

func TestCooldownManager_Expiry(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	cm.SetCooldown("deploy-a", 50*time.Millisecond, ReasonRateLimit)
	assert.True(t, cm.InCooldown("deploy-a"))

	time.Sleep(60 * time.Millisecond)
	assert.False(t, cm.InCooldown("deploy-a"))
}

func TestCooldownManager_ClearCooldown(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	cm.SetCooldown("deploy-a", 10*time.Second, ReasonRateLimit)
	require.True(t, cm.InCooldown("deploy-a"))

	cm.ClearCooldown("deploy-a")
	assert.False(t, cm.InCooldown("deploy-a"))
}

func TestCooldownManager_CooldownStatus(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	// Not in cooldown
	assert.Equal(t, time.Duration(0), cm.CooldownStatus("deploy-a"))

	// In cooldown
	cm.SetCooldown("deploy-a", 5*time.Second, ReasonRateLimit)
	remaining := cm.CooldownStatus("deploy-a")
	assert.True(t, remaining > 4*time.Second, "expected >4s remaining, got %v", remaining)
	assert.True(t, remaining <= 5*time.Second, "expected <=5s remaining, got %v", remaining)
}

func TestCooldownManager_CooldownStatus_Expired(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	cm.SetCooldown("deploy-a", 10*time.Millisecond, ReasonRateLimit)
	time.Sleep(20 * time.Millisecond)

	assert.Equal(t, time.Duration(0), cm.CooldownStatus("deploy-a"))
}

func TestCooldownManager_MultipleDeploys(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	cm.SetCooldown("deploy-a", 1*time.Second, ReasonRateLimit)
	cm.SetCooldown("deploy-b", 50*time.Millisecond, ReasonUnavailable)
	cm.SetCooldown("deploy-c", 1*time.Second, ReasonQuota)

	assert.True(t, cm.InCooldown("deploy-a"))
	assert.True(t, cm.InCooldown("deploy-b"))
	assert.True(t, cm.InCooldown("deploy-c"))

	time.Sleep(60 * time.Millisecond)

	assert.True(t, cm.InCooldown("deploy-a"))
	assert.False(t, cm.InCooldown("deploy-b"))
	assert.True(t, cm.InCooldown("deploy-c"))
}

func TestCooldownManager_OverwriteCooldown(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	cm.SetCooldown("deploy-a", 50*time.Millisecond, ReasonRateLimit)
	time.Sleep(30 * time.Millisecond)

	// Extend cooldown
	cm.SetCooldown("deploy-a", 1*time.Second, ReasonQuota)
	time.Sleep(30 * time.Millisecond)

	// Should still be in cooldown (extended)
	assert.True(t, cm.InCooldown("deploy-a"))
}

func TestCooldownManager_ClearNonExistent(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	// Should not panic
	cm.ClearCooldown("nonexistent")
	assert.False(t, cm.InCooldown("nonexistent"))
}

func TestCooldownManager_Stop(t *testing.T) {
	cm := NewCooldownManager()
	cm.Stop()

	// Should still work after stop (just no cleanup goroutine)
	cm.SetCooldown("deploy-a", 1*time.Second, ReasonRateLimit)
	assert.True(t, cm.InCooldown("deploy-a"))
}

func TestCooldownManager_GetEntry(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	// Not in cooldown
	dur, reason := cm.GetEntry("deploy-a")
	assert.Equal(t, time.Duration(0), dur)
	assert.Equal(t, "", reason)

	// In cooldown with reason
	cm.SetCooldown("deploy-a", 5*time.Second, ReasonQuota)
	dur, reason = cm.GetEntry("deploy-a")
	assert.True(t, dur > 4*time.Second)
	assert.Equal(t, ReasonQuota, reason)
}

func TestCooldownManager_Snapshot(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	cm.SetCooldown("deploy-a", 5*time.Second, ReasonRateLimit)
	cm.SetCooldown("deploy-b", 10*time.Millisecond, ReasonQuota)

	time.Sleep(20 * time.Millisecond)

	snap := cm.Snapshot()
	assert.Len(t, snap, 1) // deploy-b expired
	assert.Equal(t, ReasonRateLimit, snap["deploy-a"].Reason)
}

func TestCooldownManager_EmitsStartedAndEndedTransitions(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	type event struct{ kind, replica, reason string }
	var got []event
	var mu sync.Mutex
	cm.SetEventEmitter(func(kind, replica, reason string) {
		mu.Lock()
		got = append(got, event{kind, replica, reason})
		mu.Unlock()
	})

	cm.SetCooldown("r1", 100*time.Millisecond, ReasonRateLimit)
	// Back-to-back SetCooldown on the same active replica must NOT
	// re-fire started — one event per cooldown episode.
	cm.SetCooldown("r1", 100*time.Millisecond, ReasonRateLimit)
	cm.ClearCooldown("r1")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 2)
	assert.Equal(t, "cooldown_started", got[0].kind)
	assert.Equal(t, "r1", got[0].replica)
	assert.Equal(t, ReasonRateLimit, got[0].reason)
	assert.Equal(t, "cooldown_ended", got[1].kind)
}

func TestCooldownManager_SetCooldownIfLonger_EmitsStarted(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	type event struct{ kind, replica, reason string }
	var got []event
	var mu sync.Mutex
	cm.SetEventEmitter(func(kind, replica, reason string) {
		mu.Lock()
		got = append(got, event{kind, replica, reason})
		mu.Unlock()
	})

	cm.SetCooldownIfLonger("r1", 100*time.Millisecond, ReasonRateLimit)
	cm.SetCooldownIfLonger("r1", 50*time.Millisecond, ReasonRateLimit)  // shorter, no transition
	cm.SetCooldownIfLonger("r1", 200*time.Millisecond, ReasonRateLimit) // extension while active

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 1, "only the out-of-cooldown→in-cooldown transition emits")
	assert.Equal(t, "cooldown_started", got[0].kind)
	assert.Equal(t, "r1", got[0].replica)
	assert.Equal(t, ReasonRateLimit, got[0].reason)
}

func TestCooldownManager_Cleanup_EmitsEnded(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()

	type event struct{ kind, replica, reason string }
	var got []event
	var mu sync.Mutex
	cm.SetEventEmitter(func(kind, replica, reason string) {
		mu.Lock()
		got = append(got, event{kind, replica, reason})
		mu.Unlock()
	})

	cm.SetCooldown("r1", 10*time.Millisecond, ReasonQuota)
	cm.SetCooldown("r2", 10*time.Second, ReasonRateLimit)
	time.Sleep(20 * time.Millisecond)
	cm.cleanup()

	mu.Lock()
	defer mu.Unlock()
	// Expect: started/r1, started/r2, ended/r1. r2 still active.
	require.Len(t, got, 3)
	assert.Equal(t, "cooldown_started", got[0].kind)
	assert.Equal(t, "cooldown_started", got[1].kind)
	assert.Equal(t, "cooldown_ended", got[2].kind)
	assert.Equal(t, "r1", got[2].replica)
	assert.Equal(t, ReasonQuota, got[2].reason)
}

func TestCooldownManager_NilEmitterIsNoop(t *testing.T) {
	cm := NewCooldownManager()
	defer cm.Stop()
	// No SetEventEmitter call — must not panic on transitions.
	cm.SetCooldown("r1", time.Second, ReasonQuota)
	cm.ClearCooldown("r1")
}
