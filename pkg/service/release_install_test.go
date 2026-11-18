//go:build !windows

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/require"
)

func TestReleaseStageIncludesCompleteInstallableBundle(t *testing.T) {
	stageScript, err := filepath.Abs("../../scripts/stage-release.sh")
	require.NoError(t, err)
	for _, platform := range []string{"darwin-arm64", "linux-amd64", "windows-amd64"} {
		t.Run(platform, func(t *testing.T) {
			root := t.TempDir()
			extension := ""
			if platform == "windows-amd64" {
				extension = ".exe"
			}
			for _, binary := range []string{"zzrouter", "zzrouter-node", "zzrouter-launcher"} {
				require.NoError(t, os.WriteFile(filepath.Join(root, binary+"-"+platform+extension), []byte(binary), 0o700))
			}
			notices := filepath.Join(root, "notices")
			require.NoError(t, os.WriteFile(notices, []byte("test license"), 0o600))
			output := filepath.Join(root, "stage")
			result, err := host.Command("bash", stageScript, platform, root, output, notices).CombinedOutput()
			require.NoError(t, err, "%s", result)
			for _, binary := range []string{"zzrouter", "zzrouter-node", "zzrouter-launcher"} {
				data, err := os.ReadFile(filepath.Join(output, binary+extension))
				require.NoError(t, err)
				require.Equal(t, binary, string(data))
			}
			installer := "install.sh"
			if platform == "windows-amd64" {
				installer = "install.ps1"
			}
			for _, name := range []string{installer, "LICENSE", "NOTICE", "THIRD_PARTY_NOTICES"} {
				_, err := os.Stat(filepath.Join(output, name))
				require.NoError(t, err)
			}
			// The local Makefile stages in-place; this must not fail by copying a binary onto itself.
			result, err = host.Command("bash", stageScript, platform, output, output, notices).CombinedOutput()
			require.NoError(t, err, "%s", result)
		})
	}
}

func TestReleaseStageRefusesMissingLauncherBeforeWriting(t *testing.T) {
	root := t.TempDir()
	for _, binary := range []string{"zzrouter", "zzrouter-node"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, binary), []byte(binary), 0o700))
	}
	script, err := filepath.Abs("../../scripts/stage-release.sh")
	require.NoError(t, err)
	output := filepath.Join(root, "output")
	_, err = host.Command("bash", script, "linux-amd64", root, output, "unused").CombinedOutput()
	require.Error(t, err)
	_, err = os.Stat(output)
	require.True(t, os.IsNotExist(err))
}

func TestInstallerFindsAdjacentBinariesFromAnotherDirectory(t *testing.T) {
	root := t.TempDir()
	for _, binary := range []string{"zzrouter", "zzrouter-node", "zzrouter-launcher"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, binary), []byte(binary), 0o700))
	}
	script, err := filepath.Abs("../../scripts/install.sh")
	require.NoError(t, err)
	rig := `source "$1"
SCRIPT_DIR="$2"
PROJECT_ROOT="$2/missing"
PLATFORM=linux-amd64
find_binaries
validate_binary_set
test "$BINARY_SRC" = "$2"
`
	output, err := host.Command("bash", "-c", rig, "installer-test", script, root).CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.NoError(t, os.Remove(filepath.Join(root, "zzrouter-launcher")))
	_, err = host.Command("bash", "-c", rig, "installer-test", script, root).CombinedOutput()
	require.Error(t, err)
}

func TestInstallerPreservesExistingNodeConfiguration(t *testing.T) {
	root := t.TempDir()
	const original = "node:\n  bind: 192.0.2.5\n  port: 9190\ncluster:\n  mode: worker\n  bind_port: 9191\ncustom: retained\n"
	config := filepath.Join(root, "node.yaml")
	require.NoError(t, os.WriteFile(config, []byte(original), 0o600))
	const identity = "#!/bin/sh\nprintf '%s\\n' '{\"mode\":\"worker\",\"bind\":\"192.0.2.5\",\"port\":9190,\"cluster_port\":9191}'\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "zzrouter-node"), []byte(identity), 0o700))
	script, err := filepath.Abs("../../scripts/install.sh")
	require.NoError(t, err)
	rig := `source "$1"
INSTALL_DIR="$2"
write_node_yaml "$2" 9090
test "$RESOLVED_PORT" = 9190
test "$ROLE" = worker
test "$BIND" = 192.0.2.5
`
	output, err := host.Command("bash", "-c", rig, "installer-test", script, root).CombinedOutput()
	require.NoError(t, err, "%s", output)
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	require.Equal(t, original, string(data))
}

func TestInstallerRefusesUnsafeIdentityBeforeStopping(t *testing.T) {
	script, err := filepath.Abs("../../scripts/install.sh")
	require.NoError(t, err)
	for _, identity := range []string{
		`{"mode":"coordinator","bind":"127.0.0.1","port":9090,"cluster_port":9091,"secure":true}`,
		`{"mode":"coordinator","bind":"127.0.0.1"}`,
	} {
		t.Run(identity, func(t *testing.T) {
			root := t.TempDir()
			configDir := filepath.Join(root, "preserved config")
			require.NoError(t, os.Mkdir(configDir, 0o700))
			configFile := filepath.Join(configDir, "node.yaml")
			require.NoError(t, os.WriteFile(configFile, []byte("unchanged"), 0o600))
			for _, name := range []string{"zzrouter", "zzrouter-node", "zzrouter-launcher"} {
				body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%s'\n", identity)
				require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0o700))
			}
			rig := `source "$1"
ZZROUTER_CONFIG_DIR="$2/preserved config"
SCRIPT_DIR="$2"
PROJECT_ROOT="$2/missing"
MUTATION_MARKER="$2/mutated"
detect_platform() { OS=linux; PLATFORM=linux-amd64; }
id() { printf '1000\n'; }
stop_running_node() { touch "$MUTATION_MARKER"; return 1; }
install_binaries() { touch "$MUTATION_MARKER"; return 1; }
main --no-service --yes
`
			output, err := host.Command("bash", "-c", rig, "installer-test", script, root).CombinedOutput()
			require.Error(t, err, "%s", output)
			_, err = os.Stat(filepath.Join(root, "mutated"))
			require.ErrorIs(t, err, os.ErrNotExist)
			data, err := os.ReadFile(configFile)
			require.NoError(t, err)
			require.Equal(t, "unchanged", string(data))
		})
	}
}

func TestInstallerActivatesExistingManagedServiceBeforeStart(t *testing.T) {
	script, err := filepath.Abs("../../scripts/install.sh")
	require.NoError(t, err)
	for _, mode := range []string{"existing", "new", "activation-failed", "darwin"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			binary := `#!/bin/sh
printf '%s\n' "$*" >> "$(dirname "$0")/calls"
if [ "$1" = update ] && [ -f "$(dirname "$0")/fail" ]; then exit 1; fi
`
			require.NoError(t, os.WriteFile(filepath.Join(root, "zzrouter-node"), []byte(binary), 0700))
			if mode == "activation-failed" {
				require.NoError(t, os.WriteFile(filepath.Join(root, "fail"), nil, 0600))
			}
			rig := `source "$1"
INSTALL_DIR="$2"
OS=linux
mode="$3"
[ "$mode" != darwin ] || OS=darwin
systemctl() { [ "$1" = cat ] && [ "$2" = zzrouter-node.service ] && [ "$mode" != new ]; }
start_system_service "$2/config"
`
			output, err := host.Command("bash", "-c", rig, "installer-test", script, root, mode).CombinedOutput()
			calls, readErr := os.ReadFile(filepath.Join(root, "calls"))
			require.NoError(t, readErr)
			if mode == "activation-failed" {
				require.Error(t, err)
				require.Equal(t, "update enable --restart=false\n", string(calls), "do not start the old tree after activation fails")
				return
			}
			require.NoError(t, err, "%s", output)
			expected := "start --dir " + root + "/config\n"
			if mode == "existing" {
				expected = "update enable --restart=false\n" + expected
			}
			require.Equal(t, expected, string(calls))
		})
	}
}

func TestInstallerRejectsHealthyOldVersion(t *testing.T) {
	script, err := filepath.Abs("../../scripts/install.sh")
	require.NoError(t, err)
	for _, running := range []string{"1.2.3+new", "1.2.3+old", ""} {
		t.Run(running, func(t *testing.T) {
			rig := `source "$1"
ROLE=worker
VERSION=1.2.3+new
running="$2"
have_http_client() { return 0; }
http_get() { printf '{"status":"healthy","version":"%s","details":{"node_identity":{"config_path":"/test/node.yaml","cluster_mode":"worker"}}}\n' "$running"; }
verify_identity 9090 /test/node.yaml
`
			output, err := host.Command("bash", "-c", rig, "installer-test", script, running).CombinedOutput()
			if running == "1.2.3+new" {
				require.NoError(t, err, "%s", output)
			} else {
				require.Error(t, err)
				require.Contains(t, string(output), "does not match the installed bundle")
			}
		})
	}
}

func TestInstallerDelegatesCredentialCreation(t *testing.T) {
	root := t.TempDir()
	node := filepath.Join(root, "zzrouter-node")
	require.NoError(t, os.WriteFile(node, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.args\"\n"), 0o700))
	configDir := filepath.Join(root, "config with spaces")
	require.NoError(t, os.Mkdir(configDir, 0o700))
	legacy := filepath.Join(configDir, "zzrouter.env")
	require.NoError(t, os.WriteFile(legacy, []byte("SAMPLE=fixture\n"), 0o600))
	script, err := filepath.Abs("../../scripts/install.sh")
	require.NoError(t, err)
	rig := `source "$1"
INSTALL_DIR="$2"
OS=darwin
write_env_file "$3"
`
	output, err := host.Command("bash", "-c", rig, "installer-test", script, root, configDir).CombinedOutput()
	require.NoError(t, err, "%s", output)
	arguments, err := os.ReadFile(node + ".args")
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("config\ncredentials\n--dir\n%s\n--legacy-file\n%s\n", configDir, legacy), string(arguments))
	// The shell supplies paths only. It must not generate a separate key or read
	// credentials to print them while the canonical CLI owns creation/migration.
	_, err = os.Stat(filepath.Join(configDir, ".env"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Empty(t, output)
}

func TestInstallerServiceDropinPreflight(t *testing.T) {
	script, err := filepath.Abs("../../scripts/install.sh")
	require.NoError(t, err)
	for _, field := range []string{"Environment", "EnvironmentFile", "ExecStart", "User"} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "zzrouter-node.service.d")
			require.NoError(t, os.Mkdir(directory, 0o700))
			// The managed stop-only override remains acceptable.
			require.NoError(t, os.WriteFile(filepath.Join(directory, "stop.conf"), []byte("[Service]\nKillMode=mixed\nTimeoutStopSec=60\n"), 0o600))
			rig := `source "$1"; check_service_dropins "$2"`
			output, err := host.Command("bash", "-c", rig, "installer-test", script, root).CombinedOutput()
			require.NoError(t, err, "%s", output)
			custom := filepath.Join(directory, "custom.conf")
			require.NoError(t, os.WriteFile(custom, []byte("[Service]\n"+field+"=private-fixture-value\n"), 0o600))
			output, err = host.Command("bash", "-c", rig, "installer-test", script, root).CombinedOutput()
			require.Error(t, err)
			require.Contains(t, string(output), custom)
			require.NotContains(t, string(output), "private-fixture-value")
		})
	}
}

func TestInstallerRefusesCustomLinuxServiceDirectoryBeforeMutation(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "custom config")
	require.NoError(t, os.Mkdir(configDir, 0o700))
	configFile := filepath.Join(configDir, "node.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte("unchanged"), 0o600))
	script, err := filepath.Abs("../../scripts/install.sh")
	require.NoError(t, err)
	rig := `source "$1"
ZZROUTER_CONFIG_DIR="$2/custom config"
MUTATION_MARKER="$2/mutated"
detect_platform() { OS=linux; PLATFORM=linux-amd64; }
id() { printf '0\n'; }
find_binaries() { touch "$MUTATION_MARKER"; return 1; }
stop_running_node() { touch "$MUTATION_MARKER"; return 1; }
install_binaries() { touch "$MUTATION_MARKER"; return 1; }
main --service --yes
`
	output, err := host.Command("bash", "-c", rig, "installer-test", script, root).CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "Linux service installations require /etc/zzrouter")
	_, err = os.Stat(filepath.Join(root, "mutated"))
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Equal(t, "unchanged", string(data))
}

func TestInstallerOpensClusterFirewallWithoutOpeningLocalConsole(t *testing.T) {
	script, err := filepath.Abs("../../scripts/install.sh")
	require.NoError(t, err)
	root := t.TempDir()
	rig := `source "$1"
BIND=127.0.0.1
CLUSTER_PORT=9191
FIXTURE_FIREWALL="$2"
id() { printf '0\n'; }
ufw() { if [ "$1" = status ]; then printf 'Status: active\n'; else printf '%s\n' "$*" >> "$FIXTURE_FIREWALL"; fi; }
firewall-cmd() { return 1; }
open_firewall_port 9090
open_firewall_port 9191
`
	output, err := host.Command("bash", "-c", rig, "firewall-test", script, filepath.Join(root, "rules")).CombinedOutput()
	require.NoError(t, err, "%s", output)
	rules, err := os.ReadFile(filepath.Join(root, "rules"))
	require.NoError(t, err)
	require.Equal(t, "allow 9191/tcp\n", string(rules))
}
