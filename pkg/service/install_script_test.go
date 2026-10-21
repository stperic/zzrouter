//go:build !windows

package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/require"
)

func TestInstallScriptUninstallRemovesStopSettings(t *testing.T) {
	for _, operator := range []bool{false, true} {
		name := "empty directory"
		if operator {
			name = "operator drop-in survives"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			unit := filepath.Join(root, "etc/systemd/system/zzrouter-node.service")
			dropDir := unit + ".d"
			require.NoError(t, os.MkdirAll(dropDir, 0700))
			require.NoError(t, os.WriteFile(unit, nil, 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dropDir, "stop.conf"), nil, 0600))
			if operator {
				require.NoError(t, os.WriteFile(filepath.Join(dropDir, "operator.conf"), []byte("operator settings"), 0600))
			}
			script, err := filepath.Abs("../../scripts/install.sh")
			require.NoError(t, err)
			const rig = `source "$1"
rig_root="$2"
OS=linux
INSTALL_DIR="$rig_root/bin"
SERVICE_INSTALL_DIR="$rig_root/local/bin"
MANAGED_BIN_DIR="$rig_root/managed/bin"
MANAGED_VERSIONS_DIR="$rig_root/managed/versions"
PROFILE_SNIPPET="$rig_root/profile"
id() { [ "$1" = -u ] && echo 0; }
pkill() { return 0; }
systemctl() {
    printf '%s\n' "$*" >> "$rig_root/calls"
    if [ "$1" = list-unit-files ]; then echo 'zzrouter-node.service enabled'; fi
    if [ "$1" = daemon-reload ] && [ -e "$rig_root/etc/systemd/system/zzrouter-node.service.d/stop.conf" ]; then
        echo stale-dropin >> "$rig_root/calls"
    fi
    return 0
}
rm() { /bin/rm "$1" "$rig_root$2"; }
rmdir() { /bin/rmdir "$rig_root$1"; }
uninstall <<< n
`
			output, err := host.Command("bash", "-c", rig, "uninstall-test", script, root).CombinedOutput()
			require.NoError(t, err, "%s", output)
			for _, path := range []string{unit, filepath.Join(dropDir, "stop.conf")} {
				_, err = os.Stat(path)
				require.True(t, os.IsNotExist(err), "%s must be removed", path)
			}
			if operator {
				content, readErr := os.ReadFile(filepath.Join(dropDir, "operator.conf"))
				require.NoError(t, readErr)
				require.Equal(t, "operator settings", string(content))
			} else {
				_, err = os.Stat(dropDir)
				require.True(t, os.IsNotExist(err), "empty drop-in directory must be removed")
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			require.NoError(t, err)
			require.Contains(t, string(calls), "stop zzrouter-node\ndisable zzrouter-node\ndaemon-reload\n")
			require.NotContains(t, string(calls), "stale-dropin", "drop-in must be removed before daemon-reload")
		})
	}
}
