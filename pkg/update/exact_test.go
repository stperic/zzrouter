package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExactReleaseBypassesChannelAndPagination(t *testing.T) {
	for _, tc := range []struct {
		name, tag string
		draft     bool
		status    int
		ok        bool
	}{
		{"lab", "v1.2.3-lab.1", false, 200, true},
		{"different tag", "v9.9.9", false, 200, false},
		{"draft", "v1.2.3-lab.1", true, 200, false},
		{"missing", "", false, 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "/repos/stperic/zzrouter/releases/tags/v1.2.3-lab.1", r.URL.Path)
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(githubRelease{TagName: tc.tag, Draft: tc.draft, Prerelease: true})
			}))
			defer srv.Close()
			c := NewChecker("stable", "9.x")
			c.SetAPIBaseURL(srv.URL)
			release, err := c.Release(context.Background(), "1.2.3-lab.1")
			if tc.ok {
				require.NoError(t, err)
				require.Equal(t, "1.2.3-lab.1", release.Version.String())
			} else {
				require.Error(t, err)
			}
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestVersionCannotNameLocation(t *testing.T) {
	for _, v := range []string{"", "v1.2.3", " 1.2.3", "https://github.com/release", "../../1.2.3", "1.2.3-lab..1", "1.2.3-01"} {
		require.Error(t, ValidateVersion(v), v)
	}
	for _, v := range []string{"1.2.3", "1.2.3-lab.1", "1.2.3+build.42"} {
		require.NoError(t, ValidateVersion(v), v)
	}
}

func TestSchedulingDisableDuringCheckPreventsApply(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		ext := ".tar.gz"
		if runtime.GOOS == "windows" {
			ext = ".zip"
		}
		_ = json.NewEncoder(w).Encode([]githubRelease{{TagName: "v99.0.0", Assets: []githubAsset{{Name: "zzrouter-" + runtime.GOOS + "-" + runtime.GOARCH + ext}}}})
	}))
	defer srv.Close()
	s := NewSchedulerIn(t.TempDir(), &config.UpdateConfig{}, clock.System())
	defer s.Stop()
	s.checker.SetAPIBaseURL(srv.URL)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.performCheck(context.Background()) }()
	<-entered
	s.SetEnabled(false)
	close(release)
	wg.Wait()
	require.False(t, s.Enabled())
	require.True(t, s.GetStatus().UpdateAvailable, "fixture did not offer a usable release")
	require.Equal(t, StateIdle, s.GetStatus().State)
	require.Empty(t, s.GetStatus().Error, "automatic apply ran after disable")
}

func TestDisabledSchedulingStillAllowsManualCheck(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = w.Write([]byte("[]")) }))
	defer srv.Close()
	disabled := false
	cfg := &config.UpdateConfig{Enabled: &disabled}
	s := NewSchedulerIn(t.TempDir(), cfg, clock.System())
	defer s.Stop()
	s.checker.SetAPIBaseURL(srv.URL)
	s.performCheck(context.Background())
	require.Zero(t, calls.Load())
	_, err := s.CheckNow(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
	s.SetEnabled(true)
	require.False(t, cfg.IsEnabled(), "scheduler mutated caller config")
}

func TestConfirmationReadFailureIsVisible(t *testing.T) {
	s := NewSchedulerIn(t.TempDir(), &config.UpdateConfig{}, clock.System())
	defer s.Stop()
	require.NoError(t, os.WriteFile(s.confirmer.Path(), []byte("{bad json"), 0600))
	require.NotEmpty(t, s.GetStatus().ConfirmationError)
}

func TestHandoffRefusesQueuedAndClaimedConflicts(t *testing.T) {
	h := NewHandoff(t.TempDir(), t.TempDir())
	require.NoError(t, h.Submit(&Request{Action: ActionApply, TargetVersion: "1.2.3-lab.1"}))
	require.ErrorIs(t, h.Submit(&Request{Action: ActionApply, TargetVersion: "9.9.9"}), ErrApplyInFlight)
	req, err := h.Claim()
	require.NoError(t, err)
	require.Equal(t, "1.2.3-lab.1", req.TargetVersion)
	require.ErrorIs(t, h.Submit(&Request{Action: ActionApply, TargetVersion: "9.9.9"}), ErrApplyInFlight)
}

func TestHandoffCanRecoverAnExitedOwner(t *testing.T) {
	h := NewHandoff(t.TempDir(), t.TempDir())
	require.NoError(t, h.Publish(&RunStatus{Action: ActionApply, PID: 1 << 30, State: StateApplying}))
	require.NoError(t, h.Submit(&Request{Action: ActionRollback}))
}

func TestConfirmationWriteFailureRestoresPairBeforeRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses Unix executable scripts")
	}
	f := newApplyFixture(t, map[string][]byte{
		"zzrouter-launcher-linux-amd64": fakeBinary("zzrouter-launcher", "99.99.99"),
		"zzrouter-node-linux-amd64":     fakeBinary("zzrouter-node", "99.99.99"),
	})
	original, err := os.ReadFile(f.nodePath)
	require.NoError(t, err)
	originalLauncher, err := os.ReadFile(f.launcherPath)
	require.NoError(t, err)
	f.scheduler.confirmer.path = f.nodePath + "/not-a-directory"
	err = f.scheduler.ApplyNow(context.Background())
	require.ErrorContains(t, err, "record update confirmation")
	node, err := os.ReadFile(f.nodePath)
	require.NoError(t, err)
	require.Equal(t, original, node)
	launcher, err := os.ReadFile(f.launcherPath)
	require.NoError(t, err)
	require.Equal(t, originalLauncher, launcher)
	require.False(t, f.restarter.called)
}
