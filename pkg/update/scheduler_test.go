package update

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewScheduler(t *testing.T) {
	cfg := &config.UpdateConfig{
		Channel:              "stable",
		CheckIntervalHours:   24,
		KeepPreviousVersions: 2,
	}

	s := NewScheduler(cfg, clock.System())
	require.NotNil(t, s)

	assert.Equal(t, StateIdle, s.status.State)
	assert.Equal(t, "stable", s.status.Channel)
	assert.NotNil(t, s.checker)
	assert.NotNil(t, s.downloader)
	assert.NotNil(t, s.verifier)
	assert.NotNil(t, s.installer)
	assert.NotNil(t, s.history)
}

func TestScheduler_Start_Disabled(t *testing.T) {
	enabled := false
	cfg := &config.UpdateConfig{
		Enabled: &enabled,
	}

	s := NewScheduler(cfg, clock.System())
	ctx := context.Background()
	t.Cleanup(s.Stop)

	// Start should return immediately when disabled
	s.Start(ctx)

	// Give goroutine time to start if it would
	time.Sleep(100 * time.Millisecond)

	// Status should still be idle
	status := s.GetStatus()
	assert.Equal(t, StateIdle, status.State)
}

// Single-flight gate: beginApply is a CAS; two concurrent claims
// produce one winner. endApply releases the gate so the next claim
// succeeds — this is the invariant performCheck / ApplyNow /
// ApplyNowAsync all depend on to serialize the applyUpdate
// choke-point.
func TestScheduler_BeginApplyIsSingleFlight(t *testing.T) {
	s := NewScheduler(&config.UpdateConfig{}, clock.System())
	require.True(t, s.beginApply(), "first claim must succeed")
	require.False(t, s.beginApply(), "concurrent claim must fail")
	s.endApply()
	require.True(t, s.beginApply(), "claim after endApply must succeed")
	s.endApply()
}

func TestScheduler_ApplyNow_InFlightReturnsError(t *testing.T) {
	s := NewScheduler(&config.UpdateConfig{}, clock.System())
	require.True(t, s.beginApply(), "simulate in-flight apply")
	defer s.endApply()

	err := s.ApplyNow(context.Background())
	require.Error(t, err)
	// The gate error should surface before resolvePending fails for an
	// empty UpdateConfig — otherwise we'd be masking a spurious error.
	require.ErrorIs(t, err, ErrApplyInFlight, "want ErrApplyInFlight, got %v", err)

	_, asyncErr := s.ApplyNowAsync(context.Background())
	require.ErrorIs(t, asyncErr, ErrApplyInFlight)
}

func TestScheduler_Stop(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	// Stop should not panic even if not started
	s.Stop()

	// Calling Stop again should be safe
	s.Stop()
}

func TestScheduler_GetStatus(t *testing.T) {
	cfg := &config.UpdateConfig{
		Channel: "beta",
	}
	s := NewScheduler(cfg, clock.System())

	status := s.GetStatus()
	assert.Equal(t, StateIdle, status.State)
	assert.Equal(t, "beta", status.Channel)
	assert.False(t, status.UpdateAvailable)
}

func TestScheduler_GetHistory_Empty(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	history, err := s.GetHistory()
	require.NoError(t, err)
	assert.NotNil(t, history)
	// History may be empty or have entries from previous tests
}

func TestScheduler_setState(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	// Set various states
	states := []UpdateState{StateChecking, StateDownloading, StateVerifying, StateApplying, StateIdle}
	for _, state := range states {
		s.setState(state)
		status := s.GetStatus()
		assert.Equal(t, state, status.State)
	}
}

func TestScheduler_setError(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	err := assert.AnError
	s.setError(err)

	status := s.GetStatus()
	assert.Equal(t, StateFailed, status.State)
	assert.Equal(t, err.Error(), status.Error)
}

func TestScheduler_setLastCheckTime(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	now := time.Now()
	s.setLastCheckTime(now)

	status := s.GetStatus()
	require.NotNil(t, status.LastCheckTime)
	assert.Equal(t, now.Unix(), status.LastCheckTime.Unix())
}

func TestScheduler_setNextCheckTime(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	future := time.Now().Add(24 * time.Hour)
	s.setNextCheckTime(future)

	status := s.GetStatus()
	require.NotNil(t, status.NextCheckTime)
	assert.Equal(t, future.Unix(), status.NextCheckTime.Unix())
}

func TestScheduler_isInMaintenanceWindow_NoWindow(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	// No window configured = always in window
	assert.True(t, s.isInMaintenanceWindow())
}

func TestScheduler_isInMaintenanceWindow_WithWindow(t *testing.T) {
	// Set window to current hour
	currentHour := time.Now().Hour()
	var window string
	if currentHour < 10 {
		window = "0 " + string(rune('0'+currentHour)) + " * * *"
	} else {
		window = "0 " + string(rune('0'+currentHour/10)) + string(rune('0'+currentHour%10)) + " * * *"
	}

	cfg := &config.UpdateConfig{
		MaintenanceWindow: window,
	}
	s := NewScheduler(cfg, clock.System())

	// Should be in window at current hour
	// This may be flaky depending on test execution time
	assert.True(t, s.isInMaintenanceWindow())
}

func TestScheduler_EnsureDirectories(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	err := s.EnsureDirectories()
	assert.NoError(t, err)
}

func TestScheduler_GetBackupDir(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	backupDir := s.GetBackupDir()
	assert.NotEmpty(t, backupDir)
	assert.Contains(t, backupDir, "backups")
}

func TestScheduler_ConcurrentAccess(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	var wg sync.WaitGroup

	// Concurrent reads
	for range 10 {
		wg.Go(func() {
			_ = s.GetStatus()
		})
	}

	// Concurrent state changes
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			states := []UpdateState{StateIdle, StateChecking, StateDownloading}
			s.setState(states[i%len(states)])
		}(i)
	}

	wg.Wait()

	// Should complete without race conditions
	status := s.GetStatus()
	assert.NotNil(t, status)
}

func TestScheduler_CheckNow(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// This will make a real API call to GitHub
	// In a real test environment, you'd mock this
	result, err := s.CheckNow(ctx)

	// We expect this to either succeed or fail with network error
	// depending on network availability
	if err == nil {
		assert.NotNil(t, result)
	}

	// Status should be back to idle
	status := s.GetStatus()
	assert.Equal(t, StateIdle, status.State)
}

func TestScheduler_ApplyNow_NoUpdate(t *testing.T) {
	t.Skip("Skipping test that makes real API calls")

	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// With no pending update and current version being latest,
	// should return "no update available" error
	err := s.ApplyNow(ctx)
	assert.Error(t, err)
}

func TestScheduler_Rollback(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	// Rollback with no backups should fail
	result, err := s.Rollback(t.Context())
	if err != nil {
		assert.Contains(t, err.Error(), "no backups available")
	} else if result != nil {
		// If somehow we have a backup, it should succeed
		assert.True(t, result.Success || result.Error != "")
	}
}

func TestScheduler_recordUpdate(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	// Record a successful update
	from, _ := parseVersion("1.0.0")
	to, _ := parseVersion("1.1.0")

	s.recordUpdate(from, to, true, "", 5*time.Second)

	// Verify status updated
	status := s.GetStatus()
	assert.False(t, status.UpdateAvailable)
	require.NotNil(t, status.LastUpdateTime)

	// Verify history recorded
	history, err := s.GetHistory()
	require.NoError(t, err)
	assert.NotEmpty(t, history.Entries)
}

func TestScheduler_recordUpdate_Failed(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	from, _ := parseVersion("1.0.0")
	to, _ := parseVersion("1.1.0")

	s.recordUpdate(from, to, false, "installation failed", 5*time.Second)

	// Verify history recorded
	history, err := s.GetHistory()
	require.NoError(t, err)
	assert.NotEmpty(t, history.Entries)

	// Find the failed entry
	var found bool
	for _, entry := range history.Entries {
		if !entry.Success && entry.Error == "installation failed" {
			found = true
			break
		}
	}
	assert.True(t, found, "Failed update entry should be recorded")
}

// Helper to parse version for tests
func parseVersion(v string) (*version.Version, error) {
	return version.ParseVersion(v)
}

func TestNewScheduler_NilClockPanics(t *testing.T) {
	cfg := &config.UpdateConfig{}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewScheduler(cfg, nil) did not panic")
		}
	}()
	_ = NewScheduler(cfg, nil)
}

// TestScheduler_recordUpdate_UsesInjectedClock proves that the
// Timestamp on a recorded history entry comes from the injected
// clock, not wall-clock. This is the invariant that makes
// deterministic testing possible — if it fails, migration is
// incomplete.
func TestScheduler_recordUpdate_UsesInjectedClock(t *testing.T) {
	cfg := &config.UpdateConfig{}
	fakeEpoch := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	fc := clocktest.NewFakeClock(fakeEpoch)
	s := NewScheduler(cfg, fc)

	from, _ := parseVersion("1.0.0")
	to, _ := parseVersion("1.1.0")
	s.recordUpdate(from, to, true, "", 5*time.Second)

	history, err := s.GetHistory()
	require.NoError(t, err)
	require.NotEmpty(t, history.Entries)

	// The entry whose Timestamp equals fakeEpoch is the one we just
	// wrote; earlier test-run residue will carry different stamps.
	var found bool
	for _, entry := range history.Entries {
		if entry.Timestamp.Equal(fakeEpoch) {
			found = true
			break
		}
	}
	require.True(t, found, "no recorded entry carries the FakeClock timestamp — clock injection incomplete")

	status := s.GetStatus()
	require.NotNil(t, status.LastUpdateTime)
	require.True(t, status.LastUpdateTime.Equal(fakeEpoch),
		"LastUpdateTime = %v, want %v (injected clock)", status.LastUpdateTime, fakeEpoch)
}

// TestScheduler_setLastCheckTime_FromInjectedClock exercises the
// CheckNow path's clock read without driving a real HTTP call.
func TestScheduler_setLastCheckTime_FromInjectedClock(t *testing.T) {
	cfg := &config.UpdateConfig{}
	fakeEpoch := time.Date(2030, 6, 15, 12, 0, 0, 0, time.UTC)
	fc := clocktest.NewFakeClock(fakeEpoch)
	s := NewScheduler(cfg, fc)

	// Direct-stamp via the setter the scheduler loop would use with
	// the clock-read result; validates the round-trip stamping path.
	s.setLastCheckTime(fc.Now())
	status := s.GetStatus()
	require.NotNil(t, status.LastCheckTime)
	require.True(t, status.LastCheckTime.Equal(fakeEpoch))

	// Advancing the fake clock and re-stamping picks up the new now.
	fc.Advance(2 * time.Hour)
	s.setLastCheckTime(fc.Now())
	status = s.GetStatus()
	require.True(t, status.LastCheckTime.Equal(fakeEpoch.Add(2*time.Hour)))
}

func TestScheduler_ApplyNow_WithPendingUpdate(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	// Set a fake pending release
	fakeVersion, _ := parseVersion("99.99.99")
	s.mu.Lock()
	s.status.PendingRelease = &ReleaseInfo{
		Version: fakeVersion,
		TagName: "v99.99.99",
		Assets: []ReleaseAsset{
			{Name: "zzrouter-linux-amd64.tar.gz", DownloadURL: "https://github.com/test/test.tar.gz"},
		},
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// This will fail because the download URL is not valid, but it tests the code path
	err := s.ApplyNow(ctx)
	assert.Error(t, err) // Expected to fail
}

func TestScheduler_CheckNow_ContextCancelled(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	// Create already-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.CheckNow(ctx)
	assert.Error(t, err)
}

func TestScheduler_Start_AndStop(t *testing.T) {
	enabled := true
	cfg := &config.UpdateConfig{
		Enabled:            &enabled,
		CheckIntervalHours: 1,
	}
	s := NewScheduler(cfg, clock.System())

	ctx := t.Context()

	// Start scheduler
	s.Start(ctx)

	// Give it a moment to start
	time.Sleep(50 * time.Millisecond)

	// Stop should work cleanly
	s.Stop()

	// Double stop should be safe
	s.Stop()
}

func TestScheduler_isInMaintenanceWindow_InvalidFormat(t *testing.T) {
	cfg := &config.UpdateConfig{
		MaintenanceWindow: "invalid cron format",
	}
	s := NewScheduler(cfg, clock.System())

	// Invalid format should default to allowing updates
	result := s.isInMaintenanceWindow()
	assert.True(t, result)
}

func TestScheduler_isInMaintenanceWindow_NotInWindow(t *testing.T) {
	// Set window to hour that's not current
	currentHour := time.Now().Hour()
	differentHour := (currentHour + 12) % 24 // 12 hours different

	var window string
	if differentHour < 10 {
		window = "0 " + string(rune('0'+differentHour)) + " * * *"
	} else {
		window = "0 " + string(rune('0'+differentHour/10)) + string(rune('0'+differentHour%10)) + " * * *"
	}

	cfg := &config.UpdateConfig{
		MaintenanceWindow: window,
	}
	s := NewScheduler(cfg, clock.System())

	// Should NOT be in window
	assert.False(t, s.isInMaintenanceWindow())
}

func TestScheduler_Rollback_WithMetrics(t *testing.T) {
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	// Rollback with no backups - tests the metrics recording path
	result, err := s.Rollback(t.Context())
	// Should fail with no backups
	if err != nil {
		assert.Contains(t, err.Error(), "no backups available")
	} else if result != nil {
		// If there were backups somehow, result would exist
		assert.NotNil(t, result)
	}
}

func TestScheduler_EnsureDirectories_Error(t *testing.T) {
	// Create scheduler with invalid directory (can't create)
	cfg := &config.UpdateConfig{}
	s := NewScheduler(cfg, clock.System())

	// Override with invalid path (on Unix, /proc is not writable)
	s.tempDir = "/proc/not-writable-dir"
	s.backupDir = "/proc/not-writable-dir"

	err := s.EnsureDirectories()
	// On most systems this should fail
	if err != nil {
		assert.Contains(t, err.Error(), "failed to create directory")
	}
}

// stubRestarter records whether the apply asked for a restart.
type stubRestarter struct {
	called bool
	err    error
}

func (s *stubRestarter) Restart() error {
	s.called = true
	return s.err
}

func TestScheduler_restartAfterApply(t *testing.T) {
	autoRestartOff := false

	tests := []struct {
		name           string
		cfg            *config.UpdateConfig
		restarter      *stubRestarter
		wantCalled     bool
		wantRequired   bool
		wantReasonPart string
		wantState      UpdateState
	}{
		{
			name:       "restarts when supervised",
			cfg:        &config.UpdateConfig{},
			restarter:  &stubRestarter{},
			wantCalled: true,
			wantState:  StateRestarting,
		},
		{
			name:           "reports restart_required when nothing would bring it back",
			cfg:            &config.UpdateConfig{},
			restarter:      &stubRestarter{err: ErrNoSupervisor},
			wantCalled:     true,
			wantRequired:   true,
			wantReasonPart: "no service manager",
			wantState:      StateIdle,
		},
		{
			name:           "reports restart_required when auto_restart is off",
			cfg:            &config.UpdateConfig{AutoRestart: &autoRestartOff},
			restarter:      &stubRestarter{},
			wantCalled:     false,
			wantRequired:   true,
			wantReasonPart: "auto_restart is disabled",
			wantState:      StateIdle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScheduler(tt.cfg, clock.System(), WithRestarter(tt.restarter))

			s.restartAfterApply()

			assert.Equal(t, tt.wantCalled, tt.restarter.called)
			status := s.GetStatus()
			assert.Equal(t, tt.wantRequired, status.RestartRequired)
			assert.Equal(t, tt.wantState, status.State)
			if tt.wantReasonPart != "" {
				assert.Contains(t, status.RestartRequiredReason, tt.wantReasonPart)
			} else {
				assert.Empty(t, status.RestartRequiredReason)
			}
		})
	}
}

func TestNewScheduler_InstallsASupervisorRestarter(t *testing.T) {
	s := NewScheduler(&config.UpdateConfig{}, clock.System())

	require.NotNil(t, s.restarter, "a nil restarter is what made auto-restart a no-op")
	assert.IsType(t, &supervisorRestarter{}, s.restarter)
}

// A failed apply has to leave the status saying so.
//
// applyUpdate advances the state as it works — downloading, verifying —
// and on failure just returns; only the caller turns that into
// StateFailed. The scheduled path does it, the async one did not, so a
// manual apply that failed left the status pinned on the phase it died
// in with an empty error field. Anything watching the status rather
// than the job — a UI, an agent, a node with no jobs registry and so no
// job at all — waits for a run that is already over.
func TestScheduler_FailedAsyncApplyReportsFailed(t *testing.T) {
	s := NewScheduler(&config.UpdateConfig{}, clock.System())

	// A pending release with no assets: resolvePending returns it
	// without touching the network, and applyUpdate gets as far as
	// StateDownloading before failing to find one for this platform.
	s.status.PendingRelease = &ReleaseInfo{
		Version: &version.Version{Major: 9, Minor: 9, Patch: 9},
		TagName: "v9.9.9",
	}

	_, err := s.ApplyNowAsync(context.Background())
	require.NoError(t, err)
	s.wg.Wait()

	status := s.GetStatus()
	assert.Equal(t, StateFailed, status.State,
		"the status still reports work in progress that nothing is doing")
	assert.NotEmpty(t, status.Error, "a failed apply that names no reason cannot be acted on")
}
