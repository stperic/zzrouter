//go:build linux

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stretchr/testify/require"
)

func systemdStopFixture(t *testing.T) (*SystemdManager, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TEST_SYSTEMCTL_DIR", dir)
	script := "#!/bin/sh\necho \"$*\" >> \"$TEST_SYSTEMCTL_DIR/calls\"\nif [ \"$1\" = daemon-reload ] && [ -e \"$TEST_SYSTEMCTL_DIR/fail\" ]; then exit 1; fi\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &SystemdManager{unitPath: filepath.Join(dir, "zzrouter-node.service")}, dir
}

func TestSystemdStopSettingsSingleDefinition(t *testing.T) {
	m, _ := systemdStopFixture(t)
	require.NoError(t, m.InstallService())
	unit, err := os.ReadFile(m.servicePath())
	require.NoError(t, err)
	drop, err := os.ReadFile(m.servicePath() + ".d/stop.conf")
	require.NoError(t, err)
	require.NotContains(t, string(unit), "KillMode=")
	require.NotContains(t, string(unit), "TimeoutStopSec=")
	require.Contains(t, string(drop), "[Service]\nKillMode=mixed\n")
	require.NotContains(t, string(unit), "{{")
	require.Equal(t, 1, strings.Count(string(drop), "TimeoutStopSec="))
	require.Contains(t, string(unit), "Restart=on-failure\n")
	_, value, found := strings.Cut(string(drop), "TimeoutStopSec=")
	require.True(t, found)
	value, _, _ = strings.Cut(value, "\n")
	timeout, err := time.ParseDuration(value)
	require.NoError(t, err)
	require.Equal(t, constants.SupervisorStopTimeout, timeout)
	require.Greater(t, timeout, constants.ShutdownMaxTimeout)
}

func TestSystemdStopSettingsAtomicIdempotentAndReloadRetry(t *testing.T) {
	m, dir := systemdStopFixture(t)
	path := m.servicePath() + ".d/stop.conf"
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	victim := filepath.Join(dir, "victim")
	require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0600))
	require.NoError(t, os.Symlink(victim, path))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fail"), nil, 0600))
	require.Error(t, EnsureStopSettings(m))
	content, err := os.ReadFile(victim)
	require.NoError(t, err)
	require.Equal(t, "untouched", string(content), "atomic rename must replace the link")
	first, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, "fail")))
	require.NoError(t, EnsureStopSettings(m))
	second, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(first, second), "unchanged settings must not be rewritten")
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	require.NoError(t, err)
	require.Equal(t, "daemon-reload\ndaemon-reload\n", string(calls), "a failed reload must be retried")
}

func TestSystemdUninstallRemovesStopSettings(t *testing.T) {
	m, dir := systemdStopFixture(t)
	require.NoError(t, m.InstallService())
	other := m.servicePath() + ".d/operator.conf"
	require.NoError(t, os.WriteFile(other, []byte("[Service]\n"), 0600))
	require.NoError(t, m.UninstallService())
	_, err := os.Stat(m.servicePath() + ".d/stop.conf")
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(other)
	require.NoError(t, err)
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(string(calls), "stop zzrouter-node.service\ndisable zzrouter-node.service\ndaemon-reload\n"))
}

func TestSystemdUninstallRemovesEmptyDropInDirectory(t *testing.T) {
	m, _ := systemdStopFixture(t)
	require.NoError(t, m.InstallService())
	require.NoError(t, m.UninstallService())
	_, err := os.Stat(m.servicePath() + ".d")
	require.True(t, os.IsNotExist(err))
}
