//go:build darwin

package service

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/stperic/zzrouter/pkg/host"
	"howett.net/plist"
)

// NewServiceManager returns the macOS launchd service manager
func NewServiceManager() ServiceManager {
	return &LaunchdManager{}
}

// LaunchdManager handles service management on macOS using launchd
type LaunchdManager struct{}

// Service identifiers
const (
	launchdLabel       = "com.zzrouter.node"
	launchdPlistName   = "com.zzrouter.node.plist"
	macOSAppSupportDir = "Library/Application Support/zzrouter"
	macOSLogsDir       = "Library/Logs/zzrouter"
)

// LaunchdPlist represents the launchd plist structure
type LaunchdPlist struct {
	Label                string            `plist:"Label"`
	ProgramArguments     []string          `plist:"ProgramArguments"`
	RunAtLoad            bool              `plist:"RunAtLoad"`
	KeepAlive            bool              `plist:"KeepAlive"`
	StandardOutPath      string            `plist:"StandardOutPath"`
	StandardErrorPath    string            `plist:"StandardErrorPath"`
	EnvironmentVariables map[string]string `plist:"EnvironmentVariables,omitempty"`
	WorkingDirectory     string            `plist:"WorkingDirectory"`
}

// IsPrivileged returns true if running as root
func (m *LaunchdManager) IsPrivileged() bool {
	return os.Getuid() == 0
}

// IsFirstRun returns true if the launchd plist doesn't exist
func (m *LaunchdManager) IsFirstRun() bool {
	plistPath := m.getPlistPath()
	_, err := os.Stat(plistPath)
	return os.IsNotExist(err)
}

// IsServiceUser returns false on macOS (runs as current user)
func (m *LaunchdManager) IsServiceUser() bool {
	// On macOS, the service runs as the current user, not a dedicated user
	return false
}

// CreateServiceUser is a no-op on macOS (runs as current user)
func (m *LaunchdManager) CreateServiceUser() error {
	// No dedicated user needed on macOS - standard practice is to run as current user
	return nil
}

// SetupDirectories creates required directories in ~/Library
func (m *LaunchdManager) SetupDirectories() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	dirs := []string{
		filepath.Join(homeDir, macOSAppSupportDir),
		filepath.Join(homeDir, macOSLogsDir),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return nil
}

// InstallService creates and loads the launchd plist
func (m *LaunchdManager) InstallService() error {
	// Get executable path
	exe, err := getExecutablePath()
	if err != nil {
		return err
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	configDir := filepath.Join(homeDir, macOSAppSupportDir)
	logDir := filepath.Join(homeDir, macOSLogsDir)

	// Create the plist structure
	plistData := LaunchdPlist{
		Label: launchdLabel,
		ProgramArguments: []string{
			exe,
			"start",
			"--no-setup",
		},
		RunAtLoad:         true,
		KeepAlive:         true,
		StandardOutPath:   filepath.Join(logDir, "output.log"),
		StandardErrorPath: filepath.Join(logDir, "error.log"),
		WorkingDirectory:  configDir,
		EnvironmentVariables: map[string]string{
			"ZZROUTER_CONFIG_DIR": configDir,
		},
	}

	// Encode to plist format
	plistBytes, err := plist.MarshalIndent(plistData, plist.XMLFormat, "\t")
	if err != nil {
		return fmt.Errorf("failed to marshal plist: %w", err)
	}

	// Ensure LaunchAgents directory exists
	launchAgentsDir := filepath.Join(homeDir, "Library/LaunchAgents")
	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents directory: %w", err)
	}

	// Write the plist file
	plistPath := m.getPlistPath()
	if err := os.WriteFile(plistPath, plistBytes, 0644); err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}

	// Load the service (unload first if it exists; ignore error when not loaded)
	_ = host.Command("launchctl", "unload", plistPath).Run()
	cmd := host.Command("launchctl", "load", plistPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to load launchd service: %w", err)
	}

	return nil
}

// UninstallService unloads and removes the launchd plist
func (m *LaunchdManager) UninstallService() error {
	plistPath := m.getPlistPath()

	// Unload the service (ignore error when not loaded)
	_ = host.Command("launchctl", "unload", plistPath).Run()

	// Remove the plist file
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist file: %w", err)
	}

	return nil
}

// StartService starts the service via launchctl
func (m *LaunchdManager) StartService() error {
	cmd := host.Command("launchctl", "start", launchdLabel)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}
	return nil
}

// StopService stops the service via launchctl
func (m *LaunchdManager) StopService() error {
	cmd := host.Command("launchctl", "stop", launchdLabel)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to stop service: %w", err)
	}
	return nil
}

// RestartService restarts the service via launchctl
func (m *LaunchdManager) RestartService() error {
	plistPath := m.getPlistPath()

	// Stop the service (ignore error when not running)
	_ = host.Command("launchctl", "stop", launchdLabel).Run()

	// Unload and reload
	_ = host.Command("launchctl", "unload", plistPath).Run()
	cmd := host.Command("launchctl", "load", plistPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart service: %w", err)
	}

	return nil
}

// RestartsOnExit reports whether the installed plist asks launchd to
// relaunch the job when it exits. KeepAlive is what makes an update's
// self-exit come back; a hand-edited plist that dropped it would leave
// the node down, so the plist on disk is the answer rather than the
// value this package writes at install time.
func (m *LaunchdManager) RestartsOnExit() (bool, string) {
	plistPath := m.getPlistPath()

	data, err := os.ReadFile(plistPath) //nolint:gosec // path is this package's own install location
	if err != nil {
		return false, fmt.Sprintf("could not read %s", plistPath)
	}

	var installed LaunchdPlist
	if _, err := plist.Unmarshal(data, &installed); err != nil {
		return false, fmt.Sprintf("could not parse %s", plistPath)
	}
	if !installed.KeepAlive {
		return false, fmt.Sprintf("%s has KeepAlive disabled", plistPath)
	}
	return true, ""
}

// launchctlDictInt reads an integer value out of the plist-style dict
// that `launchctl list <label>` prints, e.g. `	"PID" = 4711;`. The
// unlabelled `launchctl list` prints a PID/Status/Label table instead —
// parsing that shape here silently reported every job as not running.
func launchctlDictInt(output, key string) (int, bool) {
	needle := `"` + key + `" = `
	for _, line := range strings.Split(output, "\n") {
		_, value, found := strings.Cut(line, needle)
		if !found {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(value), ";"))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// GetStatus returns the current status of the launchd service
func (m *LaunchdManager) GetStatus() (*Status, error) {
	status := &Status{
		ConfigDir: m.GetConfigDir(),
		LogPath:   m.GetLogPath(),
	}

	// Get current user for status
	currentUser, err := user.Current()
	if err == nil {
		status.User = currentUser.Username
	}

	// Check if service is loaded and get PID
	cmd := host.Command("launchctl", "list", launchdLabel)
	output, err := cmd.Output()
	if err != nil {
		// Service not loaded
		status.Running = false
		status.Enabled = false
		return status, nil //nolint:nilerr // "not loaded" is a data outcome, not a caller error
	}

	if pid, ok := launchctlDictInt(string(output), "PID"); ok {
		status.PID = pid
		status.Running = pid > 0
	}
	if code, ok := launchctlDictInt(string(output), "LastExitStatus"); ok && code != 0 {
		status.Error = fmt.Sprintf("Exit status: %d", code)
	}

	// Check if plist exists (enabled)
	plistPath := m.getPlistPath()
	if _, err := os.Stat(plistPath); err == nil {
		status.Enabled = true
	}

	return status, nil
}

// GetLogPath returns the path to service logs
func (m *LaunchdManager) GetLogPath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, macOSLogsDir, "output.log")
}

// GetConfigDir returns the service configuration directory
func (m *LaunchdManager) GetConfigDir() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, macOSAppSupportDir)
}

// GetDataDir returns the service data directory (same as config on macOS)
func (m *LaunchdManager) GetDataDir() string {
	return m.GetConfigDir()
}

// GenerateAPIKey generates a secure API key and saves it to the environment file
func (m *LaunchdManager) GenerateAPIKey() (string, error) {
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
			key = adminKey
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

	return key, nil
}

// SwitchToServiceUser is a no-op on macOS (runs as current user)
func (m *LaunchdManager) SwitchToServiceUser() error {
	// On macOS, we just start the service directly as the current user
	return nil
}

// getPlistPath returns the path to the launchd plist file
func (m *LaunchdManager) getPlistPath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, "Library/LaunchAgents", launchdPlistName)
}

// GeneratePlistFromTemplate generates a plist file using a Go template
// This is an alternative to the struct-based approach if more customization is needed
func (m *LaunchdManager) GeneratePlistFromTemplate() ([]byte, error) {
	tmpl := GetLaunchdPlistTemplate()

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	exe, err := getExecutablePath()
	if err != nil {
		return nil, err
	}

	data := struct {
		BinaryPath string
		ConfigDir  string
		LogDir     string
	}{
		BinaryPath: exe,
		ConfigDir:  filepath.Join(homeDir, macOSAppSupportDir),
		LogDir:     filepath.Join(homeDir, macOSLogsDir),
	}

	t, err := template.New("plist").Parse(tmpl)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
