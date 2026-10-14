package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

func TestWritableManagedNodeDelegatesButUpdaterDoesNot(t *testing.T) {
	l, installer := newManagedInstall(t, "2.0.0", 2)
	seedVersion(t, l, "1.0.0")
	require.NoError(t, l.saveState(&layoutState{Previous: []string{"1.0.0"}}))
	require.NoError(t, installer.CheckWritable(), "fixture must represent a root-writable install")
	work := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", work)
	clk := clocktest.NewFakeClock(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	node := NewScheduler(&config.UpdateConfig{}, clk, func(s *Scheduler) { s.installer = installer })
	defer node.Stop()
	require.NotNil(t, node.Delegated(), "permission to write must never bypass the updater")
	require.Equal(t, DefaultHandoff().RequestPath(), node.Delegated().RequestPath())
	require.Empty(t, node.GetStatus().Blocked)

	updater := NewSchedulerIn(work, &config.UpdateConfig{}, clk, func(s *Scheduler) { s.installer = installer })
	defer updater.Stop()
	require.Nil(t, updater.Delegated(), "update run must keep direct execution")
	err := updater.RequestPrivileged(ActionRollback, "updater")
	require.Error(t, err)
	result, err := updater.Rollback(t.Context())
	require.NoError(t, err)
	require.True(t, result.Success)
	active, err := l.activeVersion()
	require.NoError(t, err)
	require.Equal(t, "1.0.0", active)
}

func TestRequestPrivilegedPreservesPendingTargetAndMetadata(t *testing.T) {
	_, installer := newManagedInstall(t, "1.0.0", 2)
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	clk := clocktest.NewFakeClock(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	h := NewHandoff(t.TempDir(), t.TempDir())
	s := NewScheduler(&config.UpdateConfig{}, clk, func(s *Scheduler) { s.installer = installer })
	defer s.Stop()
	s.delegate = h
	s.status.PendingRelease = &ReleaseInfo{Version: version.MustParseVersion("2.0.0")}
	for _, action := range []RequestAction{ActionApply, ActionRollback} {
		err := s.RequestPrivileged(action, "api request fixture")
		require.NoError(t, err)
		pending, err := h.Pending()
		require.NoError(t, err)
		require.Equal(t, &Request{Action: action, TargetVersion: "2.0.0", RequestedBy: "api request fixture", RequestedAt: clk.Now().UTC()}, pending)
		require.NoError(t, os.Remove(h.RequestPath()))
	}
}

func TestRequestPrivilegedReturnsSubmissionFailure(t *testing.T) {
	_, installer := newManagedInstall(t, "1.0.0", 2)
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("unchanged"), 0600))
	h := NewHandoff(filepath.Join(blocker, "requests"), t.TempDir())
	s := NewScheduler(&config.UpdateConfig{}, clocktest.NewFakeClock(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)), func(s *Scheduler) { s.installer = installer })
	defer s.Stop()
	s.delegate = h
	err := s.RequestPrivileged(ActionRollback, "cli")
	require.Error(t, err)
	data, err := os.ReadFile(blocker)
	require.NoError(t, err)
	require.Equal(t, "unchanged", string(data))
}
