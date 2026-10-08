package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	processinfo "github.com/shirou/gopsutil/v4/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestWindowsExecutablePathAliases(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	long, err := filepath.EvalSymlinks(self)
	require.NoError(t, err)
	name, err := windows.UTF16PtrFromString(long)
	require.NoError(t, err)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(name, &buf[0], uint32(len(buf)))
	require.NoError(t, err)
	require.Less(t, n, uint32(len(buf)))
	short := windows.UTF16ToString(buf[:n])
	proc, err := processinfo.NewProcess(int32(os.Getpid()))
	require.NoError(t, err)
	exe, err := proc.Exe()
	require.NoError(t, err)
	t.Logf("PID=%d self=%q processExe=%q long=%q short=%q", os.Getpid(), self, exe, long, short)

	for _, alias := range []string{self, long, short, `\\?\` + long, strings.ToUpper(long)} {
		t.Run(alias, func(t *testing.T) {
			assert.True(t, sameExecutablePath(canonicalExecutablePath(exe), canonicalExecutablePath(alias)))
			assert.Equal(t, PresenceRunning, BinaryPresence(alias))
			pids, err := DaemonPIDs(t.Context(), alias, os.Args[1:])
			require.NoError(t, err)
			assert.Contains(t, pids, os.Getpid())
		})
	}

	other := filepath.Join(t.TempDir(), "another.exe")
	require.NoError(t, os.WriteFile(other, []byte("different executable"), 0o600))
	assert.False(t, sameExecutablePath(canonicalExecutablePath(exe), canonicalExecutablePath(other)))
}

func TestWindowsExecutableNamespacePrefixes(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{`\\?\C:\missing\provider.exe`, `C:\missing\provider.exe`},
		{`\\?\UNC\missing-server\share\provider.exe`, `\\missing-server\share\provider.exe`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, stripWindowsPathPrefix(tc.path))
		})
	}
}
