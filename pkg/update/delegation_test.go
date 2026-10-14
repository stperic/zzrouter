package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

func TestDelegatedHistoryReadsPrivilegedApplyRollbackReapply(t *testing.T) {
	requireSymlinks(t)
	serviceDir := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", serviceDir)
	// Inject only release HTTP transport: signatures, downloads and managed swaps run for real.
	updater, assets := releaseManifestFixture(t, "99.99.99", rolloutManifest("99.99.99"))
	name := fmt.Sprintf("zzrouter-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	members := make(map[string][]byte)
	for _, binary := range managedBinaryNames {
		members[fmt.Sprintf("%s-%s-%s", binary, runtime.GOOS, runtime.GOARCH)] = fakeBinary(binary, "99.99.99")
	}
	archive := createTarGzContentForIntegration(t, members)
	assets[name] = archive
	assets["checksums.txt"] = []byte(fmt.Sprintf("%s  %s\n", computeSHA256ForIntegration(archive), name))
	bundle, material := signedUpdateChecksums(t, assets["checksums.txt"], "99.99.99")
	assets["checksums.txt.sigstore.json"] = bundle
	updater.verifier.trustedMaterial = material
	updater.restarter = &stubRestarter{}
	l, installer := newManagedInstall(t, "0.0.1", 2)
	updater.installer = installer
	node := NewScheduler(&config.UpdateConfig{}, clock.System(), func(s *Scheduler) { s.installer = installer })
	t.Cleanup(node.Stop)
	require.NotNil(t, node.Delegated())
	node.delegate = NewHandoff(filepath.Join(serviceDir, "requests"), filepath.Dir(updater.history.filePath))
	// A service-writable record must never replace the privileged record.
	require.NoError(t, node.history.Add(UpdateHistoryEntry{ID: "unprivileged"}))
	serviceBefore, err := os.ReadFile(node.history.filePath)
	require.NoError(t, err)
	for count := 1; count <= 2; count++ {
		require.NoError(t, updater.ApplyVersion(t.Context(), "99.99.99"))
		active, err := l.activeVersion()
		require.NoError(t, err)
		require.Equal(t, "99.99.99", active)
		before, err := os.ReadFile(updater.history.filePath)
		require.NoError(t, err)
		var recorded UpdateHistory
		require.NoError(t, json.Unmarshal(before, &recorded))
		history, err := node.GetHistory()
		require.NoError(t, err)
		require.Len(t, history.Entries, count)
		require.Equal(t, recorded, *history)
		require.True(t, history.Entries[0].Success)
		require.Equal(t, "99.99.99", history.Entries[0].ToVersion.String())
		after, err := os.ReadFile(updater.history.filePath)
		require.NoError(t, err)
		require.Equal(t, before, after, "delegated history reads must never rewrite root storage")
		if count == 1 {
			result, err := updater.Rollback(t.Context())
			require.NoError(t, err)
			require.True(t, result.Success)
			active, err = l.activeVersion()
			require.NoError(t, err)
			require.Equal(t, "0.0.1", active)
		}
	}
	serviceAfter, err := os.ReadFile(node.history.filePath)
	require.NoError(t, err)
	require.Equal(t, serviceBefore, serviceAfter, "privileged updates must never write service storage")
}

func TestDelegatedHistoryDoesNotFallbackOnReadFailure(t *testing.T) {
	serviceDir := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", serviceDir)
	s := NewScheduler(&config.UpdateConfig{}, clock.System())
	t.Cleanup(s.Stop)
	s.delegate = NewHandoff(serviceDir, t.TempDir())
	require.NoError(t, s.history.Add(UpdateHistoryEntry{ID: "unprivileged"}))
	path := filepath.Join(s.delegate.statusDir, "update-history.json")
	history, err := s.GetHistory()
	require.NoError(t, err)
	require.Empty(t, history.Entries, "an unused privileged history must not consult service storage")
	require.NoError(t, os.WriteFile(path, []byte("malformed"), 0600))
	_, err = s.GetHistory()
	require.Error(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0700))
	_, err = s.GetHistory()
	require.Error(t, err, "OS read failures must be visible, not replaced with an empty history")
}

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
