//go:build linux

package service

import (
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/host"
)

//go:embed templates/zzrouter-node.service
var systemdServiceTemplate string

// The privileged updater: a root oneshot, the path unit that lets the
// unprivileged node trigger it, and the timer that runs the scheduled
// check. See templates/zzrouter-update.service for why it is split.

//go:embed templates/zzrouter-update.service
var systemdUpdateServiceTemplate string

//go:embed templates/zzrouter-update.path
var systemdUpdatePathTemplate string

//go:embed templates/zzrouter-update.timer
var systemdUpdateTimerTemplate string

// NewServiceManager returns the Linux systemd service manager
func NewServiceManager() ServiceManager {
	return &SystemdManager{}
}

// SystemdManager handles service management on Linux using systemd
type SystemdManager struct{}

// systemd service file path
const systemdServicePath = "/etc/systemd/system/zzrouter-node.service"

// IsPrivileged returns true if running as root
func (m *SystemdManager) IsPrivileged() bool {
	return os.Getuid() == 0
}

// IsFirstRun returns true if the service setup is incomplete
// Checks for: user, service file, config dir, and data dir
func (m *SystemdManager) IsFirstRun() bool {
	// Check if user exists
	if _, err := user.Lookup(ServiceUser); err != nil {
		return true
	}

	// Check if service file exists
	if _, err := os.Stat(systemdServicePath); os.IsNotExist(err) {
		return true
	}

	// Check if config directory exists
	if _, err := os.Stat(LinuxConfigDir); os.IsNotExist(err) {
		return true
	}

	// Check if data directory exists
	if _, err := os.Stat(LinuxDataDir); os.IsNotExist(err) {
		return true
	}

	return false
}

// IsServiceUser returns true if running as the zzrouter user
func (m *SystemdManager) IsServiceUser() bool {
	currentUser, err := user.Current()
	if err != nil {
		return false
	}
	return currentUser.Username == ServiceUser
}

// CreateServiceUser creates a dedicated zzrouter system user
func (m *SystemdManager) CreateServiceUser() error {
	// Check if user already exists
	if _, err := user.Lookup(ServiceUser); err == nil {
		return nil // User already exists
	}

	// Create system user with no login shell and no home directory
	// -r: system account
	// -s /sbin/nologin: no login shell
	// -M: no home directory
	// -d /var/lib/zzrouter: set home dir for reference (FHS compliant)
	cmd := host.Command("useradd",
		"-r",
		"-s", "/sbin/nologin",
		"-M",
		"-d", LinuxDataDir,
		"-c", "zzRouter Service User",
		ServiceUser,
	)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create service user: %w", err)
	}

	// Add user to GPU access groups (ignore errors if groups don't exist)
	for _, group := range []string{"render", "video"} {
		_ = host.Command("usermod", "-a", "-G", group, ServiceUser).Run() // GPU groups are optional and may not exist.
	}

	return nil
}

// SetupDirectories creates required directories with proper ownership (FHS compliant)
func (m *SystemdManager) SetupDirectories() error {
	// FHS-compliant directory structure:
	// /etc/zzrouter/            - configuration (root-owned, zzrouter readable)
	// /var/lib/zzrouter/        - persistent data (zzrouter-owned)
	// /var/lib/zzrouter/models/ - model storage (zzrouter-owned, can be symlinked to larger storage)
	// /var/log/zzrouter/        - logs (optional, journald preferred)

	// Get zzrouter user info for chown
	u, err := user.Lookup(ServiceUser)
	if err != nil {
		return fmt.Errorf("failed to lookup service user: %w", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	// Create config directory (zzrouter-owned so the service can update config on cluster join/leave)
	if err := os.MkdirAll(LinuxConfigDir, 0750); err != nil {
		return fmt.Errorf("failed to create %s: %w", LinuxConfigDir, err)
	}
	if err := os.Chown(LinuxConfigDir, uid, gid); err != nil {
		return fmt.Errorf("failed to chown %s: %w", LinuxConfigDir, err)
	}

	// Create data directory (zzrouter-owned)
	if err := os.MkdirAll(LinuxDataDir, 0750); err != nil {
		return fmt.Errorf("failed to create %s: %w", LinuxDataDir, err)
	}
	if err := os.Chown(LinuxDataDir, uid, gid); err != nil {
		return fmt.Errorf("failed to chown %s: %w", LinuxDataDir, err)
	}

	// Create models directory (zzrouter-owned)
	// This can be symlinked to larger storage: ln -s /mnt/nvme/models /var/lib/zzrouter/models
	modelsDir := filepath.Join(LinuxDataDir, "models")
	if err := os.MkdirAll(modelsDir, 0750); err != nil {
		return fmt.Errorf("failed to create %s: %w", modelsDir, err)
	}
	if err := os.Chown(modelsDir, uid, gid); err != nil {
		return fmt.Errorf("failed to chown %s: %w", modelsDir, err)
	}

	// Create providers directory (zzrouter-owned)
	// This is where provider installers write venvs, binaries, etc.
	linuxProvidersDir := filepath.Join(config.LinuxRootDir, "providers")
	if err := os.MkdirAll(linuxProvidersDir, 0750); err != nil {
		return fmt.Errorf("failed to create %s: %w", linuxProvidersDir, err)
	}
	if err := os.Chown(linuxProvidersDir, uid, gid); err != nil {
		return fmt.Errorf("failed to chown %s: %w", linuxProvidersDir, err)
	}

	// The managed install tree, left ROOT-OWNED on purpose -- the one
	// directory pair in this function that is not handed to the service
	// user. The node runs unprivileged and must not be able to rewrite
	// the binary it runs, because /usr/local/bin/zzrouter-node links
	// here and an operator's sudo would then execute whatever the
	// service account wrote. Updates go through the privileged updater
	// (templates/zzrouter-update.service) instead.
	//
	// The chmod is not redundant. This process sets umask 0027 at startup
	// (security.SetSecureUmask), so MkdirAll(0755) lands as 0750 and the
	// service user cannot traverse the directory holding the binary it
	// runs -- systemd reports 203/EXEC and restart-loops the unit. Chmod
	// is not umask-masked.
	for _, dir := range []string{
		filepath.Join(config.LinuxRootDir, config.SubdirBin),
		filepath.Join(config.LinuxRootDir, config.SubdirVersions),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0755); err != nil {
			return fmt.Errorf("failed to make %s traversable: %w", dir, err)
		}
	}

	// Where the node leaves update requests, owned by the service user
	// because the node is what writes here. The privileged updater only
	// reads and unlinks. The request holds no binary and no path -- see
	// pkg/update/handoff.go for why that matters.
	handoffDir := filepath.Join(LinuxDataDir, "update")
	if err := os.MkdirAll(handoffDir, 0750); err != nil {
		return fmt.Errorf("failed to create %s: %w", handoffDir, err)
	}
	if err := os.Chown(handoffDir, uid, gid); err != nil {
		return fmt.Errorf("failed to chown %s: %w", handoffDir, err)
	}

	// Where the privileged updater works and publishes: ROOT-OWNED, and
	// deliberately not the directory above. Root must never create a
	// file in a directory the service user controls -- that user can
	// pre-plant the name as a symlink and O_CREATE follows it, which
	// lands root's write wherever the link points. Traversable and
	// readable so the node can read the status it publishes.
	updaterDir := filepath.Join(config.LinuxRootDir, "update")
	if err := os.MkdirAll(updaterDir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", updaterDir, err)
	}
	if err := os.Chmod(updaterDir, 0755); err != nil {
		return fmt.Errorf("failed to make %s readable: %w", updaterDir, err)
	}

	// Create log directory (zzrouter-owned, optional since journald is preferred)
	if err := os.MkdirAll(LinuxLogDir, 0750); err != nil {
		return fmt.Errorf("failed to create %s: %w", LinuxLogDir, err)
	}
	if err := os.Chown(LinuxLogDir, uid, gid); err != nil {
		return fmt.Errorf("failed to chown %s: %w", LinuxLogDir, err)
	}

	// Create default config files if they don't exist (zzrouter-owned, 0640)
	nodeConfigPath := filepath.Join(LinuxConfigDir, "node.yaml")
	if _, err := os.Stat(nodeConfigPath); os.IsNotExist(err) {
		if err := os.WriteFile(nodeConfigPath, []byte(templates.GetNodeTemplate()), 0640); err != nil {
			return fmt.Errorf("failed to create %s: %w", nodeConfigPath, err)
		}
		if err := os.Chown(nodeConfigPath, uid, gid); err != nil {
			return fmt.Errorf("failed to chown %s: %w", nodeConfigPath, err)
		}
	}

	appsDir := filepath.Join(LinuxConfigDir, "providers")
	if info, err := os.Stat(appsDir); err != nil || !info.IsDir() {
		if err := templates.InstallDefaults(appsDir); err != nil {
			return fmt.Errorf("install providers templates: %w", err)
		}
	}

	// Hand the whole config tree to the service user, not just the files
	// created above. An installer or operator that pre-seeds
	// /etc/zzrouter/node.yaml leaves it owned by root, and the service --
	// which runs as ServiceUser -- then cannot read the very config it was
	// installed to run. That surfaces at startup as a config-load error
	// naming a file that plainly exists and looks correct, which is a long
	// way from the cause.
	if err := chownTree(LinuxConfigDir, uid, gid); err != nil {
		return fmt.Errorf("failed to chown %s: %w", LinuxConfigDir, err)
	}

	return nil
}

// chownTree gives every entry under root to uid/gid.
//
// Per-entry failures are ignored on purpose: setup should not abort
// because one file resists chown, and the caller's real requirement is
// that the service user can read its config, which a partial pass still
// satisfies for everything it touched. A failure to walk at all is
// returned, since that means the tree was never visited.
func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(p string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		_ = os.Chown(p, uid, gid)
		return nil
	})
}

// InstallService installs the systemd service unit file
func (m *SystemdManager) InstallService() error {
	// Get the embedded service template
	serviceContent := systemdServiceTemplate

	// Get the current executable path and use it in the service file
	exePath, err := getExecutablePath()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	// Prefer the managed symlink when the relocation ran: it names the
	// running version and survives every update, whereas the path we
	// resolved above is one specific version directory that the next
	// update replaces. ExecStart must point at the stable name or the
	// first update leaves the unit running a version that was pruned.
	managed := isSymlink(defaultManagedNodePath)
	if managed {
		exePath = defaultManagedNodePath
	}

	// Replace the placeholder path with the actual binary location
	serviceContent = strings.ReplaceAll(serviceContent, defaultManagedNodePath, exePath)

	// A node that is NOT in the managed layout still replaces its own
	// binary in place, and under ProtectSystem=strict it cannot unless
	// the directory it runs from is named here. Ownership still applies
	// on top: a root-owned install directory stays unwritable to the
	// service user, which Installer.CheckWritable reports up front.
	//
	// A managed node is the opposite case and deliberately gets nothing
	// added. Its install tree must stay read-only to it -- the
	// privileged updater owns every write there -- so naming the
	// directory would undo the split. Ownership already prevents it;
	// leaving it out of ReadWritePaths means the sandbox does too.
	if !managed {
		serviceContent = strings.Replace(serviceContent,
			"ReadWritePaths=/var/lib/zzrouter",
			"ReadWritePaths="+filepath.Dir(exePath)+" /var/lib/zzrouter", 1)
	}

	// Write service file
	if err := os.WriteFile(systemdServicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %w", err)
	}

	// Reload systemd daemon
	cmd := host.Command("systemctl", "daemon-reload")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}

	// Enable service to start on boot
	cmd = host.Command("systemctl", "enable", "zzrouter-node.service")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to enable service: %w", err)
	}

	if err := m.installUpdaterUnits(exePath); err != nil {
		// Not fatal. Without these the node runs and serves; what it
		// loses is the ability to update itself, which it reports in
		// `update status` rather than discovering during an apply.
		slog.Warn("the node is installed but the privileged updater is not; auto-update will stay blocked", "err", err)
	}

	return nil
}

// updaterUnits are the three units that make up the privileged updater:
// the root oneshot that does the work, the path unit that lets the
// unprivileged node ask for it, and the timer that runs the scheduled
// check. See templates/zzrouter-update.service for why the work is not
// done by the node itself.
var updaterUnits = []struct {
	name     string
	template string
	enable   bool
}{
	{"zzrouter-update.service", systemdUpdateServiceTemplate, false},
	{"zzrouter-update.path", systemdUpdatePathTemplate, true},
	{"zzrouter-update.timer", systemdUpdateTimerTemplate, true},
}

// installUpdaterUnits writes and enables the updater units.
//
// Only the path and the timer are enabled: the service they both point
// at is oneshot and is started by them, so enabling it too would run an
// update check at every boot.
func (m *SystemdManager) installUpdaterUnits(nodePath string) error {
	for _, unit := range updaterUnits {
		content := strings.ReplaceAll(unit.template, defaultManagedNodePath, nodePath)
		path := filepath.Join(filepath.Dir(systemdServicePath), unit.name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	if err := host.Command("systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("reload systemd after writing the updater units: %w", err)
	}

	for _, unit := range updaterUnits {
		if !unit.enable {
			continue
		}
		// --now so the watcher is live for a request made before the
		// next boot, which is every request an operator makes today.
		if err := host.Command("systemctl", "enable", "--now", unit.name).Run(); err != nil {
			return fmt.Errorf("enable %s: %w", unit.name, err)
		}
	}
	return nil
}

// defaultManagedNodePath is the ExecStart the updater templates ship
// with, replaced with wherever the node binary actually landed.
const defaultManagedNodePath = "/opt/zzrouter/bin/zzrouter-node"

// isSymlink reports whether path exists and is a symbolic link.
func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// UninstallService removes the systemd service
func (m *SystemdManager) UninstallService() error {
	// Stop the service first
	_ = host.Command("systemctl", "stop", "zzrouter-node.service").Run() // Best-effort cleanup: remove the unit even if systemctl cannot stop it.

	// Disable the service
	_ = host.Command("systemctl", "disable", "zzrouter-node.service").Run() // Best-effort cleanup: remove the unit even if systemctl cannot disable it.

	// Remove the service file
	if err := os.Remove(systemdServicePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove service file: %w", err)
	}

	// Reload systemd daemon
	if err := host.Command("systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("failed to reload systemd after uninstall: %w", err)
	}

	return nil
}

// StartService starts the zzrouter-node service via systemctl
func (m *SystemdManager) StartService() error {
	cmd := host.Command("systemctl", "start", "zzrouter-node.service")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}
	return nil
}

// StopService stops the zzrouter-node service via systemctl
func (m *SystemdManager) StopService() error {
	cmd := host.Command("systemctl", "stop", "zzrouter-node.service")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to stop service: %w", err)
	}
	return nil
}

// RestartService restarts the zzrouter-node service via systemctl
func (m *SystemdManager) RestartService() error {
	cmd := host.Command("systemctl", "restart", "zzrouter-node.service")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart service: %w", err)
	}
	return nil
}

// RestartsOnExit reports whether the unit's Restart= policy covers a
// non-zero exit. Only "always" and "on-failure" do: "on-abnormal" and
// "on-abort" react to signals rather than exit codes, and "on-success"
// would leave the node down after the failure exit an update restart
// uses. Anything else reports false, so callers never exit into a node
// nothing will bring back.
func (m *SystemdManager) RestartsOnExit() (bool, string) {
	output, err := host.Command("systemctl", "show", "-p", "Restart", "--value", "zzrouter-node.service").Output()
	if err != nil {
		return false, "could not read the Restart= policy of zzrouter-node.service"
	}

	switch policy := strings.TrimSpace(string(output)); policy {
	case "always", "on-failure":
		return true, ""
	case "":
		return false, "zzrouter-node.service declares no Restart= policy"
	default:
		return false, fmt.Sprintf("zzrouter-node.service has Restart=%s, which does not cover a failure exit", policy)
	}
}

// GetStatus returns the current status of the systemd service
func (m *SystemdManager) GetStatus() (*Status, error) {
	status := &Status{
		ConfigDir: m.GetConfigDir(),
		LogPath:   m.GetLogPath(),
	}

	// Check if service is active
	cmd := host.Command("systemctl", "is-active", "zzrouter-node.service")
	output, _ := cmd.Output()
	status.Running = strings.TrimSpace(string(output)) == "active"

	// Check if service is enabled
	cmd = host.Command("systemctl", "is-enabled", "zzrouter-node.service")
	output, _ = cmd.Output()
	status.Enabled = strings.TrimSpace(string(output)) == "enabled"

	// Get PID if running
	if status.Running {
		cmd = host.Command("systemctl", "show", "-p", "MainPID", "zzrouter-node.service")
		output, _ = cmd.Output()
		pidStr := strings.TrimPrefix(strings.TrimSpace(string(output)), "MainPID=")
		if pid, err := strconv.Atoi(pidStr); err == nil {
			status.PID = pid
		}

		// Get uptime from ActiveEnterTimestamp
		cmd = host.Command("systemctl", "show", "-p", "ActiveEnterTimestamp", "zzrouter-node.service")
		output, _ = cmd.Output()
		timestamp := strings.TrimPrefix(strings.TrimSpace(string(output)), "ActiveEnterTimestamp=")
		if timestamp != "" {
			status.Uptime = timestamp // Could parse and format, but raw is informative
		}
	}

	status.User = ServiceUser

	// Check for any errors in the service
	cmd = host.Command("systemctl", "status", "zzrouter-node.service")
	output, err := cmd.CombinedOutput()
	if err != nil && !status.Running {
		// Extract error info from status output
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.Contains(line, "Active: failed") || strings.Contains(line, "Status:") {
				status.Error = strings.TrimSpace(line)
				break
			}
		}
	}

	return status, nil
}

// GetLogPath returns the path to view logs (journalctl for systemd)
func (m *SystemdManager) GetLogPath() string {
	return "journalctl -u zzrouter-node.service"
}

// GetConfigDir returns the service configuration directory
func (m *SystemdManager) GetConfigDir() string {
	return LinuxConfigDir
}

// GetDataDir returns the service data directory
func (m *SystemdManager) GetDataDir() string {
	return LinuxDataDir
}

// GenerateAPIKey generates a secure admin API key and saves it to the environment file.
// Preserves existing keys if present (e.g., cluster key pushed during join).
func (m *SystemdManager) GenerateAPIKey() (string, error) {
	key, err := generateSecureKey()
	if err != nil {
		return "", fmt.Errorf("failed to generate API key: %w", err)
	}

	envPath := filepath.Join(m.GetConfigDir(), "zzrouter.env")

	vars := map[string]string{
		"ZZROUTER_ADMIN_API_KEY": key,
	}

	// Preserve existing keys if present
	if existingVars, err := readEnvFile(envPath); err == nil {
		if adminKey, ok := existingVars["ZZROUTER_ADMIN_API_KEY"]; ok && adminKey != "" {
			vars["ZZROUTER_ADMIN_API_KEY"] = adminKey
			key = adminKey // Return the existing key, not the new one
		}
		if clusterKey, ok := existingVars["ZZROUTER_CLUSTER_NETWORK_KEY"]; ok && clusterKey != "" {
			vars["ZZROUTER_CLUSTER_NETWORK_KEY"] = clusterKey
		}
		if userKey, ok := existingVars["ZZROUTER_API_KEY"]; ok && userKey != "" {
			vars["ZZROUTER_API_KEY"] = userKey
		}
	}

	if err := writeEnvFile(envPath, vars); err != nil {
		return "", fmt.Errorf("failed to write env file: %w", err)
	}

	// Chown the env file to zzrouter user
	if u, err := user.Lookup(ServiceUser); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		if err := os.Chown(envPath, uid, gid); err != nil {
			return "", fmt.Errorf("failed to chown env file: %w", err)
		}
	}

	return key, nil
}

// SwitchToServiceUser re-executes the current process as the zzrouter user
func (m *SystemdManager) SwitchToServiceUser() error {
	// Get zzrouter user info
	u, err := user.Lookup(ServiceUser)
	if err != nil {
		return fmt.Errorf("failed to lookup service user: %w", err)
	}

	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	// Get the current executable path
	exe, err := getExecutablePath()
	if err != nil {
		return err
	}

	// Build arguments, adding --no-setup flag to prevent re-running setup
	args := []string{exe}
	args = append(args, os.Args[1:]...)
	// Check if --no-setup is already present
	hasNoSetup := false
	for _, arg := range args {
		if arg == "--no-setup" {
			hasNoSetup = true
			break
		}
	}
	if !hasNoSetup {
		args = append(args, "--no-setup")
	}

	// Use runuser to switch to the service user
	// This is more reliable than setuid/setgid for service contexts
	runuserArgs := []string{"-u", ServiceUser, "--"}
	runuserArgs = append(runuserArgs, args...)

	// Set environment for the new process
	env := os.Environ()
	env = append(env, fmt.Sprintf("HOME=%s", LinuxDataDir))
	env = append(env, fmt.Sprintf("USER=%s", ServiceUser))

	// Try runuser first (systemd systems)
	runuser, err := exec.LookPath("runuser")
	if err == nil {
		return syscall.Exec(runuser, append([]string{"runuser"}, runuserArgs...), env) //nolint:gosec // G702: runuser receives separate argv after --, never a shell command.
	}

	// Fall back to su
	su, err := exec.LookPath("su")
	if err == nil {
		suArgs := serviceUserSUArgs(args)
		return syscall.Exec(su, suArgs, env) //nolint:gosec // G702: the shell program is fixed; executable and arguments are positional parameters.
	}

	// Last resort: setuid/setgid directly
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("failed to setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("failed to setuid: %w", err)
	}

	return syscall.Exec(exe, args, env) //nolint:gosec // G702: exe is the current executable; original argv is passed directly without a shell.
}

// readEnvFile is defined in manager.go (cross-platform)

// serviceUserSUArgs preserves argv through su without interpreting it as shell code.
func serviceUserSUArgs(args []string) []string {
	return append([]string{"su", "-s", "/bin/sh", ServiceUser, "-c", `exec "$@"`, "--", "zzrouter"}, args...)
}
