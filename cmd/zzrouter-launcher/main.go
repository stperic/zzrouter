// zzrouter-launcher wraps provider processes launched by zzrouter-node.
// It provides clear ownership identification and reliable signal forwarding.
//
// Usage:
//
//	zzrouter-launcher --instance-id=ID --provider=TYPE [--pid-dir=DIR] -- CMD [ARGS...]
//
// The launcher:
// 1. Writes a PID file ({pid-dir}/{instance-id}.pid)
// 2. Launches the child command in a new process group
// 3. Forwards SIGTERM/SIGINT to the child process group
// 4. Waits for graceful shutdown (5s), then escalates to SIGKILL
// 5. Removes PID file on exit
// 6. Exits with the child's exit code
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/utils"
)

const gracefulTimeout = 5 * time.Second

type pidInfo struct {
	PID        int    `json:"pid"`
	ChildPID   int    `json:"child_pid"`
	Provider   string `json:"provider"`
	InstanceID string `json:"instance_id"`
	StartedAt  string `json:"started_at"`
}

func main() {
	instanceID := flag.String("instance-id", "", "unique instance identifier (required)")
	provider := flag.String("provider", "", "provider type, e.g. vllm, llama-cpp, mlx (required)")
	pidDir := flag.String("pid-dir", filepath.Join(os.TempDir(), "zzrouter-pids"), "directory for PID files")
	flag.Parse()

	if *instanceID == "" || *provider == "" {
		fmt.Fprintln(os.Stderr, "error: --instance-id and --provider are required")
		flag.Usage()
		os.Exit(1)
	}

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: child command required after flags (use -- CMD [ARGS...])")
		os.Exit(1)
	}

	// Ensure PID directory exists.
	if err := os.MkdirAll(*pidDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create pid dir %s: %v\n", *pidDir, err)
		os.Exit(1)
	}

	// Set up the child command in its own process group.
	cmd := host.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	setupProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to start child: %v\n", err)
		os.Exit(1)
	}
	assignChildToJob(cmd)

	// Write PID file atomically.
	pidFile := filepath.Join(*pidDir, *instanceID+".pid")
	if err := writePIDFile(pidFile, pidInfo{
		PID:        os.Getpid(),
		ChildPID:   cmd.Process.Pid,
		Provider:   *provider,
		InstanceID: *instanceID,
		StartedAt:  utils.NowUTC().Format(time.RFC3339),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write pid file: %v\n", err)
	}
	defer os.Remove(pidFile)

	// Channel to receive child exit result.
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	// Forward signals to the child process group.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, shutdownSignals()...)

	// Monitor stdin for EOF. When zzrouter-node wants to gracefully stop
	// this instance, it closes the stdin pipe. This is the primary Windows
	// shutdown mechanism (works in any session including session 0 / Windows
	// Service). On Unix, stdin is typically /dev/null (EOF immediately) or
	// a terminal; we only enable this if stdin is a pipe.
	stdinClosed := make(chan struct{}, 1)
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeNamedPipe != 0) {
		go func() {
			buf := make([]byte, 1)
			// Blocks until pipe closes or EOF. Real read errors (rare given the
			// pipe-mode guard above) are surfaced so a bad fd doesn't silently
			// trigger graceful shutdown.
			if _, err := os.Stdin.Read(buf); err != nil && !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "warning: stdin read error (treating as shutdown): %v\n", err)
			}
			close(stdinClosed)
		}()
	}

	// Wait for any shutdown trigger: signal, stdin-EOF, or child exit.
	select {
	case sig := <-sigCh:
		forwardSignalToChild(cmd, sig)

		select {
		case <-waitCh:
		case <-time.After(gracefulTimeout):
			forceKillChild(cmd)
			<-waitCh
		}

	case <-stdinClosed:
		// Parent closed stdin pipe — initiate graceful shutdown.
		// Mirror the signal path: try graceful first, escalate to force kill.
		// This gives GPU processes (vLLM, llama.cpp) a chance to flush VRAM
		// and clean up CUDA contexts before being killed.
		forwardSignalToChild(cmd, os.Interrupt)

		select {
		case <-waitCh:
		case <-time.After(gracefulTimeout):
			forceKillChild(cmd)
			<-waitCh
		}

	case <-waitCh:
		// Child exited on its own.
	}

	os.Exit(exitCode(cmd))
}

// writePIDFile writes the PID info atomically using temp file + rename.
func writePIDFile(path string, info pidInfo) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// exitCode extracts the exit code from a finished command.
func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 1
	}
	return cmd.ProcessState.ExitCode()
}
