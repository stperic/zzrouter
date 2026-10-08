package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBinary is a runnable stand-in for a released binary: a script
// that answers --version the way the real one does. Tests use it rather
// than a text file so the install path exercises its own smoke test
// instead of stepping around it.
func fakeBinary(name, ver string) []byte {
	return []byte("#!/bin/sh\necho \"" + name + " version " + ver + "\"\n")
}

// applyFixture is a Scheduler wired end to end against local stand-ins
// for the GitHub API and the release CDN, with its binaries, backups and
// history inside t.TempDir(). Everything from "check" to "restart" runs
// for real; only the two servers and the process exit are substituted.
type applyFixture struct {
	scheduler *Scheduler
	restarter *stubRestarter

	nodePath     string
	launcherPath string
}

// newApplyFixture publishes one release, v99.99.99, carrying the archive
// members a real release carries.
func newApplyFixture(t *testing.T, archiveMembers map[string][]byte) *applyFixture {
	t.Helper()
	return newApplyFixtureWithChecksum(t, archiveMembers, "")
}

// newApplyFixtureWithChecksum publishes a release whose checksums.txt
// claims publishedChecksum for the archive. Empty means publish the
// archive's real hash.
func newApplyFixtureWithChecksum(t *testing.T, archiveMembers map[string][]byte, publishedChecksum string) *applyFixture {
	t.Helper()

	tmpDir := t.TempDir()
	installDir := filepath.Join(tmpDir, "bin")
	require.NoError(t, os.MkdirAll(installDir, 0750))

	nodePath := filepath.Join(installDir, "zzrouter-node")
	launcherPath := filepath.Join(installDir, "zzrouter-launcher")
	require.NoError(t, os.WriteFile(nodePath, fakeBinary("zzrouter-node", "0.0.1"), 0750))
	require.NoError(t, os.WriteFile(launcherPath, fakeBinary("zzrouter-launcher", "0.0.1"), 0750))

	archive := createTarGzContentForIntegration(t, archiveMembers)
	assetName := fmt.Sprintf("zzrouter-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if publishedChecksum == "" {
		publishedChecksum = computeSHA256ForIntegration(archive)
	}
	checksums := fmt.Sprintf("%s  %s\n", publishedChecksum, assetName)
	bundle, material := signedUpdateChecksums(t, []byte(checksums), "99.99.99")

	// Release CDN. TLS because the downloader refuses plain HTTP, and
	// registered before the API server so its URL can go into the asset
	// list.
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + assetName:
			_, _ = w.Write(archive)
		case "/checksums.txt.sigstore.json":
			_, _ = w.Write(bundle)
		case "/checksums.txt":
			_, _ = w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cdn.Close)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/test/test/releases" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]githubRelease{{
			TagName:     "v99.99.99",
			Name:        "Test Release",
			PublishedAt: time.Now().Format(time.RFC3339),
			Assets: []githubAsset{
				{Name: assetName, Size: int64(len(archive)), BrowserDownloadURL: cdn.URL + "/" + assetName},
				{Name: "checksums.txt", Size: int64(len(checksums)), BrowserDownloadURL: cdn.URL + "/checksums.txt"},
				{Name: "checksums.txt.sigstore.json", Size: int64(len(bundle)), BrowserDownloadURL: cdn.URL + "/checksums.txt.sigstore.json"},
			},
		}})
	}))
	t.Cleanup(api.Close)

	// The allowlist matches on host:port, not hostname.
	cdnURL, err := url.Parse(cdn.URL)
	require.NoError(t, err)
	cdnHost := cdnURL.Host

	restarter := &stubRestarter{}
	s := NewScheduler(&config.UpdateConfig{Channel: "stable"}, clock.System(), WithRestarter(restarter))

	// Keep every side effect inside the temp tree: NewScheduler points
	// these at the real XDG config dir.
	s.tempDir = filepath.Join(tmpDir, "update-temp")
	s.backupDir = filepath.Join(tmpDir, "backups")
	s.history = NewHistory(filepath.Join(tmpDir, "update-history.json"))
	require.NoError(t, s.EnsureDirectories())
	s.confirmer = &Confirmer{
		path:      filepath.Join(tmpDir, pendingConfirmFileName),
		installer: NewInstaller(s.backupDir, 2),
	}

	s.checker.owner, s.checker.repo = "test", "test"
	s.checker.SetAPIBaseURL(api.URL)

	s.verifier.trustedMaterial = material
	s.downloader = NewDownloader(s.tempDir)
	s.downloader.SetAllowedHosts([]string{cdnHost})
	s.downloader.httpClient = cdn.Client()

	s.installer = NewInstaller(s.backupDir, 2)
	s.installer.SetCurrentExecutable(nodePath)

	return &applyFixture{
		scheduler:    s,
		restarter:    restarter,
		nodePath:     nodePath,
		launcherPath: launcherPath,
	}
}

func (f *applyFixture) read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // path is inside the test's own temp tree
	require.NoError(t, err)
	return string(content)
}

// TestApply_EndToEnd walks the whole chain — check, download, verify,
// install, restart — against a release that ships the node and its
// launcher, and asserts the outcome that actually matters: the process
// was asked to exit so the new binary is the one that runs.
func TestApply_EndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	f := newApplyFixture(t, map[string][]byte{
		"zzrouter-linux-amd64":          fakeBinary("zzrouter", "99.99.99"),
		"zzrouter-launcher-linux-amd64": fakeBinary("zzrouter-launcher", "99.99.99"),
		"zzrouter-node-linux-amd64":     fakeBinary("zzrouter-node", "99.99.99"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	check, err := f.scheduler.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, check.UpdateAvailable, "skipped: %s", check.SkippedReason)
	require.Equal(t, 99, check.LatestRelease.Version.Major)

	require.NoError(t, f.scheduler.ApplyNow(ctx))

	assert.Equal(t, string(fakeBinary("zzrouter-node", "99.99.99")), f.read(t, f.nodePath))
	assert.Equal(t, string(fakeBinary("zzrouter-launcher", "99.99.99")), f.read(t, f.launcherPath),
		"the launcher has to move with the node or its embedded hash no longer matches")

	assert.True(t, f.restarter.called, "an applied update that never restarts leaves the old code running")
	status := f.scheduler.GetStatus()
	assert.False(t, status.RestartRequired)
	assert.Equal(t, StateRestarting, status.State)

	// The next boot has to know it is on trial, and what to go back to.
	pending, err := f.scheduler.confirmer.ClaimBoot()
	require.NoError(t, err)
	require.NotNil(t, pending, "an update that restarts must leave a record for the boot that follows")
	assert.Equal(t, "99.99.99", pending.ToVersion)
	assert.NotEmpty(t, pending.BackupPath)
	assert.NotEmpty(t, pending.LauncherBackupPath, "a rollback has to put the launcher back too")

	// The apply is on the record with both versions.
	history, err := f.scheduler.GetHistory()
	require.NoError(t, err)
	require.Len(t, history.Entries, 1)
	assert.True(t, history.Entries[0].Success)
	assert.Equal(t, "99.99.99", history.Entries[0].ToVersion.String())
}

// TestApply_EndToEnd_UnsupervisedNodeReportsRestartRequired: the update
// still lands, but a node nothing would restart has to say so rather
// than report a version it is not running.
func TestApply_EndToEnd_UnsupervisedNodeReportsRestartRequired(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	f := newApplyFixture(t, map[string][]byte{
		"zzrouter-node-linux-amd64": fakeBinary("zzrouter-node", "99.99.99"),
	})
	f.restarter.err = ErrNoSupervisor

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	require.NoError(t, f.scheduler.ApplyNow(ctx))

	assert.Equal(t, string(fakeBinary("zzrouter-node", "99.99.99")), f.read(t, f.nodePath))

	status := f.scheduler.GetStatus()
	assert.True(t, status.RestartRequired)
	assert.Contains(t, status.RestartRequiredReason, "no service manager")
	assert.Equal(t, StateIdle, status.State)
}

// TestApply_EndToEnd_RollbackRestoresBothBinaries closes the loop: the
// pair that went in together comes back out together.
func TestApply_EndToEnd_RollbackRestoresBothBinaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	f := newApplyFixture(t, map[string][]byte{
		"zzrouter-launcher-linux-amd64": fakeBinary("zzrouter-launcher", "99.99.99"),
		"zzrouter-node-linux-amd64":     fakeBinary("zzrouter-node", "99.99.99"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, f.scheduler.ApplyNow(ctx))

	result, err := f.scheduler.Rollback(ctx)
	require.NoError(t, err)
	require.True(t, result.Success)

	assert.Equal(t, string(fakeBinary("zzrouter-node", "0.0.1")), f.read(t, f.nodePath))
	assert.Equal(t, string(fakeBinary("zzrouter-launcher", "0.0.1")), f.read(t, f.launcherPath))

	// The rolled-away version must stop being watched, or the next boot
	// counts an attempt against it and eventually "rolls back" to it.
	pending, err := f.scheduler.confirmer.ClaimBoot()
	require.NoError(t, err)
	assert.Nil(t, pending)
}

// TestApply_EndToEnd_ChecksumMismatchInstallsNothing: verification is
// the gate in front of an irreversible binary swap. A release whose
// published checksum does not match what the CDN served must leave both
// binaries alone and must not restart the node onto them.
func TestApply_EndToEnd_ChecksumMismatchInstallsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	f := newApplyFixtureWithChecksum(t,
		map[string][]byte{
			"zzrouter-launcher-linux-amd64": fakeBinary("zzrouter-launcher", "99.99.99"),
			"zzrouter-node-linux-amd64":     fakeBinary("zzrouter-node", "99.99.99"),
		},
		"0000000000000000000000000000000000000000000000000000000000000000",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := f.scheduler.ApplyNow(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verification failed")

	assert.Equal(t, string(fakeBinary("zzrouter-node", "0.0.1")), f.read(t, f.nodePath))
	assert.Equal(t, string(fakeBinary("zzrouter-launcher", "0.0.1")), f.read(t, f.launcherPath))
	assert.False(t, f.restarter.called, "a node must not restart onto a binary that failed verification")
}

func TestApply_EndToEnd_MissingBundleInstallsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix test binaries")
	}
	f := newApplyFixture(t, map[string][]byte{
		"zzrouter-node-linux-amd64": fakeBinary("zzrouter-node", "99.99.99"),
	})
	ctx := context.Background()
	check, err := f.scheduler.CheckNow(ctx)
	require.NoError(t, err)
	assets := check.LatestRelease.Assets
	check.LatestRelease.Assets = nil
	for _, asset := range assets {
		if asset.Name != "checksums.txt.sigstore.json" {
			check.LatestRelease.Assets = append(check.LatestRelease.Assets, asset)
		}
	}
	err = f.scheduler.ApplyNow(ctx)
	require.ErrorContains(t, err, "requires checksums.txt.sigstore.json")
	assert.Equal(t, string(fakeBinary("zzrouter-node", "0.0.1")), f.read(t, f.nodePath))
	assert.Equal(t, string(fakeBinary("zzrouter-launcher", "0.0.1")), f.read(t, f.launcherPath))
	assert.False(t, f.restarter.called)
	history, err := f.scheduler.GetHistory()
	require.NoError(t, err)
	require.Len(t, history.Entries, 1)
	assert.False(t, history.Entries[0].Success)
}

func TestSchedulerSourceCannotChangePublisherTrust(t *testing.T) {
	s := NewScheduler(&config.UpdateConfig{Source: &config.UpdateSourceConfig{Owner: "attacker", Repo: "router"}}, clock.System())
	assert.Equal(t, "https://github.com/stperic/zzrouter/.github/workflows/release.yaml", s.verifier.workflow)
	assert.Equal(t, "https://token.actions.githubusercontent.com", s.verifier.issuer)
}
