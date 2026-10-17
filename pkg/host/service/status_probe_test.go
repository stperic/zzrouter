//go:build !windows

package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServiceProbeFailureDoesNotConfirmStoppedState(t *testing.T) {
	for _, tc := range []struct {
		command string
		probe   func(context.Context, string) ServiceStatus
	}{
		{"systemctl", (&Systemd{}).StatusContext},
		{"nssm", (&NSSM{}).StatusContext},
		{"pgrep", (&Launchd{detectedLabel: "generic"}).StatusContext},
	} {
		t.Run(tc.command, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, tc.command), []byte("#!/bin/sh\nexit 7\n"), 0700))
			t.Setenv("PATH", dir)
			status := tc.probe(t.Context(), "generic")
			require.Error(t, status.ObservationError)
		})
	}
}

func TestServiceProbeCanPositivelyObserveStoppedState(t *testing.T) {
	for _, tc := range []struct {
		command, script string
		probe           func(context.Context, string) ServiceStatus
	}{
		{"systemctl", "printf 'ActiveState=inactive\\nMainPID=0\\n'", (&Systemd{}).StatusContext},
		{"nssm", "printf 'SERVICE_STOPPED\\n'", (&NSSM{}).StatusContext},
		{"pgrep", "exit 1", (&Launchd{detectedLabel: "generic"}).StatusContext},
	} {
		t.Run(tc.command, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, tc.command), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700))
			t.Setenv("PATH", dir)
			status := tc.probe(t.Context(), "generic")
			require.NoError(t, status.ObservationError)
			require.False(t, status.Running)
		})
	}
}

// An unanswered probe is an error; only a probe that answered may report absence.
func TestServiceDetectSeparatesAbsentFromUnanswered(t *testing.T) {
	for _, tc := range []struct {
		command string
		probe   func(context.Context, string) (bool, error)
	}{
		{"systemctl", (&Systemd{}).DetectContext},
		{"nssm", (&NSSM{}).DetectContext},
		{"launchctl", (&Launchd{}).DetectContext},
	} {
		t.Run(tc.command, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			detected, err := tc.probe(t.Context(), "generic")
			require.NoError(t, err, "a manager that is not installed does not own the service")
			require.False(t, detected)

			command := filepath.Join(dir, tc.command)
			require.NoError(t, os.WriteFile(command, []byte("#!/nonexistent/sh\n"), 0700))
			_, err = tc.probe(t.Context(), "generic")
			require.Error(t, err, "a probe that cannot run cannot tell")

			require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\n[ -e \"$0.hang\" ] && exec /bin/sleep 5\nexit 1\n"), 0700))
			detected, err = tc.probe(t.Context(), "generic")
			require.NoError(t, err)
			require.False(t, detected)

			require.NoError(t, os.WriteFile(command+".hang", nil, 0600))
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			detected, err = tc.probe(ctx, "generic")
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.False(t, detected)
		})
	}
}
