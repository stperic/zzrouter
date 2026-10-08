package process

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/security"
)

// LauncherSHA256 is the expected SHA256 hash of the zzrouter-launcher binary.
// Set at build time via ldflags. If empty, launcher integrity check is skipped.
var LauncherSHA256 string

// ManagedProcessEnvKey is set on all processes launched by zzRouter
// to distinguish them from unrelated processes during orphan detection.
const ManagedProcessEnvKey = "ZZROUTER_PROCESS_MARKER"

// ManagedProcessEnvValue is the value for the marker env var.
const ManagedProcessEnvValue = "1"

// LaunchResult contains the outcome of a process launch.
type LaunchResult struct {
	PID            int
	ProcessGroupID int
	Process        *os.Process
	command        *exec.Cmd
}

// Wait reaps the process and drains its output before returning exit details.
func (r *LaunchResult) Wait() (*os.ProcessState, error) {
	err := r.command.Wait()
	return r.command.ProcessState, err
}

// Launcher starts and manages native processes for provider instances.
type Launcher struct {
	security *SecurityValidator
	trusted  bool   // true for config-driven apps (skip command validation)
	pidDir   string // directory for launcher PID files (shared with PIDTracker)
}

// NewLauncher creates a process launcher.
// If trusted is true, command validation is skipped (config-driven apps).
// pidDir sets the PID directory for the launcher wrapper; empty uses the launcher's default.
func NewLauncher(securityConfig *SecurityConfig, trusted bool, pidDir string) *Launcher {
	return &Launcher{
		security: NewSecurityValidator(securityConfig),
		trusted:  trusted,
		pidDir:   pidDir,
	}
}

// maxLoggedArgBytes is the longest argument the launch log line prints;
// a longer one is a file's text, not a flag value anyone reads there.
const maxLoggedArgBytes = 256

// outputDrainTimeout bounds pipes retained by surviving descendants.
const outputDrainTimeout = 2 * time.Second

// launchLogLine is the command as the launch log shows it, quoted for
// paths with spaces. A file handed over by content (a chat template, say)
// is shown by size; its digest is in the run's resolved files.
func launchLogLine(cmd string, args []string) string {
	var line strings.Builder
	line.WriteString(cmd)
	for _, arg := range args {
		switch {
		case len(arg) > maxLoggedArgBytes:
			fmt.Fprintf(&line, " <%d bytes>", len(arg))
		case strings.Contains(arg, " "):
			fmt.Fprintf(&line, " %q", arg)
		default:
			line.WriteString(" " + arg)
		}
	}
	return line.String()
}

// EnvironmentTransform composes trusted paths after validating caller overrides.
type EnvironmentTransform func([]string) ([]string, error)

// Launch starts a native process for the given instance.
// The instance must have Port and HealthURL already set.
// On success, updates inst.ProcessID, inst.ProcessGroupID, inst.LaunchCommand.
//
// Process lifetime is NOT tied to ctx — the process runs until explicitly
// terminated via Terminate(). The caller must call result.Wait()
// to reap the child and avoid zombies.
func (l *Launcher) Launch(_ context.Context, inst *instance.Instance, cmd string, args []string, envVars map[string]string, logWriter io.Writer, transforms ...EnvironmentTransform) (*LaunchResult, error) {
	env, err := ChildEnvironment(envVars)
	if err != nil {
		return nil, err
	}
	for _, transform := range transforms {
		if transform == nil {
			continue
		}
		env, err = transform(env)
		if err != nil {
			return nil, err
		}
	}

	// Store original command before sandboxing
	originalCmd := cmd
	originalArgs := append([]string{}, args...)

	// Security validation for ad-hoc commands only
	fullCommand := append([]string{cmd}, args...)
	if !l.trusted {
		if err := l.security.ValidateCommand(fullCommand); err != nil {
			return nil, fmt.Errorf("command security validation failed: %w", err)
		}
	}

	// Apply sandboxing (all commands, config-driven included)
	fullCommand = l.security.SandboxCommand(fullCommand)
	if len(fullCommand) > 0 {
		cmd = fullCommand[0]
		args = fullCommand[1:]
	}

	// Wrap with zzrouter-launcher for PID tracking and signal forwarding
	if launcherPath := findLauncherBinary(); launcherPath != "" {
		launcherArgs := []string{
			"--instance-id", inst.ID,
			"--provider", inst.Provider,
		}
		if l.pidDir != "" {
			launcherArgs = append(launcherArgs, "--pid-dir", l.pidDir)
		}
		launcherArgs = append(launcherArgs, "--", cmd)
		launcherArgs = append(launcherArgs, args...)
		cmd = launcherPath
		args = launcherArgs
		inst.SetUsingLauncher(true)
	} else {
		// Say so. Without the launcher this process still starts and
		// still serves, so the only symptom is that stopping it later
		// does not work and nothing explains why. verifyLauncher logs
		// the tampered-binary case, but a launcher that is simply
		// absent fails os.Stat and LookPath in silence, which is the
		// common case on a hand-built or partially-copied install.
		slog.Warn("launcher unavailable: provider starts without PID tracking or signal forwarding",
			"instance", inst.ID, "provider", inst.Provider)
		inst.TryLog("Launcher unavailable: PID tracking and signal forwarding are disabled for this process.")
	}

	// Log the launch command (redacted for secrets, quoted for paths with spaces)
	redacted := security.RedactSensitive(launchLogLine(cmd, args))
	slog.Info("Launching provider process", "instance", inst.ID, "provider", inst.Provider, "command", redacted)
	inst.TryLog("Launching: " + redacted)

	// Create the process — NOT tied to any context.
	// Process lifetime is managed explicitly via Terminate().
	process := host.Command(cmd, args...)
	// Descendants must not keep the output drain open indefinitely.
	process.WaitDelay = outputDrainTimeout
	process.Env = env

	// Pipe stdout/stderr to log writer if provided
	if logWriter != nil {
		process.Stdout = logWriter
		process.Stderr = logWriter
	}

	// Create a stdin pipe to the launcher for graceful shutdown signaling.
	// When zzrouter-node wants to stop this instance, it closes the pipe;
	// the launcher detects EOF and initiates provider shutdown. This works
	// in all sessions including Windows Service (session 0) where console-
	// based signals are undeliverable.
	stdinPipe, pipeErr := process.StdinPipe()
	if pipeErr != nil {
		slog.Warn("failed to create stdin pipe: graceful shutdown will use signals only",
			"error", pipeErr)
	}

	// Platform-specific process attributes.
	setSpawnAttributes(process, inst.IsUsingLauncher())

	if err := process.Start(); err != nil {
		return nil, fmt.Errorf("failed to start process: %w", err)
	}

	pid := process.Process.Pid

	// When using the launcher, store pgid=0 so Terminate signals the launcher
	// PID directly (not a process group).
	pgid := 0
	if !inst.IsUsingLauncher() {
		pgid = getProcessGroupID(process.Process)
	}

	// Assign to a Windows Job Object for automatic orphan cleanup.
	// If zzrouter-node crashes, the kernel closes the handle and kills
	// all processes in the job. No-op on Unix (returns 0, nil).
	jobHandle, jobErr := assignToJobObject(pid)
	if jobErr != nil {
		slog.Warn("failed to assign to job object: orphan cleanup unavailable",
			"pid", pid, "error", jobErr)
	}

	// Store process info + Windows handles on the instance.
	inst.SetProcessInfo(pid, pgid, &instance.LaunchCommand{
		Command:     originalCmd,
		Args:        originalArgs,
		Environment: envVars,
	})
	if stdinPipe != nil {
		inst.SetStdinPipe(stdinPipe)
	}
	if jobHandle != 0 {
		inst.SetJobHandle(jobHandle)
	}

	return &LaunchResult{
		PID:            pid,
		ProcessGroupID: pgid,
		Process:        process.Process,
		command:        process,
	}, nil
}

// findLauncherBinary locates and verifies the zzrouter-launcher binary.
// Checks next to the current executable first, then falls back to PATH.
// If LauncherSHA256 is set (build-time), the binary is rejected if its hash doesn't match.
//
// On Windows the installed binary is zzrouter-launcher.exe; we append the
// extension when the sibling-of-exe lookup runs. LookPath itself handles the
// suffix on its own.
func findLauncherBinary() string {
	name := "zzrouter-launcher"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	// Check next to current executable
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			candidate := filepath.Join(filepath.Dir(real), name)
			if verifyLauncher(candidate) {
				return candidate
			}
		}
	}

	// Fall back to PATH
	if p, err := exec.LookPath(name); err == nil {
		if verifyLauncher(p) {
			return p
		}
	}

	return ""
}

// verifyLauncher checks the binary exists and (when LauncherSHA256 is
// set) hashes to the expected value. See launcher_integrity_{dev,prod}.go
// for the empty-hash policy, which differs between dev and release builds.
func verifyLauncher(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}

	if LauncherSHA256 == "" {
		if !devLauncherBypass {
			slog.Error("launcher integrity check FAILED: no hash embedded in release build", "path", path)
			return false
		}
		slog.Warn("launcher integrity check SKIPPED (dev build)", "path", path)
		return true
	}

	f, err := os.Open(path)
	if err != nil {
		slog.Warn("launcher integrity check failed: cannot open", "path", path, "error", err)
		return false
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		slog.Warn("launcher integrity check failed: cannot read", "path", path, "error", err)
		return false
	}

	actual := hex.EncodeToString(h.Sum(nil))
	if actual != LauncherSHA256 {
		slog.Error("launcher integrity check FAILED: hash mismatch: binary may be tampered",
			"path", path, "expected", LauncherSHA256, "actual", actual)
		return false
	}

	return true
}
