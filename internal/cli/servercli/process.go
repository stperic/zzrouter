package servercli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mitchellh/go-ps"
	"github.com/spf13/cobra"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/service"

	"github.com/stperic/zzrouter/pkg/utils"
)

// checkNodeNotRunning checks if a zzrouter-node is already running
// Returns error if ANY server instance is already running (regardless of port)
// This is the SINGLE centralized check used by both detached and interactive modes
func checkNodeNotRunning() error {
	// Get current process info to identify our own process
	currentPid := os.Getpid()

	// Get parent process info (for detached mode where parent spawns child)
	parentPid := os.Getppid()

	// Find all processes
	processes, err := ps.Processes()
	if err != nil {
		return fmt.Errorf("failed to list processes: %w", err)
	}

	// Look for existing zzrouter-node processes
	for _, proc := range processes {
		// Skip our own process and parent process (detached launcher)
		if proc.Pid() == currentPid || proc.Pid() == parentPid {
			continue
		}

		// Check if executable contains "zzrouter-node" (server process only)
		// Only check for server processes, not client commands like "zzrouter chat"
		// This prevents running multiple server instances regardless of port
		exeName := proc.Executable()
		if !strings.Contains(exeName, "zzrouter-node") {
			continue
		}
		// Running this binary is not the same as serving. The
		// privileged updater is this binary too, and it is alive
		// precisely while a node is restarting onto a new version --
		// see isServerProcess.
		if !isServerProcess(proc.Pid()) {
			continue
		}
		return fmt.Errorf("zzRouter server is already running. Use 'zzrouter-node stop' to stop it first")
	}

	return nil
}

// cleanupExistingProcesses kills existing zzrouter serve processes
// Only used by 'zzrouter-node stop' command
func cleanupExistingProcesses() error {
	return killExistingServeProcesses()
}

// killExistingServeProcesses finds and kills the existing zzrouter server process
// Only one server process is allowed per host
func killExistingServeProcesses() error {
	currentPid := os.Getpid()

	// Find all processes
	processes, err := ps.Processes()
	if err != nil {
		return fmt.Errorf("failed to list processes: %w", err)
	}

	// Find the zzrouter-node process (should only be one)
	for _, proc := range processes {
		// isServerProcess for the same reason as checkNodeNotRunning,
		// and with a sharper consequence here: without it, `zzrouter-node
		// stop` kills the privileged updater, and killing it mid-update
		// abandons a node on a version nothing is left watching.
		if strings.Contains(proc.Executable(), "zzrouter-node") && proc.Pid() != currentPid && isServerProcess(proc.Pid()) {
			stopped, err := stopManagedService(proc.Pid())
			if err != nil {
				return err
			}
			if stopped {
				return nil
			}
			fmt.Printf("Found existing zzRouter server process (PID %d), terminating...\n", proc.Pid())
			if err := killPid(proc.Pid()); err != nil {
				return fmt.Errorf("failed to kill process %d: %w", proc.Pid(), err)
			}
			fmt.Println("zzRouter server process terminated successfully.")
			return nil
		}
	}

	// No existing process found - this is normal during startup, don't print message
	// (Message was confusing when shown during normal startup)
	return nil
}

// stopManagedService stops the node through the system service manager
// when the process found IS that service, and reports whether it did.
//
// Signalling a service's process stops nothing for long: the SCM and
// systemd both restart it, so the node is back seconds later, still on
// the config it already had. The symptom is a node that will not stay
// stopped, and an operator who concludes their config edit did not take.
func stopManagedService(pid int) (bool, error) {
	return stopServiceIfManaging(service.NewServiceManager(), pid)
}

// stopServiceIfManaging is stopManagedService with the service manager
// supplied, so the decision can be exercised without a real SCM.
func stopServiceIfManaging(mgr service.ServiceManager, pid int) (bool, error) {
	if mgr == nil {
		return false, nil
	}
	status, err := mgr.GetStatus()
	if err != nil {
		// A manager we cannot query is, as far as this decision goes,
		// not managing anything: fall through to signalling the process.
		return false, nil //nolint:nilerr // an unqueryable service manager means "not managed", not a failure to report
	}
	if status == nil || !status.Running || status.PID != pid {
		return false, nil
	}

	fmt.Printf("zzRouter is running as a system service (PID %d); stopping the service.\n", pid)
	if err := mgr.StopService(); err != nil {
		fmt.Println("The node runs under the service manager, so terminating the process")
		fmt.Println("would only have it restarted. Retry with administrator or root privileges.")
		return false, fmt.Errorf("failed to stop the zzRouter service (PID %d): %w", pid, err)
	}
	fmt.Println("zzRouter service stopped.")
	return true, nil
}

// processAliveInterval is how often killPid re-checks that a signaled
// process has exited. Bounded by ProcessWaitTimeout on the outside.
const processAliveInterval = 50 * time.Millisecond

// killPid stops a process gracefully: signal first (SIGTERM on Unix,
// os.Interrupt on Windows so the server runs its shutdown hooks), poll
// liveness up to ProcessWaitTimeout, escalate to Kill only if it hasn't
// exited. Graceful shutdown is required for cluster integrations that
// rely on shutdown handlers firing (worker goodbye notify, spend state
// flush).
//
// Liveness is observed via ps.FindProcess (table scan, nil on exit)
// rather than process.Wait: the target is not our child, so on macOS
// and older Linux kernels Wait returns ECHILD immediately, turning the
// "graceful wait" into a false positive that silently skipped the full
// grace period.
func killPid(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(gracefulStopSignal()); err != nil {
		// Signal delivery failed (process already gone, or unsupported
		// on this platform) — fall through to hard Kill.
		_ = process.Kill()
		return nil //nolint:nilerr // signal failure usually means the process is already gone; Kill is best-effort
	}
	deadline := utils.Now().Add(constants.ProcessWaitTimeout)
	for utils.Now().Before(deadline) {
		if p, _ := ps.FindProcess(pid); p == nil {
			return nil
		}
		time.Sleep(processAliveInterval)
	}
	_ = process.Kill()
	return nil
}

// handleDetachedMode handles starting the server in detached mode
func handleDetachedMode(cmd *cobra.Command, hostCfg *pkgConfig.NodeConfig) error {
	// Run server in background by spawning a separate process
	if err := startDetachedNode(cmd); err != nil {
		return fmt.Errorf("failed to start detached server: %w", err)
	}

	fmt.Printf("Node started in detached mode on port %d\n", hostCfg.Node.Port)
	fmt.Printf("Use 'zzrouter-node stop' to stop the server\n")
	fmt.Println()
	return nil
}

// detachedNodeArgs builds the argv the detached child is spawned with.
//
// The child is what actually becomes the server, and it inherits the
// parent's elevation, so every flag that decides how the server comes up
// has to cross the detach boundary. --no-setup is the load-bearing one:
// drop it and an elevated child takes the privileged path, installs the
// system service, and the service then runs as its own account reading
// its own node.yaml rather than the config this command was pointed at.
func detachedNodeArgs(cmd *cobra.Command) ([]string, error) {
	port, _, err := parseNodeFlags(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to parse flags: %w", err)
	}

	// ALWAYS include the port in the command line for process detection.
	if port == 0 {
		port = constants.DefaultZZROUTERPort
	}
	args := []string{"start", "--port", fmt.Sprintf("%d", port)}

	if configDir, _ := cmd.Flags().GetString("dir"); configDir != "" {
		args = append(args, "--dir", configDir)
	}
	if debug, _ := cmd.Flags().GetBool("debug"); debug {
		args = append(args, "--debug")
	}
	if noSetup, _ := cmd.Flags().GetBool("no-setup"); noSetup {
		args = append(args, "--no-setup")
	}

	return args, nil
}

// startDetachedNode spawns a new process to run the server in detached mode
// Returns immediately after spawning - does not wait for server to be ready
func startDetachedNode(cmd *cobra.Command) error {
	// Check if server is already running BEFORE spawning
	// This prevents race conditions when multiple detach commands run simultaneously
	if err := checkNodeNotRunning(); err != nil {
		return err
	}

	// Get the current executable path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	args, err := detachedNodeArgs(cmd)
	if err != nil {
		return err
	}

	detachedCmd := host.Command(execPath, args...)

	// The child will open its own rotating log via pkg/logger; we pass the
	// path through an env var rather than piping a file descriptor. Piping
	// would split ownership between the child's inherited os.Stderr and
	// lumberjack's internal fd — after a rotation, the inherited fd points
	// at the archived file and late panic stacks land in the wrong place.
	// Letting the child own the file end-to-end avoids that.
	logPath := serverLogPath()
	if logPath != "" {
		// Preserve operator-set ZZROUTER_LOG_FILE if present — they may
		// have pointed it elsewhere intentionally. Only inject our default
		// when the env var is unset upstream.
		if _, already := os.LookupEnv("ZZROUTER_LOG_FILE"); !already {
			env := os.Environ()
			env = append(env, "ZZROUTER_LOG_FILE="+logPath)
			detachedCmd.Env = env
		}
	}
	detachedCmd.Stdout = nil
	detachedCmd.Stderr = nil
	detachedCmd.Stdin = nil

	// Set platform-specific process attributes for proper daemonization
	// Unix: Setpgid to create new process group
	// Windows: CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS
	detachedCmd.SysProcAttr = getSysProcAttr()

	// Start the process in background
	if err := detachedCmd.Start(); err != nil {
		return fmt.Errorf("failed to start detached process: %w", err)
	}

	// Return immediately - don't wait for server
	// Node will log "zzRouter server started" when ready
	fmt.Println("zzRouter server process started.")
	if logPath != "" {
		fmt.Printf("Server logs: %s\n", logPath)
	}

	return nil
}

// serverLogPath returns the node's rotating log file path, ensuring the
// logs directory exists. Returns "" on any filesystem error so the caller
// can fall back to not setting ZZROUTER_LOG_FILE — the child will then run
// with the default stderr handler.
func serverLogPath() string {
	paths := pkgConfig.Paths()
	logsDir := paths.GetLogsDir()
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return ""
	}
	return filepath.Join(logsDir, "zzrouter-node.log")
}
