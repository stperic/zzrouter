//go:build !windows

package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
