// zzrouter-launcher wraps provider processes launched by zzrouter-node.
// It provides clear ownership identification and reliable signal forwarding.
//
// Usage:
//
//	zzrouter-launcher --instance-id=ID --provider=TYPE [--pid-dir=DIR] [--node-owned] -- CMD [ARGS...]
//
// The launcher:
// 1. Writes a PID file ({pid-dir}/{instance-id}.pid)
// 2. With --node-owned on Unix, keeps the child in the node's group and leaves signaling to the node
// 3. Otherwise isolates the child and forwards shutdown signals
// 4. On stdin EOF, forwards shutdown; escalates forwarded shutdowns after 5s
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
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
)

const gracefulTimeout = 5 * time.Second

type pidInfo struct {
	PID        int    `json:"pid"`
	StartToken int64  `json:"start_token,omitempty"`
	ChildPID   int    `json:"child_pid"`
	Provider   string `json:"provider"`
	InstanceID string `json:"instance_id"`
	StartedAt  string `json:"started_at"`
}

func main() {
	os.Exit(run((*exec.Cmd).Start))
}

func run(startChild func(*exec.Cmd) error) int {
	instanceID := flag.String("instance-id", "", "unique instance identifier (required)")
	provider := flag.String("provider", "", "provider type, e.g. vllm, llama-cpp, mlx (required)")
	pidDir := flag.String("pid-dir", filepath.Join(os.TempDir(), "zzrouter-pids"), "directory for PID files")
	nodeOwnedFlag := flag.Bool("node-owned", false, "let the node own the child's process group on Unix")
	flag.Parse()

	if *instanceID == "" || *provider == "" {
		fmt.Fprintln(os.Stderr, "error: --instance-id and --provider are required")
		flag.Usage()
		return 1
	}

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: child command required after flags (use -- CMD [ARGS...])")
		return 1
	}

	// Ensure PID directory exists.
	if err := os.MkdirAll(*pidDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create pid dir %s: %v\n", *pidDir, err)
		return 1
	}

	// Keep a node-owned group; isolate a standalone child.
	cmd := host.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	nodeOwned := setupProcessGroup(cmd, *nodeOwnedFlag)

	// A node-owned launcher must die on a stop before its child starts.
	sigCh := make(chan os.Signal, 2)
	if !nodeOwned {
		signal.Notify(sigCh, shutdownSignals()...)
	}
	defer signal.Stop(sigCh)

	if err := startChild(cmd); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to start child: %v\n", err)
		return 1
	}
	if nodeOwned {
		signal.Notify(sigCh, shutdownSignals()...)
	}
	assignChildToJob(cmd)

	// Write PID file atomically.
	pidFile := filepath.Join(*pidDir, *instanceID+".pid")
	startToken, _ := process.StartToken(os.Getpid())
	if err := writePIDFile(pidFile, pidInfo{
		PID:        os.Getpid(),
		StartToken: startToken,
		ChildPID:   cmd.Process.Pid,
		Provider:   *provider,
		InstanceID: *instanceID,
		StartedAt:  utils.NowUTC().Format(time.RFC3339),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write pid file: %v\n", err)
	} else {
		defer os.Remove(pidFile)
	}

	// Channel to receive child exit result.
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	// Stdin EOF requests shutdown on Windows and means the node died on Unix.
	// Only pipes are monitored: standalone launchers can use /dev/null or a terminal.
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
	var shutdown os.Signal
	select {
	case shutdown = <-sigCh:
		if nodeOwned {
			// The node owns escalation unless it dies before the child exits.
			select {
			case <-waitCh:
				return exitCode(cmd)
			case <-stdinClosed:
				shutdown = os.Interrupt
			}
		}
	case <-stdinClosed:
		shutdown = os.Interrupt
	case <-waitCh:
		return exitCode(cmd)
	}
	forwardSignalToChild(cmd, shutdown)
	select {
	case <-waitCh:
	case <-time.After(gracefulTimeout):
		forceKillChild(cmd)
		<-waitCh
	}

	return exitCode(cmd)
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
