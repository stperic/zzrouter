//go:build windows

package service

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/host"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// NewServiceManager returns the Windows SCM service manager
func NewServiceManager() ServiceManager {
	return &WindowsManager{}
}

// WindowsManager handles service management on Windows using SCM
type WindowsManager struct{}

// Windows service constants
const (
	windowsServiceName        = "zzrouter"
	windowsServiceDisplayName = "zzRouter Node Service"
	windowsServiceDescription = "zzRouter distributed LLM management service"
)

// IsPrivileged returns true if running with Administrator privileges
func (m *WindowsManager) IsPrivileged() bool {
	// Check if we can open the service manager with admin access
	scm, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer scm.Disconnect() //nolint:errcheck // best-effort cleanup in defer

	// Try to open the service manager with full access
	// This will only succeed if we have admin privileges
	return true
}

// IsFirstRun returns true if the Windows service doesn't exist
func (m *WindowsManager) IsFirstRun() bool {
	scm, err := mgr.Connect()
	if err != nil {
		return true
	}
	defer scm.Disconnect() //nolint:errcheck // best-effort cleanup in defer

	s, err := scm.OpenService(windowsServiceName)
	if err != nil {
		return true // Service doesn't exist
	}
	s.Close()
	return false
}

// IsServiceUser returns true if running as the LocalService account
func (m *WindowsManager) IsServiceUser() bool {
	// Check if running as a Windows service
	isService, err := svc.IsWindowsService()
	return err == nil && isService
}

// CreateServiceUser is a no-op on Windows (uses LocalService account)
func (m *WindowsManager) CreateServiceUser() error {
	// Windows services use built-in accounts (LocalService, LocalSystem, etc.)
	// No user creation needed
	return nil
}

// SetupDirectories creates required directories in %PROGRAMDATA%
func (m *WindowsManager) SetupDirectories() error {
	programData := os.Getenv("PROGRAMDATA")
	if programData == "" {
		programData = `C:\ProgramData`
	}

	dirs := []string{
		filepath.Join(programData, "zzrouter", "config"),
		filepath.Join(programData, "zzrouter", "logs"),
		filepath.Join(programData, "zzrouter", "data"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return nil
}

// InstallService installs the Windows service using SCM
func (m *WindowsManager) InstallService() error {
	exe, err := getExecutablePath()
	if err != nil {
		return err
	}

	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to service manager: %w", err)
	}
	defer scm.Disconnect() //nolint:errcheck // best-effort cleanup in defer

	// Check if service already exists
	s, err := scm.OpenService(windowsServiceName)
	if err == nil {
		s.Close()
		return nil // Service already installed
	}

	// Create the service
	s, err = scm.CreateService(
		windowsServiceName,
		exe,
		mgr.Config{
			DisplayName: windowsServiceDisplayName,
			Description: windowsServiceDescription,
			StartType:   mgr.StartAutomatic,
		},
		"start", "--no-setup", "--service",
	)
	if err != nil {
		return fmt.Errorf("failed to create service: %w", err)
	}
	defer s.Close()

	// Restart after a failure exit, including the non-zero stop an update's
	// self-exit reports: SCM counts that as a non-crash failure and skips
	// recovery actions unless the non-crash flag is set. Failures only warn;
	// RestartsOnExit reports them before anything relies on a restart.
	recoveryActions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if err := s.SetRecoveryActions(recoveryActions, 86400); err != nil {
		fmt.Printf("Warning: failed to set recovery options: %v\n", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		fmt.Printf("Warning: failed to apply recovery options to non-crash failures: %v\n", err)
	}
	return nil
}

// StartService starts the Windows service
func (m *WindowsManager) StartService() error {
	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to service manager: %w", err)
	}
	defer scm.Disconnect() //nolint:errcheck // best-effort cleanup in defer

	s, err := scm.OpenService(windowsServiceName)
	if err != nil {
		return fmt.Errorf("failed to open service: %w", err)
	}
	defer s.Close()

	if err := s.Start(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	return nil
}

// StopService stops the Windows service
func (m *WindowsManager) StopService() error {
	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to service manager: %w", err)
	}
	defer scm.Disconnect() //nolint:errcheck // best-effort cleanup in defer

	s, err := scm.OpenService(windowsServiceName)
	if err != nil {
		return fmt.Errorf("failed to open service: %w", err)
	}
	defer s.Close()

	_, err = s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("failed to stop service: %w", err)
	}

	return nil
}

// RestartService restarts the Windows service
func (m *WindowsManager) RestartService() error {
	// Try to stop the service (ignore errors - might not be running)
	_ = m.StopService()

	// Wait for service to actually stop (poll status instead of blind sleep)
	if err := m.waitForStop(); err != nil {
		// Log warning but continue - service might already be stopped
		fmt.Printf("Warning: waiting for service stop: %v\n", err)
	}

	return m.StartService()
}

// RestartsOnExit reports whether SCM would start the service again after
// an update's self-exit, a non-zero stop that SCM counts as a non-crash
// failure. That takes a restart recovery action and the non-crash flag;
// without either the service would stay stopped.
func (m *WindowsManager) RestartsOnExit() (bool, string) {
	scm, err := mgr.Connect()
	if err != nil {
		return false, fmt.Sprintf("cannot connect to the service manager: %v", err)
	}
	defer scm.Disconnect() //nolint:errcheck // best-effort cleanup in defer

	s, err := scm.OpenService(windowsServiceName)
	if err != nil {
		return false, fmt.Sprintf("service %s is not installed", windowsServiceName)
	}
	defer s.Close()

	actions, err := s.RecoveryActions()
	if err != nil {
		return false, fmt.Sprintf("cannot read the recovery actions of %s: %v", windowsServiceName, err)
	}
	nonCrash, err := s.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return false, fmt.Sprintf("cannot read the non-crash failure flag of %s: %v", windowsServiceName, err)
	}
	return restartCovered(actions, nonCrash)
}

// restartCovered is RestartsOnExit's decision over what SCM reported.
func restartCovered(actions []mgr.RecoveryAction, nonCrash bool) (bool, string) {
	if !slices.ContainsFunc(actions, func(a mgr.RecoveryAction) bool { return a.Type == mgr.ServiceRestart }) {
		return false, fmt.Sprintf("service %s has no restart recovery action", windowsServiceName)
	}
	if !nonCrash {
		return false, fmt.Sprintf("service %s does not restart after a non-crash exit; as Administrator run `sc.exe failureflag %s 1`", windowsServiceName, windowsServiceName)
	}
	return true, ""
}

// GetStatus returns the current status of the Windows service
func (m *WindowsManager) GetStatus() (*Status, error) {
	status := &Status{
		ConfigDir: m.GetConfigDir(),
		LogPath:   m.GetLogPath(),
		User:      "LocalService",
	}

	scm, err := mgr.Connect()
	if err != nil {
		status.Error = fmt.Sprintf("Cannot connect to service manager: %v", err)
		return status, nil
	}
	defer scm.Disconnect() //nolint:errcheck // best-effort cleanup in defer

	s, err := scm.OpenService(windowsServiceName)
	if err != nil {
		status.Error = "Service not installed"
		return status, nil
	}
	defer s.Close()

	// Get service status
	svcStatus, err := s.Query()
	if err != nil {
		status.Error = fmt.Sprintf("Cannot query service: %v", err)
		return status, nil
	}

	status.Running = svcStatus.State == svc.Running
	status.PID = int(svcStatus.ProcessId)

	// Get service config for enabled status
	config, err := s.Config()
	if err == nil {
		status.Enabled = config.StartType == mgr.StartAutomatic
	}

	// Get uptime from Windows event log (simplified)
	if status.Running {
		status.Uptime = "Running"
	}

	return status, nil
}

// GetLogPath returns the path to service logs
func (m *WindowsManager) GetLogPath() string {
	programData := os.Getenv("PROGRAMDATA")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, "zzrouter", "logs", "zzrouter-node.log")
}

// GetConfigDir returns the service configuration directory
func (m *WindowsManager) GetConfigDir() string {
	programData := os.Getenv("PROGRAMDATA")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, "zzrouter", "config")
}

// GetDataDir returns the service data directory
func (m *WindowsManager) GetDataDir() string {
	programData := os.Getenv("PROGRAMDATA")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, "zzrouter", "data")
}

// GenerateAPIKey generates a secure API key and saves it to the environment file
func (m *WindowsManager) GenerateAPIKey() (string, error) {
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

// SwitchToServiceUser starts the Windows service instead of re-executing
func (m *WindowsManager) SwitchToServiceUser() error {
	// On Windows, we start the service instead of switching users
	return m.StartService()
}

// RunAsService runs the server as a Windows service
// This is called when the --service flag is passed
func RunAsService(runFunc func() error) error {
	return svc.Run(windowsServiceName, &windowsService{runFunc: runFunc})
}

// windowsService implements the Windows service interface
type windowsService struct {
	runFunc func() error
}

func (ws *windowsService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	// Start the server in a goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- ws.runFunc()
	}()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for {
		select {
		case err := <-errChan:
			if err != nil {
				// SCM logs errno=1 as "Incorrect function" with no body —
				// route the actual error through slog (file backend wired
				// in runUnderServiceManager) so operators have something
				// to grep when the service won't stay up.
				slog.Error("zzrouter-node service runFunc returned error", "error", err)
				fmt.Printf("Service error: %v\n", err)
				changes <- svc.Status{State: svc.StopPending}
				return false, 1
			}
			return false, 0

		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				// Signal shutdown (the server should have a way to handle this)
				return false, 0
			}
		}
	}
}

// IsWindowsService checks if the current process is running as a Windows service
func IsWindowsService() bool {
	isService, err := svc.IsWindowsService()
	return err == nil && isService
}

// waitForStop polls service status until stopped or timeout
func (m *WindowsManager) waitForStop() error {
	scm, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer scm.Disconnect() //nolint:errcheck // best-effort cleanup in defer

	s, err := scm.OpenService(windowsServiceName)
	if err != nil {
		return nil // Service doesn't exist, consider it stopped
	}
	defer s.Close()

	return waitForServiceStop(s)
}

// waitForServiceStop polls service status until stopped or timeout
func waitForServiceStop(s *mgr.Service) error {
	timeout := time.After(constants.WindowsServiceStopTimeout)
	ticker := time.NewTicker(constants.WindowsServicePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for service to stop")
		case <-ticker.C:
			status, err := s.Query()
			if err != nil {
				// Can't query status, assume stopped
				return nil
			}
			if status.State == svc.Stopped {
				return nil
			}
		}
	}
}

// ViewLogs opens the log file or displays recent logs
func (m *WindowsManager) ViewLogs() error {
	logPath := m.GetLogPath()

	// Check if log file exists
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		// Try viewing Windows Event Log instead
		cmd := host.Command("powershell", "-Command",
			fmt.Sprintf(`Get-EventLog -LogName Application -Source "%s" -Newest 50 | Format-Table -AutoSize`, windowsServiceName))
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Read and display the log file
	content, err := os.ReadFile(logPath)
	if err != nil {
		return fmt.Errorf("failed to read log file: %w", err)
	}

	// Display last 100 lines
	lines := strings.Split(string(content), "\n")
	start := 0
	if len(lines) > 100 {
		start = len(lines) - 100
	}
	for _, line := range lines[start:] {
		fmt.Println(line)
	}

	return nil
}
